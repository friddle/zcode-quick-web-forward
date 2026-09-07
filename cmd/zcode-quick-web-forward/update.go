// `update` command: refresh the ZCode engine runtime and this binary itself
// from the same GitHub "releases/latest/download" URLs install.sh uses, then
// hand off to a detached shell script that sleeps, kills the old daemon
// (possibly this very process), swaps the binary and restarts everything.
//
//	usage: zcode-quick-web-forward update [--check] [--no-engine] [--no-self]
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"syscall"
	"time"

	"github.com/friddle/zcode-quick-web-forward/internal/runtime"
)

const (
	repoBase = "https://github.com/friddle/zcode-quick-web-forward"
	apiBase  = "https://api.github.com/repos/friddle/zcode-quick-web-forward"
)

// httpClientTimeout bounds each update download attempt; the ghDownload retry
// loop handles transient failures.
var httpClientTimeout = http.Client{Timeout: 10 * time.Minute}

// ghMirror applies the GH_PROXY prefix (same convention as install.sh) so
// GitHub downloads work behind the GFW.
func ghMirror(u string) string {
	p := strings.TrimSpace(os.Getenv("GH_PROXY"))
	if p == "" {
		return u
	}
	for _, pre := range []string{"https://gh-proxy.com/", "https://ghp.ci/", "https://ghproxy.net/"} {
		u = strings.TrimPrefix(u, pre)
	}
	return strings.TrimRight(p, "/") + "/" + u
}

func ghDownload(url, dst string) error {
	for _, u := range []string{ghMirror(url), url} {
		if err := httpCopy(u, dst); err == nil {
			return nil
		} else {
			fmt.Printf("zcode: download %s failed: %v\n", u, err)
		}
	}
	return fmt.Errorf("all sources failed")
}

func httpCopy(url, dst string) error {
	client := httpClientTimeout
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

func sha256File(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// selfAsset returns the release asset name for this platform.
func selfAsset() string {
	ext := ""
	if goruntime.GOOS == "windows" {
		ext = ".exe"
	}
	return fmt.Sprintf("zcode-quick-web-forward-%s-%s%s", goruntime.GOOS, goruntime.GOARCH, ext)
}

// isNewerVersion compares dotted numeric version strings (v-prefix tolerated).
func isNewerVersion(candidate, current string) bool {
	part := func(s string, i int) int {
		fields := strings.Split(strings.TrimPrefix(s, "v"), ".")
		if i >= len(fields) {
			return 0
		}
		n := 0
		fmt.Sscanf(fields[i], "%d", &n)
		return n
	}
	for i := 0; i < 6; i++ {
		if a, b := part(candidate, i), part(current, i); a != b {
			return a > b
		}
	}
	return false
}

// latestSelfVersion returns the newest release tag ("" when unknown).
func latestSelfVersion() string {
	client := &httpClientTimeout
	resp, err := client.Get(ghMirror(apiBase + "/releases/latest"))
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var rel struct {
		TagName string `json:"tag_name"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel) != nil {
		return ""
	}
	return strings.TrimPrefix(rel.TagName, "v")
}

func currentBinPath() string {
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			return resolved
		}
		return exe
	}
	return filepath.Join(os.Getenv("HOME"), ".local", "bin", "zcode-quick-web-forward")
}

func doUpdate(args []string) {
	check := hasFlag(args, "--check")
	engineSkip := hasFlag(args, "--no-engine")
	selfSkip := hasFlag(args, "--no-self")

	fmt.Printf("zcode: current version %s (%s/%s)\n", version, goruntime.GOOS, goruntime.GOARCH)

	staged := ""
	// ---- 1) engine runtime -------------------------------------------------
	if !engineSkip {
		f := runtime.NewFinder()
		if v, err := f.DownloadLatest(); err != nil {
			fmt.Printf("zcode: engine update failed: %v (继续更新自身)\n", err)
		} else {
			fmt.Printf("zcode: engine runtime updated -> %s\n", v)
			pruneOldRuntimes(f.CacheRoot, v)
		}
	}

	// ---- 2) this binary ----------------------------------------------------
	latest := latestSelfVersion()
	if latest != "" {
		fmt.Printf("zcode: latest release %s (local %s)\n", latest, version)
	}
	force := hasFlag(args, "--force")
	if !selfSkip && latest != "" && !isNewerVersion(latest, version) && !force {
		fmt.Println("zcode: 本地版本不落后于 latest release,跳过自身更新 (--force 可强制)")
		selfSkip = true
	}
	if !selfSkip {
		cache, err := os.UserCacheDir()
		if err == nil {
			staging := filepath.Join(cache, "zcode-quick-web-forward", "update")
			_ = os.MkdirAll(staging, 0o755)
			staged = filepath.Join(staging, "zcode-quick-web-forward.new")
			url := repoBase + "/releases/latest/download/" + selfAsset()
			fmt.Printf("zcode: downloading %s\n", selfAsset())
			if err := ghDownload(url, staged); err != nil {
				fmt.Printf("zcode: self update download failed (%v); 保留当前版本\n", err)
				staged = ""
			} else if sha256File(staged) == sha256File(currentBinPath()) {
				fmt.Println("zcode: self already up to date")
				os.Remove(staged)
				staged = ""
			}
		}
	}

	if check {
		fmt.Println("zcode: --check 指定,不执行更新")
		return
	}

	// ---- 3) detached swap+restart script ----------------------------------
	workdir, _ := os.Getwd()
	bin := currentBinPath()
	logPath := "r.log"
	script := fmt.Sprintf(`#!/bin/sh
sleep 2
if [ -f "r.log.pid" ]; then kill "$(cat r.log.pid)" 2>/dev/null; fi
pkill -f 'zcode-quick-web-forward remote' 2>/dev/null
sleep 1
if [ -s "%s" ]; then mv -f "%s" "%s"; chmod 0755 "%s"; fi
cd "%s" || exit 1
nohup "%s" remote --foreground >> %s 2>&1 &
`, staged, staged, bin, bin, workdir, bin, logPath)
	scriptPath := filepath.Join(os.TempDir(), "zqwf-do-update.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		fatal("write update script: %v", err)
	}
	cmd := exec.Command("sh", scriptPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		fatal("launch update script: %v", err)
	}
	_ = cmd.Process.Release()
	fmt.Printf("zcode: 更新脚本已脱离执行 (%s) — 旧进程 2 秒后停止,自动安装并重启\n", scriptPath)
	time.Sleep(300 * time.Millisecond)
}

// pruneOldRuntimes removes cached runtime versions other than keep so the next
// start resolves the freshly downloaded one (findCached returns the first
// match alphabetically and could otherwise stick to an old version).
func pruneOldRuntimes(cacheRoot, keep string) {
	entries, err := os.ReadDir(cacheRoot)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == keep || e.Name() == "update" || e.Name() == "extract" {
			continue
		}
		if _, err := os.Stat(filepath.Join(cacheRoot, e.Name(), runtime.RuntimeName)); err != nil {
			continue // not a runtime version dir (nodejs/, etc.)
		}
		if err := os.RemoveAll(filepath.Join(cacheRoot, e.Name())); err == nil {
			fmt.Printf("zcode: removed old runtime %s\n", e.Name())
		}
	}
}
