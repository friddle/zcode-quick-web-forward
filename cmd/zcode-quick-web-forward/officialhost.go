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

	"github.com/friddle/zcode-quick-web-forward/internal/officialhost"
	"github.com/friddle/zcode-quick-web-forward/internal/relay"
)

type officialHostBridge struct {
	h      *officialhost.Host
	engine *relay.BridgeEngine
}

type officialHostState struct {
	mu        sync.Mutex
	active    *officialHostBridge
	nodeBin   string
	script    string
	workspace string
	mid       string
}

var officialState officialHostState

// maybeStartOfficialHost boots the official host when enabled. nodeBin/script
// describe OUR runtime (the host spawns the engine itself via
// ZCODE_AGENT_SERVER_COMMAND); workspace is the engine cwd. Returns false
// when disabled or the bundle is missing.
func maybeStartOfficialHost(engine *relay.BridgeEngine, nodeBin, script, workspace, mid string) bool {
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
	b := &officialHostBridge{h: h, engine: engine}
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
	b.h.RawPortData("svc", relay.ChannelCallBytes(c))
	return true
}

// onPortBytes handles service-port messages from the host: responses are
// already-valid channel messages, so they go to the phone verbatim.
func (b *officialHostBridge) onPortBytes(portID string, raw []byte) {
	if portID != "svc" || len(raw) == 0 {
		return
	}
	b.engine.SendRawChannelBytes(raw, func(v any) {})
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
	officialState.mu.Unlock()
	officialStopHost()
	engine := relay.NewBridgeEngine()
	if maybeStartOfficialHost(engine, node, script, ws, mid) {
		fmt.Println("zcode: official host respawned")
	}
}
