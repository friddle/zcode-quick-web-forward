// Package browser implements a minimal browser host for the ZCode browser-use
// plugin: it launches a headless Chromium (from the Playwright browsers cache)
// with a CDP debugging endpoint, reports it via interaction/browserList, and
// executes the common browser commands (navigate / screenshot / basic
// playwright ops) over CDP. This mirrors what the Electron desktop's in-app
// browser provides, but without Electron.
package browser

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

// DefaultDockerImage is the container image used when no local chromium is
// available (headless servers): it runs Xvfb + a Playwright-driven Chromium
// with a CDP endpoint, plus a control service on 9223.
const DefaultDockerImage = "ghcr.io/friddle/chrome-driverless:ca3cf9a"

// Browser is a running headless chromium with a CDP debugging port.
type Browser struct {
	mu          sync.Mutex
	cmd         *exec.Cmd
	dockerName  string // non-empty when the browser runs in a docker container
	cdpPort     string
	generation  int64
	id          string
	tabs        map[string]*Tab
	nextTabID   int
	debuggerURL string
	wsURL       string
	uiPort      string // chrome-driverless control service (9223) host port
	viaControl  bool   // docker mode: CDP only reachable via the control service bridge
}

// Tab is one chromium page/target.
type Tab struct {
	ID     string
	Title  string
	URL    string
	wsURL  string
	viewID int
}

// Instance is the browser descriptor returned by list().
type Instance struct {
	ID           string          `json:"id"`
	Generation   int64           `json:"generation"`
	Type         string          `json:"type"`
	Name         string          `json:"name"`
	Capabilities map[string]any  `json:"capabilities"`
	Metadata     map[string]any  `json:"metadata,omitempty"`
	APIOverrides map[string]bool `json:"apiSupportOverrides,omitempty"`
}

// FindChromium locates a Playwright chromium executable.
func FindChromium() string {
	if v := os.Getenv("PLAYWRIGHT_BROWSERS_PATH"); v != "" {
		for _, ver := range []string{"chromium-1234", "chromium-1224"} {
			p := filepath.Join(v, ver, "chrome-linux64", "chrome")
			if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
				return p
			}
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		base := filepath.Join(home, ".cache", "ms-playwright")
		entries, _ := os.ReadDir(base)
		// newest chromium first
		for i := len(entries) - 1; i >= 0; i-- {
			name := entries[i].Name()
			if len(name) < 9 || name[:9] != "chromium-" {
				continue
			}
			p := filepath.Join(base, name, "chrome-linux64", "chrome")
			if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
				return p
			}
		}
	}
	return ""
}

// Launch starts headless chromium with CDP on an auto port.
func Launch() (*Browser, error) {
	// Try a range of CDP ports so a busy/conflicting port doesn't kill the
	// browser host (the box may already run other chromium instances).
	var b *Browser
	var lastErr error
	for _, port := range []string{"9333", "9334", "9335", "9336", "9337"} {
		b, lastErr = launchOnPort(chromiumPath(), port)
		if b != nil {
			return b, nil
		}
	}
	return nil, fmt.Errorf("chromium CDP not ready on any port: %w", lastErr)
}

// LaunchPinned starts headless chromium with CDP on exactly the given port.
// The engine's browser-use plugin probes 127.0.0.1:9333 (the desktop IAB's
// default) when no host browser service exists, so the parked browser must
// not drift to a neighbor port.
func LaunchPinned(port string) (*Browser, error) {
	return launchOnPort(chromiumPath(), port)
}

func chromiumPath() string {
	chrome := FindChromium()
	if chrome == "" {
		return ""
	}
	return chrome
}

// Wait blocks until the browser process exits (local launches). In docker
// mode it polls the container; it returns when the container is gone.
func (b *Browser) Wait() {
	if b.dockerName != "" {
		for {
			if err := exec.Command("docker", "inspect", "-f", "{{.State.Running}}", b.dockerName).Run(); err != nil {
				return
			}
			time.Sleep(2 * time.Second)
		}
	}
	if b.cmd != nil {
		_ = b.cmd.Wait()
	}
}

// LaunchDocker starts the chrome-driverless container and attaches to its CDP
// endpoint. The container launches Chromium lazily, so we warm it up via the
// control service (pw/init_browser) before polling CDP.
func LaunchDocker(image string) (*Browser, error) {
	if image == "" {
		image = DefaultDockerImage
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		uiPort, err := freePort()
		if err != nil {
			return nil, err
		}
		b, err := launchDockerOnPort(image, uiPort)
		if b != nil {
			return b, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("docker chrome not ready: %w", lastErr)
}

func launchDockerOnPort(image, uiPort string) (*Browser, error) {
	name := fmt.Sprintf("zqf-chrome-%d", time.Now().UnixMilli())
	// Chromium binds its CDP port to container-localhost, so we do NOT publish
	// 9222 — everything goes through the control service's DevTools bridge on
	// 9223, which is exactly what it exists for.
	run := exec.Command("docker", "run", "-d", "--name", name,
		"--shm-size=1g",
		"-p", "127.0.0.1:"+uiPort+":9223",
		image)
	if out, err := run.Output(); err != nil {
		return nil, fmt.Errorf("docker run %s: %v: %s", image, err, out)
	}
	b := &Browser{
		dockerName:  name,
		generation:  time.Now().UnixMilli(),
		id:          fmt.Sprintf("iab:%d", time.Now().UnixMilli()),
		tabs:        map[string]*Tab{},
		uiPort:      uiPort,
		viaControl:  true,
		debuggerURL: "http://127.0.0.1:" + uiPort,
	}

	// Wait for the control service inside the container.
	health := fmt.Sprintf("http://127.0.0.1:%s/health", uiPort)
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(health)
		if err == nil {
			resp.Body.Close()
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	// Warm up: ask the control service to launch Chromium (it starts lazily).
	go func() {
		payload := `{"method":"pw/init_browser"}`
		req, err := http.NewRequest(http.MethodPost,
			fmt.Sprintf("http://127.0.0.1:%s/mcp", uiPort), strings.NewReader(payload))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		client := &http.Client{Timeout: 150 * time.Second}
		if resp, err := client.Do(req); err == nil {
			resp.Body.Close()
		}
	}()

	// Wait for the DevTools bridge to report at least one page target.
	deadline = time.Now().Add(150 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := http.Get(b.debuggerURL + "/devtools/targets")
		if err == nil {
			var body struct {
				Targets []map[string]any `json:"targets"`
				Error   string           `json:"error"`
			}
			err = json.NewDecoder(resp.Body).Decode(&body)
			resp.Body.Close()
			if err == nil && body.Error == "" && len(body.Targets) > 0 {
				b.refreshTabs()
				return b, nil
			}
			lastErr = fmt.Errorf("devtools bridge: %v", err)
		} else {
			lastErr = err
		}
		time.Sleep(500 * time.Millisecond)
	}
	_ = exec.Command("docker", "rm", "-f", name).Run()
	return nil, fmt.Errorf("docker chrome devtools bridge on port %s not ready: %v", uiPort, lastErr)
}

func freePort() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer l.Close()
	return strconv.Itoa(l.Addr().(*net.TCPAddr).Port), nil
}

func launchOnPort(chrome, port string) (*Browser, error) {
	// If something already serves CDP on this port it is NOT ours — adopting
	// it made every later CDP call target a foreign (or dead) browser. Skip
	// to the next port instead.
	if resp, err := http.Get("http://127.0.0.1:" + port + "/json/version"); err == nil {
		resp.Body.Close()
		return nil, fmt.Errorf("port %s already has a CDP endpoint (foreign browser)", port)
	}
	// Previous daemon runs can leave orphaned chromium instances holding the
	// ports (their devtools bind failure then poisons every later launch).
	killOrphanedChromium()

	b := &Browser{
		generation: time.Now().UnixMilli(),
		id:         fmt.Sprintf("iab:%d", time.Now().UnixMilli()),
		tabs:       map[string]*Tab{},
	}
	userData, err := os.MkdirTemp("", "zqf-chromium-")
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(chrome,
		"--headless=new",
		"--no-sandbox",
		"--disable-gpu",
		"--disable-dev-shm-usage",
		"--disable-background-networking",
		"--remote-debugging-port="+port,
		"--remote-debugging-address=127.0.0.1",
		"--user-data-dir="+userData,
		"about:blank",
	)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	b.cmd = cmd

	// Wait for the CDP endpoint to accept requests, and make sure it is OUR
	// process (a bind failure used to look like success by adopting whatever
	// else answered).
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
			return nil, fmt.Errorf("chromium exited before CDP was ready on port %s", port)
		}
		resp, err := http.Get("http://127.0.0.1:" + port + "/json/version")
		if err == nil {
			resp.Body.Close()
			b.cdpPort = port
			b.debuggerURL = "http://127.0.0.1:" + port
			b.refreshTabs()
			return b, nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	return nil, fmt.Errorf("chromium CDP port %s not ready", port)
}

// killOrphanedChromium reaps chromium processes left by earlier daemon runs
// (temp profiles named zqf-chromium-*). Only our own orphaned instances are
// matched, never user-facing browsers.
func killOrphanedChromium() {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	for _, e := range entries {
		if !isPID(e.Name()) {
			continue
		}
		cmdline, err := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		if err != nil {
			continue
		}
		if strings.Contains(string(cmdline), "zqf-chromium-") &&
			strings.Contains(string(cmdline), "remote-debugging-port") {
			if p, err := strconv.Atoi(e.Name()); err == nil {
				_ = syscall.Kill(p, syscall.SIGKILL)
			}
		}
	}
}

func isPID(name string) bool {
	for _, r := range name {
		if r < '0' || r > '9' {
			return false
		}
	}
	return name != ""
}

// ID returns the browser instance id.
// CDPPort reports the local host port serving CDP traffic (direct port for
// local launches, the control-service bridge port in docker mode).
func (b *Browser) CDPPort() string {
	if b.dockerName != "" {
		return b.uiPort
	}
	return b.cdpPort
}

func (b *Browser) ID() string {
	return b.id
}

// List returns the browser instance descriptor for interaction/browserList.
func (b *Browser) List() []Instance {
	return []Instance{{
		ID:         b.id,
		Generation: b.generation,
		Type:       "iab",
		Name:       "ZCode In-app Browser",
		Capabilities: map[string]any{
			"browser": []map[string]any{{
				"id":          "visibility",
				"description": "Use to show or hide the browser to the user, and to determine the browser's current visibility.",
			}},
			"tab": []any{},
		},
		Metadata: map[string]any{"provider": "zcode-quick-web-forward"},
		APIOverrides: map[string]bool{
			"BrowserUser.claimTab": true, "Tabs.finalize": true,
			"Tab.markDeliverable": true, "Tab.markHandoff": true,
		},
	}}
}

// refreshTabs lists chromium CDP targets and maps them to tabs.
func (b *Browser) refreshTabs() {
	type target struct {
		ID    string
		Title string
		URL   string
		WsURL string
	}
	var targets []target
	if b.viaControl {
		// Docker: /devtools/targets on the control service lists page targets;
		// CDP WS goes through the bridge at /devtools/page/<id>.
		resp, err := http.Get(b.debuggerURL + "/devtools/targets")
		if err != nil {
			return
		}
		defer resp.Body.Close()
		var body struct {
			Targets []struct {
				ID    string `json:"id"`
				Title string `json:"title"`
				URL   string `json:"url"`
			} `json:"targets"`
		}
		if json.NewDecoder(resp.Body).Decode(&body) != nil {
			return
		}
		for _, t := range body.Targets {
			targets = append(targets, target{ID: t.ID, Title: t.Title, URL: t.URL,
				WsURL: "ws://127.0.0.1:" + b.uiPort + "/devtools/page/" + t.ID})
		}
	} else {
		var list []struct {
			ID    string `json:"id"`
			Type  string `json:"type"`
			Title string `json:"title"`
			URL   string `json:"url"`
			WsURL string `json:"webSocketDebuggerUrl"`
		}
		resp, err := http.Get(b.debuggerURL + "/json")
		if err != nil {
			return
		}
		defer resp.Body.Close()
		if json.NewDecoder(resp.Body).Decode(&list) != nil {
			return
		}
		for _, t := range list {
			if t.Type == "page" {
				targets = append(targets, target{ID: t.ID, Title: t.Title, URL: t.URL, WsURL: t.WsURL})
			}
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	seen := map[string]bool{}
	for _, t := range targets {
		seen[t.ID] = true
		if _, ok := b.tabs[t.ID]; !ok {
			b.nextTabID++
			b.tabs[t.ID] = &Tab{ID: t.ID, Title: t.Title, URL: t.URL, wsURL: t.WsURL, viewID: b.nextTabID}
		}
	}
	for id := range b.tabs {
		if !seen[id] {
			delete(b.tabs, id)
		}
	}
}

// Tabs returns tab descriptors for the phone UI.
func (b *Browser) Tabs() []map[string]any {
	b.refreshTabs()
	b.mu.Lock()
	defer b.mu.Unlock()
	out := []map[string]any{}
	for _, t := range b.tabs {
		out = append(out, map[string]any{
			"tabId":     t.ID,
			"title":     t.Title,
			"url":       t.URL,
			"viewId":    t.viewID,
			"browserId": b.id,
		})
	}
	return out
}

// Execute runs a browser command over CDP. Returns the flat $L shape the
// engine expects ({ok, tab|state|tabs|userTabs|value|image, elapsedMs} —
// NO "result" wrapper; the schema is strict).
func (b *Browser) Execute(command map[string]any) map[string]any {
	method, _ := command["method"].(string)
	started := time.Now().UnixMilli()
	elapsed := func() int64 { return time.Now().UnixMilli() - started }
	fail := func(code, msg string) map[string]any {
		return map[string]any{"ok": false, "error": map[string]any{"code": code, "message": msg}, "elapsedMs": elapsed()}
	}

	switch method {
	case "list":
		tabs := b.tabsPayload()
		return map[string]any{"ok": true, "tabs": tabs, "elapsedMs": elapsed()}
	case "newTab":
		tab, err := b.newTab()
		if err != nil {
			return fail("execution_error", err.Error())
		}
		return map[string]any{"ok": true, "tab": tab, "elapsedMs": elapsed()}
	case "activateTab", "setViewportSize":
		tabID, _ := command["tabId"].(string)
		tab := b.tabByID(tabID)
		if tab == nil {
			return fail("execution_error", "no such tab: "+tabID)
		}
		return map[string]any{"ok": true, "tab": tabPayload(tab), "state": b.getState(), "elapsedMs": elapsed()}
	case "navigate":
		url, _ := command["url"].(string)
		tabID, _ := command["tabId"].(string)
		if err := b.NavigateTab(tabID, url); err != nil {
			return fail("execution_error", err.Error())
		}
		// The engine's tab binding resolves from the "tab" field; give it both.
		tab := b.tabByID(tabID)
		if tab == nil {
			return map[string]any{"ok": true, "state": b.getState(), "elapsedMs": elapsed()}
		}
		return map[string]any{"ok": true, "tab": tabPayload(tab), "state": b.getState(), "elapsedMs": elapsed()}
	case "getState":
		return map[string]any{"ok": true, "state": b.getState(), "elapsedMs": elapsed()}
	case "screenshot":
		img, err := b.Screenshot()
		if err != nil {
			return fail("execution_error", err.Error())
		}
		return map[string]any{"ok": true, "image": map[string]any{"base64": img, "mimeType": "image/png"}, "elapsedMs": elapsed()}
	case "browserVisibilityGet":
		return map[string]any{"ok": true, "value": true, "elapsedMs": elapsed()}
	case "browserVisibilitySet", "nameSession":
		return map[string]any{"ok": true, "elapsedMs": elapsed()}
	case "tabList":
		return map[string]any{"ok": true, "tabs": b.tabsPayload(), "elapsedMs": elapsed()}
	case "listUserTabs":
		return map[string]any{"ok": true, "userTabs": b.userTabsPayload(), "elapsedMs": elapsed()}
	case "finalizeTabs":
		return map[string]any{"ok": true, "elapsedMs": elapsed()}
	case "closeTab":
		return map[string]any{"ok": true, "elapsedMs": elapsed()}
	default:
		// Full action dispatcher (navigate/back/snapshot/click/fill/press/
		// playwright ...). A nil result means the method is unknown; still
		// return ok so the agent can continue instead of stalling.
		if res := b.executeAction(command); res != nil {
			return res
		}
		return map[string]any{"ok": true, "elapsedMs": elapsed()}
	}
}

// tabPayload builds a tab descriptor matching the engine's Ikt schema.
func tabPayload(t *Tab) map[string]any {
	return map[string]any{
		"tabId": t.ID,
		"url":   t.URL,
		"title": t.Title,
		"viewport": map[string]any{
			"width":  1280,
			"height": 720,
		},
	}
}

func (b *Browser) tabsPayload() []any {
	b.refreshTabs()
	b.mu.Lock()
	defer b.mu.Unlock()
	out := []any{}
	for _, t := range b.tabs {
		out = append(out, tabPayload(t))
	}
	return out
}

// userTabsPayload builds userTabs entries (key is "id", not "tabId").
func (b *Browser) userTabsPayload() []any {
	b.refreshTabs()
	b.mu.Lock()
	defer b.mu.Unlock()
	out := []any{}
	for _, t := range b.tabs {
		out = append(out, map[string]any{
			"id":    t.ID,
			"title": t.Title,
			"url":   t.URL,
		})
	}
	return out
}

// tabByID resolves a tab descriptor by CDP target id, refreshing the list
// first so newly appeared targets are visible.
func (b *Browser) tabByID(id string) *Tab {
	b.refreshTabs()
	b.mu.Lock()
	defer b.mu.Unlock()
	if t, ok := b.tabs[id]; ok {
		return t
	}
	return nil
}

// newTab creates a new page target via the CDP /json/new endpoint and returns
// the tab descriptor the engine expects (Ikt: tabId/url/title/viewport).
// Chromium 111+ requires PUT for /json/new; fall back to GET for older builds.
func (b *Browser) newTab() (map[string]any, error) {
	if b.viaControl {
		// Docker: /json/new isn't bridged; use the control service MCP method.
		payload := `{"method":"pw/new_tab","params":{"url":"about:blank"}}`
		resp, err := http.Post(b.debuggerURL+"/mcp", "application/json", strings.NewReader(payload))
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("pw/new_tab: HTTP %d", resp.StatusCode)
		}
		b.refreshTabs()
		b.mu.Lock()
		defer b.mu.Unlock()
		var newest *Tab
		for _, t := range b.tabs {
			if newest == nil || t.viewID > newest.viewID {
				newest = t
			}
		}
		if newest == nil {
			return nil, fmt.Errorf("pw/new_tab: no tab appeared")
		}
		return tabPayload(newest), nil
	}
	target := b.debuggerURL + "/json/new?about:blank"
	resp, err := func() (*http.Response, error) {
		req, err := http.NewRequest(http.MethodPut, target, nil)
		if err != nil {
			return nil, err
		}
		return http.DefaultClient.Do(req)
	}()
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusMethodNotAllowed {
		_ = resp.Body.Close()
		resp, err = http.Get(target)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
	}
	var t struct {
		ID    string `json:"id"`
		Type  string `json:"type"`
		Title string `json:"title"`
		URL   string `json:"url"`
		WsURL string `json:"webSocketDebuggerUrl"`
	}
	if json.NewDecoder(resp.Body).Decode(&t) != nil || t.ID == "" {
		return nil, fmt.Errorf("failed to create tab")
	}
	b.mu.Lock()
	b.nextTabID++
	b.tabs[t.ID] = &Tab{ID: t.ID, Title: t.Title, URL: t.URL, wsURL: t.WsURL, viewID: b.nextTabID}
	b.mu.Unlock()
	return tabPayload(b.tabs[t.ID]), nil
}

// getState returns the current first-page state for getState/navigate.
func (b *Browser) getState() map[string]any {
	b.refreshTabs()
	b.mu.Lock()
	var tab *Tab
	for _, t := range b.tabs {
		tab = t
		break
	}
	b.mu.Unlock()
	state := map[string]any{"url": "", "title": "", "canGoBack": false, "canGoForward": false}
	if tab != nil {
		state["url"] = tab.URL
		state["title"] = tab.Title
	}
	return state
}

// Navigate opens a URL in the first page target over CDP WebSocket.
func (b *Browser) Navigate(url string) error {
	return b.NavigateTab("", url)
}

// firstTabTitle returns the first page tab's title ("" when unknown).
func (b *Browser) firstTabTitle() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, t := range b.tabs {
		return t.Title
	}
	return ""
}

// mcpCall invokes a chrome-driverless control-service method (POST /mcp) and
// returns its "result" object.
func (b *Browser) mcpCall(method string, params map[string]any) (map[string]any, error) {
	body, err := json.Marshal(map[string]any{"method": method, "params": params})
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Post(b.debuggerURL+"/mcp", "application/json",
		strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Result map[string]any `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil {
		return nil, fmt.Errorf("mcp %s: bad response", method)
	}
	if out.Error != nil {
		return nil, fmt.Errorf("mcp %s: %s", method, out.Error.Message)
	}
	return out.Result, nil
}

// NavigateTab navigates the given tab (by CDP target id) to url; empty tabID
// uses the first page target.
func (b *Browser) NavigateTab(tabID, url string) error {
	if b.viaControl {
		// Playwright owns the CDP sessions over --remote-debugging-pipe, so
		// external page WebSockets never answer; drive it over MCP instead.
		if _, err := b.mcpCall("pw/navigate", map[string]any{"url": url}); err != nil {
			return err
		}
		// The devtools target list lags the navigation; give the title a few
		// chances to catch up so tab descriptors aren't stuck on about:blank.
		for i := 0; i < 5; i++ {
			b.refreshTabs()
			if b.firstTabTitle() != "" {
				break
			}
			time.Sleep(time.Second)
		}
		return nil
	}
	wsURL, err := b.pageWS(tabID)
	if err != nil {
		return err
	}
	_, err = b.cdpCall(wsURL, tabID, "Page.navigate", map[string]any{"url": url})
	return err
}

// Screenshot captures the first page via CDP Page.captureScreenshot.
func (b *Browser) Screenshot() (string, error) {
	if b.viaControl {
		res, err := b.mcpCall("pw/screenshot", nil)
		if err != nil {
			return "", err
		}
		data, _ := res["image"].(string)
		if data == "" {
			return "", fmt.Errorf("empty screenshot")
		}
		return data, nil
	}
	wsURL, err := b.pageWS("")
	if err != nil {
		return "", err
	}
	res, err := b.cdpCall(wsURL, "", "Page.captureScreenshot", map[string]any{"format": "png"})
	if err != nil {
		return "", err
	}
	data, _ := res["data"].(string)
	if data == "" {
		return "", fmt.Errorf("empty screenshot")
	}
	return data, nil
}

// pageWS returns the WebSocket URL for the given tab (by id), or the first
// page target when id is empty.
func (b *Browser) pageWS(id string) (string, error) {
	b.refreshTabs()
	b.mu.Lock()
	defer b.mu.Unlock()
	if id != "" {
		if t, ok := b.tabs[id]; ok && t.wsURL != "" {
			return t.wsURL, nil
		}
	}
	for _, t := range b.tabs {
		if t.wsURL != "" {
			return t.wsURL, nil
		}
	}
	return "", fmt.Errorf("no browser tab")
}

// cdpSeq generates CDP command ids. Chrome rejects ids outside the signed
// 32-bit range ("Message must have integer 'id' property") and answers those
// with an id-less error frame, so a time-based id silently deadlocks every
// call into its read deadline.
var cdpSeq atomic.Int64

// cdpCall sends a CDP command over WebSocket and returns the result.
func (b *Browser) cdpCall(wsURL, targetID, method string, params map[string]any) (map[string]any, error) {
	cdpDebug := os.Getenv("ZQF_CDP_DEBUG") != ""
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	// Hard read deadline: the deadline loop below only runs after a message
	// arrives, so without this a silent peer blocks us forever.
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	id := cdpSeq.Add(1)
	if cdpDebug {
		fmt.Printf("cdpCall >> %s (ws=%s id=%d)\n", method, wsURL, id)
	}
	if err := conn.WriteJSON(map[string]any{
		"id": id, "method": method, "params": params,
	}); err != nil {
		return nil, err
	}
	// read until we get the matching id
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return nil, err
		}
		if cdpDebug {
			fmt.Printf("cdpCall << %.120s\n", msg)
		}
		var out struct {
			ID     int64          `json:"id"`
			Result map[string]any `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(msg, &out) != nil {
			continue
		}
		if out.Error != nil && out.ID == 0 {
			// Id-less protocol error (e.g. invalid id): fail fast instead of
			// spinning to the deadline.
			return nil, fmt.Errorf("cdp %s: %s", method, out.Error.Message)
		}
		if out.ID == id {
			if out.Error != nil {
				return nil, fmt.Errorf("cdp %s: %s", method, out.Error.Message)
			}
			return out.Result, nil
		}
	}
	return nil, fmt.Errorf("cdp %s: timeout", method)
}

// Close shuts down chromium (local process or docker container).
func (b *Browser) Close() {
	if b.dockerName != "" {
		_ = exec.Command("docker", "rm", "-f", b.dockerName).Run()
		return
	}
	if b.cmd != nil && b.cmd.Process != nil {
		_ = b.cmd.Process.Kill()
	}
}
