package browser

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestGoSpawnManual replicates launchOnPort (spawn chromium as a child of the
// test process) then drives the page target with gorilla — isolating
// Go-parented chromium from Go-client behavior. Set ZQF_WS_PROBE_SPAWN=1.
func TestGoSpawnManual(t *testing.T) {
	if os.Getenv("ZQF_WS_PROBE_SPAWN") == "" {
		t.Skip("ZQF_WS_PROBE_SPAWN not set")
	}
	chrome := FindChromium()
	if chrome == "" {
		t.Skip("no chromium")
	}
	userData, err := os.MkdirTemp("", "zqf-chromium-")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(chrome,
		"--headless=new", "--no-sandbox", "--disable-gpu", "--disable-dev-shm-usage",
		"--disable-background-networking",
		"--remote-debugging-port=9335", "--remote-debugging-address=127.0.0.1",
		"--user-data-dir="+userData, "about:blank")
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
	}()

	var wsURL string
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://127.0.0.1:9335/json")
		if err == nil {
			var list []struct {
				Type  string `json:"type"`
				URL   string `json:"url"`
				WsURL string `json:"webSocketDebuggerUrl"`
			}
			err = json.NewDecoder(resp.Body).Decode(&list)
			resp.Body.Close()
			if err == nil {
				for _, tgt := range list {
					if tgt.Type == "page" && tgt.WsURL != "" {
						wsURL = tgt.WsURL
					}
				}
				if wsURL != "" {
					break
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if wsURL == "" {
		t.Fatal("no page target appeared on 9335")
	}
	t.Logf("daling %s", wsURL)
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(6 * time.Second))
	if err := conn.WriteJSON(map[string]any{"id": 1, "method": "Page.enable"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read enable: %v", err)
	}
	t.Logf("enable: %.80s", msg)
	if err := conn.WriteJSON(map[string]any{"id": 2, "method": "Page.navigate", "params": map[string]any{"url": "http://example.com"}}); err != nil {
		t.Fatalf("write2: %v", err)
	}
	for i := 0; i < 3; i++ {
		_, msg, err = conn.ReadMessage()
		if err != nil {
			t.Fatalf("read navigate: %v", err)
		}
		t.Logf("msg: %.120s", msg)
	}
	t.Log("ALL OK")
}
