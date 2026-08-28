// Package manifest resolves the latest ZCode desktop release (which bundles
// the glm/zcode.cjs app-server runtime) from the ZCode update-yml manifests on
// the CDN, with a fallback to the web manifest service. It stays
// dependency-free: it does a small, tolerant YAML/JSON parse by hand.
package manifest

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const (
	// CDNRoot is the CDN root for electron update manifests / artifacts.
	CDNRoot = "https://cdn-zcode.z.ai/zcode/electron/releases"
	// UpdateService is the web manifest service (secondary).
	UpdateService = "https://zcode.z.ai"
	// ManifestPath is the service manifest path.
	ManifestPath = "/api/v1/releases/electron/manifest"
	// StableChannel selects the stable release channel on the service.
	StableChannel = "1"
)

// Artifact is a downloadable release artifact.
type Artifact struct {
	URL     string
	Sha512  string
	Version string
}

// Release is a resolved latest release for a platform/arch pair.
type Release struct {
	Version  string
	Artifact Artifact
	URL      string
}

// Platform maps a goos to the ZCode electron platform token.
func Platform(goos string) string {
	switch goos {
	case "darwin":
		return "darwin"
	case "windows", "win32":
		return "win32"
	default:
		return "linux"
	}
}

// Arch maps a goarch to the ZCode electron manifest arch token.
func Arch(goarch string) string {
	switch goarch {
	case "arm64", "aarch64":
		return "arm64"
	default:
		return "x64"
	}
}

// Client fetches and parses release manifests.
type Client struct {
	HTTP     *http.Client
	Platform string
	Arch     string
}

// New creates a manifest client for the given platform/arch.
func New(platform, arch string) *Client {
	return &Client{HTTP: &http.Client{Timeout: 30 * time.Second}, Platform: platform, Arch: arch}
}

// Latest resolves the newest release for the client's platform/arch.
//
// Primary: the CDN "latest-*.yml" update manifest (matches what the official
// app updater uses). Secondary: the web manifest service with query params.
func (c *Client) Latest() (*Release, error) {
	// 1) CDN update-yml manifest
	if rel, err := c.fromUpdateYML(); err == nil && rel != nil {
		return rel, nil
	}
	// 2) service manifest
	if rel, err := c.fromService(); err == nil && rel != nil {
		return rel, nil
	}
	// 3) direct CDN URL fallback for a fixed version (unused unless needed)
	return nil, errors.New("unable to resolve latest ZCode release")
}

func (c *Client) updateYMLURL() string {
	var p string
	switch c.Platform {
	case "darwin":
		p = "mac"
	case "win32":
		p = "win"
	default:
		p = "linux"
	}
	file := map[string]string{"mac": "latest-mac.yml", "win": "latest.yml", "linux": "latest-linux.yml"}[p]
	return fmt.Sprintf("%s/update/%s/%s/%s", CDNRoot, p, c.Arch, file)
}

func (c *Client) fromUpdateYML() (*Release, error) {
	req, err := http.NewRequest(http.MethodGet, c.updateYMLURL(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/x-yaml,text/yaml,text/plain,*/*;q=0.9")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("update-yml: status %d", resp.StatusCode)
	}
	return c.parseYML(io.LimitReader(resp.Body, 4<<20))
}

func (c *Client) fromService() (*Release, error) {
	u := UpdateService + ManifestPath + "?platform=" + c.platformArch() + "&channel=" + StableChannel
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/x-yaml,text/yaml,application/json,text/plain,*/*;q=0.9")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("service manifest: status %d", resp.StatusCode)
	}
	buf, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if rel := parseJSONManifest(buf, c.ext()); rel != nil {
		return rel, nil
	}
	return parseYMLBytes(buf, c.ext())
}

func (c *Client) platformArch() string {
	rp := map[string]string{"darwin": "darwin", "win32": "windows", "linux": "linux"}[c.Platform]
	ra := map[string]string{"arm64": "aarch64", "x64": "x86_64"}[c.Arch]
	return rp + "-" + ra
}

func (c *Client) ext() string {
	switch c.Platform {
	case "darwin":
		return ".zip"
	case "win32":
		return ".exe"
	default:
		return ".deb"
	}
}

var (
	versionRe = regexp.MustCompile(`(?m)^\s*version\s*:\s*["']?([0-9][0-9a-zA-Z.\-]*)["']?`)
	urlRe     = regexp.MustCompile(`^\s*(?:-\s+)?url\s*:\s*(.+)`)
	shaRe     = regexp.MustCompile(`^\s*sha512\s*:\s*(.+)`)
)

// parseYML reads the YAML-ish update manifest and returns the first artifact
// matching our platform extension, along with the version.
func (c *Client) parseYML(r io.Reader) (*Release, error) {
	buf, err := io.ReadAll(io.LimitReader(r, 4<<20))
	if err != nil {
		return nil, err
	}
	return parseYMLBytes(buf, c.ext())
}

func parseYMLBytes(data []byte, ext string) (*Release, error) {
	version := ""
	if m := versionRe.FindSubmatch(data); m != nil {
		version = strings.TrimSpace(string(m[1]))
	}
	var artifacts []Artifact
	var cur *Artifact
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		if m := urlRe.FindStringSubmatch(trimmed); m != nil {
			if cur != nil {
				artifacts = append(artifacts, *cur)
			}
			cur = &Artifact{URL: strings.Trim(strings.TrimSpace(m[1]), `"'`)}
			continue
		}
		if cur != nil {
			if m := shaRe.FindStringSubmatch(trimmed); m != nil {
				cur.Sha512 = strings.Trim(strings.TrimSpace(m[1]), `"'`)
			}
		}
	}
	if cur != nil {
		artifacts = append(artifacts, *cur)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	for _, a := range artifacts {
		if strings.HasSuffix(strings.ToLower(a.URL), ext) {
			return &Release{Version: version, Artifact: a, URL: a.URL}, nil
		}
	}
	return nil, fmt.Errorf("no %s artifact in update manifest", ext)
}

// parseJSONManifest handles a JSON service response (files[] with url/sha512).
func parseJSONManifest(data []byte, ext string) *Release {
	var m struct {
		Version string `json:"version"`
		Files   []struct {
			URL    string `json:"url"`
			Sha512 string `json:"sha512"`
		} `json:"files"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil
	}
	for _, f := range m.Files {
		if strings.HasSuffix(strings.ToLower(f.URL), ext) {
			return &Release{Version: m.Version, Artifact: Artifact{URL: f.URL, Sha512: f.Sha512}}
		}
	}
	return nil
}
