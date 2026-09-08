package browser

import (
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestWSProbeManual dials a manually-started chromium's page target and sends
// Page.enable. Set ZQF_WS_PROBE_PORT to run (keeps CI/hermetic tests clean).
func TestWSProbeManual(t *testing.T) {
	port := os.Getenv("ZQF_WS_PROBE_PORT")
	if port == "" {
		t.Skip("ZQF_WS_PROBE_PORT not set")
	}
	// pick the first page target
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
		t.Fatalf("no page target in %d entries", len(list))
	}
	t.Logf("dialing %s", page)
	d := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	conn, _, err := d.Dial(page, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := conn.WriteJSON(map[string]any{"id": 1, "method": "Page.enable"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	t.Logf("reply: %.150s", msg)
}
