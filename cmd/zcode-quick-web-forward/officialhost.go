// Official-host integration: when ZCODE_OFFICIAL_HOST=1 and
// ~/.zcode/host-official/shim.mjs exists, phone channel traffic is forwarded
// to the DESKTOP APP's official web-remote host (running under Node via
// official-host/shim.mjs) instead of the hand-rolled channel handlers. The
// host spawns the engine itself (ZCODE_AGENT_SERVER_COMMAND), so tasks,
// uploads, automations, file service and the v4 projection are the official
// implementation end to end.

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/friddle/zcode-quick-web-forward/internal/officialhost"
	"github.com/friddle/zcode-quick-web-forward/internal/relay"
)

type officialHostBridge struct {
	h      *officialhost.Host
	engine *relay.BridgeEngine
	send   func(any) // routes to the phone's latest pending reply
}

type officialHostState struct {
	mu        sync.Mutex
	active    *officialHostBridge
	nodeBin   string
	script    string
	workspace string
	mid       string
	engine    *relay.BridgeEngine
	sender    *relaySender
}

var officialState officialHostState

// maybeStartOfficialHost boots the official host when enabled. nodeBin/script
// describe OUR runtime (the host spawns the engine itself via
// ZCODE_AGENT_SERVER_COMMAND); workspace is the engine cwd. Returns false
// when disabled or the bundle is missing.
func maybeStartOfficialHost(engine *relay.BridgeEngine, sender *relaySender, nodeBin, script, workspace, mid string) bool {
	if os.Getenv("ZCODE_OFFICIAL_HOST") == "" {
		return false
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	dir := filepath.Join(home, ".zcode", "host-official")
	if _, err := os.Stat(filepath.Join(dir, "shim.mjs")); err != nil {
		fmt.Println("zcode: ZCODE_OFFICIAL_HOST set but ~/.zcode/host-official/shim.mjs missing — using built-in handlers")
		return false
	}
	h, err := officialhost.Start(nodeBin, dir)
	if err != nil {
		fmt.Printf("zcode: official host start failed: %v — using built-in handlers\n", err)
		return false
	}
	b := &officialHostBridge{h: h, engine: engine, send: sender.send}
	h.OnLog = func(line string) {
		for _, l := range strings.Split(strings.TrimRight(line, "\n"), "\n") {
			if l != "" {
				fmt.Println("zcode: official-host | " + l)
			}
		}
	}
	h.OnParentPort = func(msg map[string]any) {
		t, _ := msg["type"].(string)
		fmt.Printf("zcode: official-host parentPort << %s\n", t)
	}
	h.OnRawPortData = b.onPortBytes

	// The host's parentPort listener only exists after its import completes;
	// messages sent earlier are silently dropped by the EventEmitter.
	if !h.WaitReady(20 * time.Second) {
		fmt.Println("zcode: official host did not report ready in 20s — using built-in handlers")
		return false
	}

	// init-local: the task-realtime bridge attaches its port first.
	h.ParentPort(map[string]any{
		"type":                  "init-local",
		"hostId":                "zqf-" + strings.ReplaceAll(hostname(), " ", "-"),
		"deliveryKind":          "desktop_window",
		"deviceMid":             mid,
		"agentSpawnFallbackCwd": workspace,
	}, "taskport")
	h.PortOpen("taskport")
	// attach-service-port: the renderer-equivalent service port. Phone channel
	// calls go in as raw channel bytes; responses come back the same way.
	h.ParentPort(map[string]any{
		"type":         "attach-service-port",
		"requestId":    uuidNew(),
		"attachmentId": "zqf-svc",
		"clientMode":   "desktop-continuous",
		"scope":        map[string]any{"kind": "local"},
	}, "svc")
	h.PortOpen("svc")

	officialState.mu.Lock()
	officialState.active = b
	officialState.nodeBin = nodeBin
	officialState.script = script
	officialState.workspace = workspace
	officialState.mid = mid
	officialState.mu.Unlock()
	fmt.Printf("zcode: OFFICIAL host active (%s) — channel traffic forwarded to the official implementation\n", dir)
	return true
}

// officialHostActive reports whether channel traffic should be forwarded to
// the official host instead of the built-in handlers.
func officialHostActive() bool {
	officialState.mu.Lock()
	b := officialState.active
	officialState.mu.Unlock()
	return b != nil && b.h.Alive()
}

// forwardCallToOfficialHost pipes one decoded phone channel call to the
// host's service port (raw channel bytes — the same wire form the desktop
// renderer writes into its port).
func forwardCallToOfficialHost(c *relay.ChannelCall) bool {
	officialState.mu.Lock()
	b := officialState.active
	officialState.mu.Unlock()
	if b == nil || !b.h.Alive() {
		return false
	}
	fmt.Printf("zcode: official-host -> svc %s/%s kind=%d id=%d\n", c.ChannelName, c.Name, c.Kind, c.ID)
	b.h.RawPortData("svc", relay.ChannelCallBytes(c))
	return true
}

// onPortBytes handles service-port messages from the host.
func (b *officialHostBridge) onPortBytes(portID string, raw []byte) {
	if portID != "svc" || len(raw) == 0 {
		return
	}
	if relay.IsChannelInitialize(raw) {
		// Server initialize: answer with the client initialize, otherwise
		// the host serves nothing on this port.
		b.h.RawPortData("svc", relay.InitializeMessage())
		fmt.Println("zcode: official-host <- svc [200] server init; client initialize sent")
		return
	}
	if res, ok := relay.DecodeChannelResponse(raw); ok {
		if b.send != nil {
			b.send(res)
		}
		return
	}
	fmt.Printf("zcode: official-host <- svc unhandled %d bytes: % x\n", len(raw), raw[:min(24, len(raw))])
}

// officialStopHost tears the host down (its engine child dies with it).
func officialStopHost() {
	officialState.mu.Lock()
	b := officialState.active
	officialState.active = nil
	officialState.mu.Unlock()
	if b != nil {
		b.h.Stop()
		fmt.Println("zcode: official host stopped")
	}
}

// officialRestartEngine rebuilds the whole host with the stored engine
// command — used as restartEngine in official-host mode (workspace switch).
func officialRestartEngine() {
	officialState.mu.Lock()
	node, script, ws, mid := officialState.nodeBin, officialState.script, officialState.workspace, officialState.mid
	engine, sender := officialState.engine, officialState.sender
	officialState.mu.Unlock()
	officialStopHost()
	engine = relay.NewBridgeEngine()
	if maybeStartOfficialHost(engine, sender, node, script, ws, mid) {
		fmt.Println("zcode: official host respawned")
	}
}
