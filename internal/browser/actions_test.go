package browser

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testPageHTML = `<!doctype html><html><title>zqf-test</title><body><main>
<h1>Hello zqf</h1>
<input id="q" placeholder="Search here"/>
<button onclick="document.getElementById('out').textContent='clicked'">Go</button>
<p id="out">idle</p>
</main></body></html>`

// TestExecuteActions drives the full action dispatcher against a real
// headless chromium: navigate a local page, snapshot with refs, fill+press,
// playwright.domSnapshot, evaluate and screenshot.
func TestExecuteActions(t *testing.T) {
	if FindChromium() == "" {
		t.Skip("no local chromium")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/testpage", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, testPageHTML)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	b, err := Launch()
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	defer b.Close()

	res := b.Execute(map[string]any{"method": "navigate", "url": srv.URL + "/testpage"})
	if ok, _ := res["ok"].(bool); !ok {
		t.Fatalf("navigate failed: %v", res)
	}

	// snapshot: elements carry refs, refSelectors registered
	res = b.Execute(map[string]any{"method": "snapshot"})
	if ok, _ := res["ok"].(bool); !ok {
		t.Fatalf("snapshot failed: %v", res)
	}
	val, _ := res["value"].(map[string]any)
	if val == nil {
		t.Fatalf("snapshot value missing: %v", res)
	}
	els, _ := val["elements"].([]any)
	if len(els) == 0 {
		t.Fatalf("snapshot has no elements: %v", val)
	}
	var inputRef string
	for _, e := range els {
		em, _ := e.(map[string]any)
		if em["tag"] == "input" {
			inputRef, _ = em["ref"].(string)
		}
	}
	if inputRef == "" {
		t.Fatalf("input element not in snapshot: %v", els)
	}

	// fill via ref
	res = b.Execute(map[string]any{"method": "fill", "ref": inputRef, "value": "baidu"})
	if ok, _ := res["ok"].(bool); !ok {
		t.Fatalf("fill failed: %v", res)
	}
	res = b.Execute(map[string]any{"method": "evaluate", "expression": "document.getElementById('q').value"})
	if v, _ := res["value"].(string); v != "baidu" {
		t.Fatalf("fill not applied: %v", res)
	}

	// click via ref triggers the onclick handler
	res = b.Execute(map[string]any{"method": "snapshot"})
	val, _ = res["value"].(map[string]any)
	var btnRef string
	for _, e := range val["elements"].([]any) {
		em, _ := e.(map[string]any)
		if em["tag"] == "button" {
			btnRef, _ = em["ref"].(string)
		}
	}
	res = b.Execute(map[string]any{"method": "click", "ref": btnRef})
	if ok, _ := res["ok"].(bool); !ok {
		t.Fatalf("click failed: %v", res)
	}
	res = b.Execute(map[string]any{"method": "evaluate", "expression": "document.getElementById('out').textContent"})
	if v, _ := res["value"].(string); v != "clicked" {
		t.Fatalf("click handler not fired: %v", res)
	}

	// playwright.domSnapshot returns YAML with the ref
	res = b.Execute(map[string]any{"method": "playwright", "action": map[string]any{"name": "domSnapshot"}})
	if ok, _ := res["ok"].(bool); !ok {
		t.Fatalf("playwright.domSnapshot failed: %v", res)
	}
	if yaml, _ := res["value"].(string); !strings.Contains(yaml, "[ref=") {
		t.Fatalf("domSnapshot missing refs: %q", yaml)
	}

	// playwright.locator.count
	res = b.Execute(map[string]any{"method": "playwright", "action": map[string]any{
		"name": "locator", "selector": "button", "operation": "count"}})
	if v, _ := res["value"].(float64); v != 1 {
		t.Fatalf("locator count: %v", res)
	}

	// screenshot returns an image
	res = b.Execute(map[string]any{"method": "screenshot"})
	if ok, _ := res["ok"].(bool); !ok {
		t.Fatalf("screenshot failed: %v", res)
	}
	img, _ := res["image"].(map[string]any)
	if img == nil || img["base64"] == "" {
		t.Fatalf("screenshot image missing: %v", res)
	}

	// waitFor text
	res = b.Execute(map[string]any{"method": "waitFor", "text": "clicked", "timeoutMs": 3000})
	if ok, _ := res["ok"].(bool); !ok {
		t.Fatalf("waitFor failed: %v", res)
	}
}
