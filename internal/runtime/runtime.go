// Package runtime downloads and extracts the ZCode app-server runtime (the
// bundled glm/zcode.cjs and its dependencies) from a ZCode desktop release
// artifact, or reuses an already-installed desktop app. The runtime directory
// that contains zcode.cjs is what the appserver package launches.
package runtime

import (
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"time"

	"github.com/friddle/zcode-quick-web-forward/internal/manifest"
)

// Node reports the node binary used to run zcode.cjs (override with ZCODE_NODE).
func Node() string {
	if v := os.Getenv("ZCODE_NODE"); v != "" {
		return v
	}
	return "node"
}

// RuntimeName is the top-level runtime directory inside an extracted app.
const RuntimeName = "runtime"

// Finder locates a usable runtime directory.
type Finder struct {
	CacheRoot string
	HTTP      *http.Client
	Platform  string
	Arch      string
}

// NewFinder builds a Finder for the current platform/arch.
func NewFinder() *Finder {
	p, a := manifest.Platform(goruntime.GOOS), manifest.Arch(goruntime.GOARCH)
	cache, _ := os.UserCacheDir()
	return &Finder{
		CacheRoot: filepath.Join(cache, "zcode-quick-web-forward"),
		HTTP:      &http.Client{Timeout: 0},
		Platform:  p,
		Arch:      a,
	}
}

// Resolve returns a ready-to-run runtime directory.
//
// Resolution order:
//
//  1. an explicit --runtime-path / ZCODE_RUNTIME_PATH pointing at the glm dir
//  2. an already-installed ZCode desktop app on disk
//  3. download the latest release artifact and extract it to cache
//
// version is a label used only for reporting.
func (f *Finder) Resolve(explicitPath string) (dir string, version string, err error) {
	if explicitPath != "" {
		if err := checkRuntime(explicitPath); err != nil {
			return "", "", fmt.Errorf("explicit runtime path: %w", err)
		}
		return explicitPath, "from-path", nil
	}
	if dir, v := f.findInstalled(); dir != "" {
		return dir, v, nil
	}
	if dir, ok := f.findCached(); ok {
		return dir, "cached", nil
	}
	v, err := f.DownloadLatest()
	if err != nil {
		return "", "", err
	}
	if dir, ok := f.findCached(); ok {
		return dir, v, nil
	}
	return "", "", errors.New("runtime downloaded but not found in cache")
}

// DownloadLatest fetches the newest release artifact for this platform/arch,
// extracts the glm runtime, and stores it in the cache. Returns the version.
func (f *Finder) DownloadLatest() (string, error) {
	client := manifest.New(f.Platform, f.Arch)
	rel, err := client.Latest()
	if err != nil {
		return "", fmt.Errorf("resolve latest release: %w", err)
	}
	fmt.Printf("zcode: downloading ZCode %s (%s) ...\n", rel.Version, rel.URL)
	fname := filepath.Base(rel.URL)
	if i := strings.Index(fname, "?"); i >= 0 {
		fname = fname[:i]
	}
	dst := filepath.Join(f.CacheRoot, fname)
	if err := f.download(rel.URL, dst, rel.Artifact.Sha512); err != nil {
		return "", fmt.Errorf("download runtime: %w", err)
	}
	extractDir := filepath.Join(f.CacheRoot, "extract")
	_ = os.RemoveAll(extractDir)
	_ = os.MkdirAll(extractDir, 0o755)
	if err := extractArtifact(dst, extractDir); err != nil {
		return "", fmt.Errorf("extract runtime: %w", err)
	}
	glmDir := findGlm(extractDir)
	if glmDir == "" {
		return "", errors.New("no glm/zcode.cjs found in extracted artifact")
	}
	versionDir := filepath.Join(f.CacheRoot, rel.Version)
	_ = os.RemoveAll(versionDir)
	runtimeDir := filepath.Join(versionDir, RuntimeName)
	if err := moveDir(glmDir, runtimeDir); err != nil {
		return "", fmt.Errorf("normalize runtime: %w", err)
	}
	return rel.Version, nil
}

// verifyChecksum reports whether a raw 64-byte sha512 matches the manifest
// value, which is base64-encoded (hex is accepted too).
func verifyChecksum(sum []byte, want string) bool {
	if want == "" {
		return true
	}
	if len(sum) == sha512.Size {
		if hex.EncodeToString(sum) == strings.ToLower(strings.TrimSpace(want)) {
			return true
		}
		if strings.EqualFold(base64.StdEncoding.EncodeToString(sum), strings.TrimSpace(want)) {
			return true
		}
	}
	return false
}

func (f *Finder) download(url, dst, shaSum string) error {
	if isUpToDate(dst, shaSum) {
		fmt.Printf("zcode: use cached artifact %s\n", filepath.Base(dst))
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	client := f.HTTP
	if client == nil {
		client = &http.Client{}
	}
	part := dst + ".part"
	const maxAttempts = 30
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if lastErr != nil {
			fmt.Printf("zcode: download interrupted (%v), resuming…\n", lastErr)
			time.Sleep(2 * time.Second)
		}
		have := int64(0)
		if fi, err := os.Stat(part); err == nil {
			have = fi.Size()
		}
		lastErr = f.fetchRange(client, url, part, have)
		if lastErr == nil {
			break
		}
	}
	if lastErr != nil {
		os.Remove(part)
		return fmt.Errorf("download %s: %w", filepath.Base(dst), lastErr)
	}
	if shaSum != "" {
		h := sha512.New()
		fh, err := os.Open(part)
		if err != nil {
			return err
		}
		_, cErr := io.Copy(h, fh)
		fh.Close()
		if cErr != nil {
			return cErr
		}
		if !verifyChecksum(h.Sum(nil), shaSum) {
			os.Remove(part)
			return fmt.Errorf("checksum mismatch for %s", filepath.Base(dst))
		}
	}
	return os.Rename(part, dst)
}

// fetchRange appends bytes from url to part, resuming at offset have. A
// connection that stalls (no data for stallTimeout) or drops mid-transfer is
// returned as an error so the caller can resume from what is already on disk.
func (f *Finder) fetchRange(client *http.Client, url, part string, have int64) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const stallTimeout = 45 * time.Second
	stall := time.AfterFunc(stallTimeout, cancel)
	defer stall.Stop()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if have > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", have))
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusPartialContent:
	case http.StatusOK:
		have = 0 // server ignored the range; start the file over
	default:
		return fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	flag := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if have == 0 {
		flag |= os.O_TRUNC
	}
	out, err := os.OpenFile(part, flag, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	buf := make([]byte, 64*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				return werr
			}
			if !stall.Reset(stallTimeout) {
				stall = time.AfterFunc(stallTimeout, cancel)
			}
		}
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			return rerr
		}
	}
}

func (f *Finder) findCached() (string, bool) {
	matches, _ := filepath.Glob(filepath.Join(f.CacheRoot, "*", RuntimeName))
	for _, m := range matches {
		if checkRuntime(m) == nil {
			return m, true
		}
	}
	return "", false
}

func (f *Finder) findInstalled() (string, string) {
	var candidates []string
	switch f.Platform {
	case "linux":
		candidates = []string{"/opt/ZCode/resources/glm"}
	case "darwin":
		candidates = []string{"/Applications/ZCode.app/Contents/Resources/glm"}
	}
	for _, c := range candidates {
		if checkRuntime(c) == nil {
			return c, "installed"
		}
	}
	return "", ""
}

func encode512(sum []byte) string {
	if len(sum) != sha512.Size {
		return ""
	}
	return hex.EncodeToString(sum)
}

func isUpToDate(dst, shaSum string) bool {
	b, err := os.ReadFile(dst)
	if err != nil {
		return false
	}
	if shaSum == "" {
		return true
	}
	h := sha512.Sum512(b)
	if encode512(h[:]) == strings.ToLower(shaSum) {
		return true
	}
	return strings.EqualFold(base64.StdEncoding.EncodeToString(h[:]), shaSum)
}

func checkRuntime(dir string) error {
	s := filepath.Join(dir, "zcode.cjs")
	if fi, err := os.Stat(s); err == nil && !fi.IsDir() {
		return nil
	}
	return fmt.Errorf("%s: no zcode.cjs (missing runtime)", dir)
}

func findGlm(root string) string {
	var found string
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if info.Name() == "zcode.cjs" {
			found = filepath.Dir(path)
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

func extractArtifact(src, outDir string) error {
	if strings.HasSuffix(src, ".deb") {
		return extractDeb(src, outDir)
	}
	return extractGeneric(src, outDir)
}

func extractDeb(src, outDir string) error {
	work, err := os.MkdirTemp("", "zcode-deb")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	if err := sh(fmt.Sprintf("ar x %s", quote(src)), work); err != nil {
		return fmt.Errorf("ar: %w", err)
	}
	entries, _ := os.ReadDir(work)
	var dataTarball string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "data.tar") {
			dataTarball = filepath.Join(work, e.Name())
			break
		}
	}
	if dataTarball == "" {
		return errors.New("no data.tar archive inside .deb")
	}
	if err := sh(fmt.Sprintf("tar -xf %s -C %s", quote(dataTarball), quote(outDir)), ""); err != nil {
		return fmt.Errorf("tar: %w", err)
	}
	return nil
}

func extractGeneric(src, outDir string) error {
	if err := sh(fmt.Sprintf("tar -xf %s -C %s", quote(src), quote(outDir)), ""); err == nil {
		return nil
	}
	return fmt.Errorf("extract %s: no applicable extractor", src)
}

func moveDir(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.Rename(src, dst)
}

func sh(cmd, dir string) error {
	c := exec.Command("sh", "-c", cmd)
	if dir != "" {
		c.Dir = dir
	}
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}

func quote(s string) string {
	if strings.Contains(s, " ") {
		return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
	}
	return s
}
