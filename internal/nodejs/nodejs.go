// Package nodejs ensures a Node.js >= 22.5 binary is available for the ZCode
// runtime (the node:sqlite built-in landed in v22.5). The system node — or
// ZCODE_NODE — is used when new enough; otherwise an official Node.js release
// is downloaded into the user cache: through the Aliyun "nodejs-release"
// mirror from China and nodejs.org elsewhere.
package nodejs

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MinMajor/MinMinor is the oldest Node.js release the ZCode runtime supports.
const (
	MinMajor = 22
	MinMinor = 5
)

const (
	// pinnedVersion is the fallback release when the mirror index cannot be
	// read (an LTS that definitely ships node:sqlite).
	pinnedVersion = "v22.14.0"

	mirrorChina  = "https://mirrors.aliyun.com/nodejs-release"
	mirrorGlobal = "https://nodejs.org/dist"

	probeGlobal = "https://www.google.com/generate_204"
	probeChina  = "https://www.baidu.com"
)

// Ensure returns a node binary able to run the ZCode runtime: ZCODE_NODE or
// the system node when new enough, otherwise a managed download selected by
// region ("" = auto-detect via DetectRegion).
func Ensure(region string) (string, error) {
	if v := os.Getenv("ZCODE_NODE"); v != "" {
		return v, nil
	}
	if p, err := exec.LookPath("node"); err == nil && Meets(p) {
		return p, nil
	}
	return Download(region)
}

// Meets reports whether the node binary at path is >= MinMajor.MinMinor.
func Meets(path string) bool {
	major, minor, err := versionOf(path)
	if err != nil {
		return false
	}
	return versionAtLeast(major, minor)
}

// Download fetches a managed Node.js release into the user cache and returns
// the path of the node binary inside it.
func Download(region string) (string, error) {
	r := Normalize(region)
	base := mirrorGlobal
	if r == "china" {
		base = mirrorChina
	}
	root, err := cacheRoot()
	if err != nil {
		return "", err
	}
	if p := managed(root); p != "" {
		return p, nil
	}
	fmt.Printf("zcode: no Node.js >= %d.%d on PATH; downloading a managed release (region=%s)…\n", MinMajor, MinMinor, r)

	vers := []string{pinnedVersion}
	if v, err := latestLTS(base); err == nil && v != "" && v != pinnedVersion {
		vers = []string{v, pinnedVersion}
	}
	var lastErr error
	for _, v := range vers {
		p, err := downloadVersion(base, root, v)
		if err == nil {
			return p, nil
		}
		lastErr = err
		fmt.Printf("zcode: node %s failed: %v\n", v, err)
	}
	return "", fmt.Errorf("download managed Node.js: %w", lastErr)
}

// Normalize resolves region to "china" or "global": explicit values win, an
// empty value probes the network (Google first, then Baidu).
func Normalize(region string) string {
	switch strings.ToLower(strings.TrimSpace(region)) {
	case "china", "cn":
		return "china"
	case "global", "intl", "international":
		return "global"
	}
	return DetectRegion()
}

var (
	regionOnce  sync.Once
	regionValue string
)

// DetectRegion probes Google, then Baidu: Google reachable -> global; only
// Baidu -> china; neither -> global. The result is cached.
func DetectRegion() string {
	regionOnce.Do(func() { regionValue = probeRegion() })
	return regionValue
}

func probeRegion() string {
	if reachable(probeGlobal) {
		return "global"
	}
	if reachable(probeChina) {
		return "china"
	}
	return "global"
}

func reachable(u string) bool {
	c := &http.Client{Timeout: 4 * time.Second}
	resp, err := c.Get(u)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode < 500
}

// managed returns a previously downloaded managed node binary, if any.
func managed(root string) string {
	hits, _ := filepath.Glob(filepath.Join(root, "node-v*", "bin", "node"))
	if goruntime.GOOS == "windows" {
		win, _ := filepath.Glob(filepath.Join(root, "node-v*-win-*", "node.exe"))
		hits = append(hits, win...)
	}
	for _, h := range hits {
		if Meets(h) {
			return h
		}
	}
	return ""
}

func downloadVersion(base, root, ver string) (string, error) {
	name := fmt.Sprintf("node-%s-%s-%s", ver, distOS(), distArch())
	archive := name + distExt()
	dst := filepath.Join(root, archive)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	if fi, err := os.Stat(dst); err != nil || fi.Size() == 0 {
		if err := fetch(base+"/"+ver+"/"+archive, dst); err != nil {
			return "", err
		}
	} else {
		fmt.Printf("zcode: use cached %s\n", archive)
	}
	fmt.Printf("zcode: extracting %s …\n", archive)
	if out, err := exec.Command("tar", "-xf", dst, "-C", root).CombinedOutput(); err != nil {
		return "", fmt.Errorf("tar: %v: %s", err, strings.TrimSpace(string(out)))
	}
	np := filepath.Join(root, name, "bin", "node")
	if distOS() == "win" {
		np = filepath.Join(root, name, "node.exe")
	}
	if _, err := os.Stat(np); err != nil {
		return "", err
	}
	if goruntime.GOOS != "windows" {
		_ = os.Chmod(np, 0o755)
	}
	if !Meets(np) {
		return "", fmt.Errorf("downloaded node failed to run: %s", np)
	}
	return np, nil
}

// fetch downloads url into dst with up to 3 resumable attempts (Range header).
func fetch(url, dst string) error {
	fmt.Printf("zcode: downloading %s\n", url)
	part := dst + ".part"
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(2 * time.Second)
		}
		lastErr = fetchOnce(url, part)
		if lastErr == nil {
			break
		}
	}
	if lastErr != nil {
		_ = os.Remove(part)
		return lastErr
	}
	return os.Rename(part, dst)
}

func fetchOnce(url, part string) error {
	var have int64
	if fi, err := os.Stat(part); err == nil {
		have = fi.Size()
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if have > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", have))
	}
	resp, err := (&http.Client{Timeout: 0}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusPartialContent:
	case http.StatusOK:
		have = 0 // server ignored the range; start over
	default:
		return fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	flag := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if have == 0 {
		flag |= os.O_TRUNC
	}
	f, err := os.OpenFile(part, flag, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

// latestLTS returns the newest LTS release (>= the minimum) from the mirror's
// index.json, e.g. "v22.14.0".
func latestLTS(base string) (string, error) {
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Get(base + "/index.json")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("index.json: status %d", resp.StatusCode)
	}
	var entries []struct {
		Version string `json:"version"`
		LTS     any    `json:"lts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		return "", err
	}
	for _, e := range entries {
		name, ok := e.LTS.(string)
		if !ok || name == "" {
			continue
		}
		major, minor, err := parseVersion(e.Version)
		if err != nil || !versionAtLeast(major, minor) {
			continue
		}
		return e.Version, nil
	}
	return "", fmt.Errorf("no LTS >= %d.%d in index.json", MinMajor, MinMinor)
}

func versionAtLeast(major, minor int) bool {
	return major > MinMajor || (major == MinMajor && minor >= MinMinor)
}

func versionOf(path string) (int, int, error) {
	out, err := exec.Command(path, "-v").Output()
	if err != nil {
		return 0, 0, err
	}
	return parseVersion(strings.TrimSpace(string(out)))
}

func parseVersion(s string) (int, int, error) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	parts := strings.SplitN(s, ".", 3)
	if len(parts) < 2 {
		return 0, 0, fmt.Errorf("unrecognized node version %q", s)
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, err
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, err
	}
	return major, minor, nil
}

func cacheRoot() (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cache, "zcode-quick-web-forward", "nodejs"), nil
}

func distOS() string {
	switch goruntime.GOOS {
	case "windows":
		return "win"
	default:
		return goruntime.GOOS // linux, darwin
	}
}

func distArch() string {
	switch goruntime.GOARCH {
	case "amd64":
		return "x64"
	case "386":
		return "x86"
	case "arm":
		return "armv7l"
	default:
		return goruntime.GOARCH // arm64, …
	}
}

func distExt() string {
	if goruntime.GOOS == "windows" {
		return ".zip"
	}
	return ".tar.gz"
}
