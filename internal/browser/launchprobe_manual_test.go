package browser

import (
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestLaunchProbe uses the real Launch() path and dumps everything: the tab
// list, the chosen wsURL, and a Page.enable probe with a short deadline.
func TestLaunchProbe(t *testing.T) {
	if os.Getenv("ZQF_WS_PROBE_SPAWN") == "" {
		t.Skip("ZQF_WS_PROBE_SPAWN not set")
	}
	b, err := Launch()
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	defer b.Close()

	t.Logf("browser id=%s cdpPort=%s", b.ID(), b.CDPPort())
	resp, err := http.Get("http://127.0.0.1:" + b.CDPPort() + "/json")
	if err != nil {
		t.Fatalf("raw list: %v", err)
	}
	var list []struct {
		Type  string `json:"type"`
		URL   string `json:"url"`
		WsURL string `json:"webSocketDebuggerUrl"`
	}
	if err := jsonNewDecoder(resp.Body)(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	for _, tgt := range list {
		t.Logf("target type=%q url=%.50q ws=%s", tgt.Type, tgt.URL, tgt.WsURL)
	}

	ws, err := b.pageWS("")
	if err != nil {
		t.Fatalf("pageWS: %v", err)
	}
	t.Logf("pageWS -> %s", ws)
	conn, _, err := websocket.DefaultDialer.Dial(ws, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(4 * time.Second))
	if err := conn.WriteJSON(map[string]any{"id": 1, "method": "Page.enable"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read enable: %v", err)
	}
	t.Logf("enable: %.80s", msg)

	// 1) navigate on the SAME (Page.enable'd) connection
	_ = conn.SetReadDeadline(time.Now().Add(6 * time.Second))
	if err := conn.WriteJSON(map[string]any{"id": 2, "method": "Page.navigate", "params": map[string]any{"url": "http://example.com"}}); err != nil {
		t.Fatalf("write nav: %v", err)
	}
	_, msg, err = conn.ReadMessage()
	if err != nil {
		t.Fatalf("read nav on enabled conn: %v", err)
	}
	t.Logf("nav on enabled conn: %.100s", msg)

	// 2) navigate on a FRESH connection (cdpCall's pattern)
	conn2, _, err := websocket.DefaultDialer.Dial(ws, nil)
	if err != nil {
		t.Fatalf("dial2: %v", err)
	}
	defer conn2.Close()
	_ = conn2.SetReadDeadline(time.Now().Add(6 * time.Second))
	if err := conn2.WriteJSON(map[string]any{"id": 3, "method": "Page.navigate", "params": map[string]any{"url": "https://www.baidu.com"}}); err != nil {
		t.Fatalf("write nav2: %v", err)
	}
	_, msg, err = conn2.ReadMessage()
	if err != nil {
		t.Fatalf("read nav on fresh conn: %v", err)
	}
	t.Logf("nav on fresh conn: %.100s", msg)
	t.Log("LAUNCH PROBE ALL OK")
}

func jsonNewDecoder(r interface{ Read([]byte) (int, error) }) func(any) error {
	return func(v any) error { return jsonDecodeReader(r, v) }
}
