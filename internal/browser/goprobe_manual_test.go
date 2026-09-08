package browser

import (
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestGoProbeManual isolates the gorilla dial/read path against a running
// chromium devtools endpoint. Set ZQF_WS_PROBE_PORT to run.
func TestGoProbeManual(t *testing.T) {
	port := os.Getenv("ZQF_WS_PROBE_PORT")
	if port == "" {
		t.Skip("ZQF_WS_PROBE_PORT not set")
	}
	resp, err := http.Get("http://127.0.0.1:" + port + "/json")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var list []struct {
		Type  string `json:"type"`
		URL   string `json:"url"`
		WsURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	var page string
	for _, tgt := range list {
		if tgt.Type == "page" && tgt.WsURL != "" {
			page = tgt.WsURL
			break
		}
	}
	if page == "" {
		t.Fatalf("no page target")
	}
	t.Logf("step 1: dialing %s", page)
	d := &websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	conn, _, err := d.Dial(page, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	t.Log("step 2: dial OK")
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := conn.WriteJSON(map[string]any{"id": 1, "method": "Page.enable"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Log("step 3: Page.enable written")
	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read Page.enable reply: %v", err)
	}
	t.Logf("step 4: reply %.100s", msg)
	if err := conn.WriteJSON(map[string]any{"id": 2, "method": "Runtime.evaluate", "params": map[string]any{"expression": "location.href", "returnByValue": true}}); err != nil {
		t.Fatalf("write2: %v", err)
	}
	t.Log("step 5: evaluate written")
	for i := 0; i < 5; i++ {
		_, msg, err = conn.ReadMessage()
		if err != nil {
			t.Fatalf("read evaluate reply: %v", err)
		}
		t.Logf("step 6: msg %.150s", msg)
	}
}
