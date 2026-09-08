// CDP façade: the chrome-driverless container exposes its browser only
// through the control-service DevTools bridge (/devtools/targets +
// /devtools/page/<id> WS + /mcp helpers), and Chromium inside binds CDP to
// container-localhost so publishing 9222 reaches nothing. The engine's
// browser fallback instead probes a STANDARD DevTools endpoint on
// 127.0.0.1:9333. ServeCDP bridges the two: it renders the bridge target
// list as /json entries whose webSocketDebuggerUrl points back at the
// façade, and reverse-proxies those WS connections into the bridge.

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// ServeCDP exposes b (a bridge-backed docker browser) as a standard Chrome
// DevTools HTTP+WS endpoint on 127.0.0.1:port. It blocks; run it in a
// goroutine.
func (b *Browser) ServeCDP(port string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/json/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"Browser":"Chrome/chrome-driverless","Protocol-Version":"1.3","User-Agent":"HeadlessChrome (chrome-driverless via zqwf)","V8-Version":"15.1.206.8","WebKit-Version":"537.36","webSocketDebuggerUrl":"ws://127.0.0.1:%s/devtools/browser/parked"}`, port)
	})
	mux.HandleFunc("/json", b.targetsJSON)
	mux.HandleFunc("/json/list", b.targetsJSON)
	mux.HandleFunc("/json/new", func(w http.ResponseWriter, r *http.Request) {
		url := r.URL.Query().Get("url")
		if url == "" {
			url = "about:blank"
		}
		if _, err := b.mcpCall("pw/new_tab", map[string]any{"url": url}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		b.targetsJSON(w, r)
	})
	mux.HandleFunc("/json/close/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/json/activate/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/devtools/page/", func(w http.ResponseWriter, r *http.Request) {
		targetID := strings.TrimPrefix(r.URL.Path, "/devtools/page/")
		if targetID == "" {
			http.NotFound(w, r)
			return
		}
		proxyPageWS(w, r, b.uiPort, targetID)
	})
	// Browser-level sessions are not bridged; hold the socket so a client
	// that dials it doesn't spin, and answer nothing.
	mux.HandleFunc("/devtools/browser/", func(w http.ResponseWriter, r *http.Request) {
		conn, err := facadeUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		conn.Close()
	})
	return http.ListenAndServe("127.0.0.1:"+port, mux)
}

var facadeUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// targetsJSON renders the bridge target list as a standard /json payload.
func (b *Browser) targetsJSON(w http.ResponseWriter, r *http.Request) {
	type bridgeTargets struct {
		Targets []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
			URL   string `json:"url"`
		} `json:"targets"`
		Error string `json:"error"`
	}
	resp, err := http.Get(b.debuggerURL + "/devtools/targets")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	var bt bridgeTargets
	if err := json.NewDecoder(resp.Body).Decode(&bt); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if bt.Error != "" {
		http.Error(w, bt.Error, http.StatusBadGateway)
		return
	}
	out := []map[string]any{}
	for _, t := range bt.Targets {
		out = append(out, map[string]any{
			"description":          "",
			"devtoolsFrontendUrl":  "",
			"id":                   t.ID,
			"title":                t.Title,
			"type":                 "page",
			"url":                  t.URL,
			"webSocketDebuggerUrl": fmt.Sprintf("ws://127.0.0.1:%s/devtools/page/%s", facadePortValue(r), t.ID),
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// facadePortValue recovers the façade port from the request Host header so
// the advertised webSocketDebuggerUrl matches how the client reached us.
func facadePortValue(r *http.Request) string {
	host := r.Host
	if i := strings.LastIndex(host, ":"); i >= 0 {
		return host[i+1:]
	}
	return host
}

// proxyPageWS pipes a façade DevTools WebSocket into the bridge's page
// socket, preserving message types (CDP is text, but events may be binary).
func proxyPageWS(w http.ResponseWriter, r *http.Request, uiPort, targetID string) {
	client, err := facadeUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer client.Close()
	upstream, _, err := websocket.DefaultDialer.Dial(
		fmt.Sprintf("ws://127.0.0.1:%s/devtools/page/%s", uiPort, targetID), nil)
	if err != nil {
		return
	}
	defer upstream.Close()

	done := make(chan struct{}, 2)
	pump := func(dst, src *websocket.Conn) {
		defer func() { done <- struct{}{} }()
		for {
			mt, data, err := src.ReadMessage()
			if err != nil {
				return
			}
			_ = dst.SetWriteDeadline(time.Now().Add(30 * time.Second))
			if err := dst.WriteMessage(mt, data); err != nil {
				return
			}
		}
	}
	go pump(client, upstream) // events & replies: upstream -> client
	go pump(upstream, client) // commands: client -> upstream
	<-done
}
