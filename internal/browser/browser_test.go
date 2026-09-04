package browser

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// cdpFake mimics the subset of the CDP HTTP endpoints the host uses, with the
// Chromium 111+ behaviour of rejecting GET on /json/new.
type cdpFake struct {
	t       *testing.T
	sawVerb string
}

func (f *cdpFake) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"T1","type":"page","title":"blank","url":"about:blank","webSocketDebuggerUrl":"ws://127.0.0.1/devtools/page/T1"}]`))
	})
	mux.HandleFunc("/json/new", func(w http.ResponseWriter, r *http.Request) {
		f.sawVerb = r.Method
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			_, _ = w.Write([]byte(`Using unsafe HTTP verb GET to invoke /json/new`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"T2","type":"page","title":"","url":"about:blank","webSocketDebuggerUrl":"ws://127.0.0.1/devtools/page/T2"}`))
	})
	return mux
}

func TestNewTabUsesPUT(t *testing.T) {
	fake := &cdpFake{t: t}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	b := &Browser{debuggerURL: srv.URL, tabs: map[string]*Tab{}}
	tab, err := b.newTab()
	if err != nil {
		t.Fatalf("newTab: %v", err)
	}
	if fake.sawVerb != http.MethodPut {
		t.Fatalf("expected PUT on /json/new, saw %s", fake.sawVerb)
	}
	if tab["tabId"] != "T2" {
		t.Fatalf("unexpected tab payload: %v", tab)
	}
	if tab["viewport"] == nil {
		t.Fatalf("tab payload missing viewport: %v", tab)
	}
}

func TestActivateTabReturnsTabPayload(t *testing.T) {
	fake := &cdpFake{t: t}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	b := &Browser{debuggerURL: srv.URL, tabs: map[string]*Tab{}}
	b.tabs["T1"] = &Tab{ID: "T1", Title: "blank", URL: "about:blank",
		wsURL: "ws://127.0.0.1/devtools/page/T1", viewID: 1}

	res := b.Execute(map[string]any{"method": "activateTab", "tabId": "T1"})
	if res["ok"] != true {
		t.Fatalf("activateTab failed: %v", res)
	}
	tab, _ := res["tab"].(map[string]any)
	if tab == nil || tab["tabId"] != "T1" {
		t.Fatalf("activateTab response missing tab payload: %v", res)
	}
}
