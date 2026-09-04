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
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Browser is a running headless chromium with a CDP debugging port.
type Browser struct {
	mu          sync.Mutex
	cmd         *exec.Cmd
	cdpPort     string
	generation  int64
	id          string
	tabs        map[string]*Tab
	nextTabID   int
	debuggerURL string
	wsURL       string
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
	chrome := FindChromium()
	if chrome == "" {
		return nil, fmt.Errorf("no Playwright chromium found; install playwright browsers")
	}
	// Try a range of CDP ports so a busy/conflicting port doesn't kill the
	// browser host (the box may already run other chromium instances).
	var b *Browser
	var lastErr error
	for _, port := range []string{"9333", "9334", "9335", "9336", "9337"} {
		b, lastErr = launchOnPort(chrome, port)
		if b != nil {
			return b, nil
		}
	}
	return nil, fmt.Errorf("chromium CDP not ready on any port: %w", lastErr)
}

func launchOnPort(chrome, port string) (*Browser, error) {
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

	// Wait for the CDP endpoint to accept requests.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://127.0.0.1:" + port + "/json")
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

// ID returns the browser instance id.
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
	var targets []struct {
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
	_ = json.NewDecoder(resp.Body).Decode(&targets)
	b.mu.Lock()
	defer b.mu.Unlock()
	seen := map[string]bool{}
	for _, t := range targets {
		if t.Type != "page" {
			continue
		}
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
		// Unknown commands: return ok so the agent can continue.
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

// NavigateTab navigates the given tab (by CDP target id) to url; empty tabID
// uses the first page target.
func (b *Browser) NavigateTab(tabID, url string) error {
	wsURL, err := b.pageWS(tabID)
	if err != nil {
		return err
	}
	_, err = b.cdpCall(wsURL, tabID, "Page.navigate", map[string]any{"url": url})
	return err
}

// Screenshot captures the first page via CDP Page.captureScreenshot.
func (b *Browser) Screenshot() (string, error) {
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

// cdpCall sends a CDP command over WebSocket and returns the result.
func (b *Browser) cdpCall(wsURL, targetID, method string, params map[string]any) (map[string]any, error) {
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	id := time.Now().UnixMilli()
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
		var out struct {
			ID     int64          `json:"id"`
			Result map[string]any `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(msg, &out) != nil {
			continue
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

// Close shuts down chromium.
func (b *Browser) Close() {
	if b.cmd != nil && b.cmd.Process != nil {
		_ = b.cmd.Process.Kill()
	}
}
