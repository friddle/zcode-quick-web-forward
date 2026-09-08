// Official-host integration (the DEFAULT): when
// ~/.zcode/host-official/shim.mjs exists, phone channel traffic is forwarded
// to the DESKTOP APP's official web-remote host (running under Node via
// official-host/shim.mjs) instead of the hand-rolled channel handlers. The
// host spawns the engine itself (ZCODE_AGENT_SERVER_COMMAND), so tasks,
// uploads, automations, file service and the v4 projection are the official
// implementation end to end.

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
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
	sender *relaySender // routes to the phone's latest pending reply
	mu     sync.Mutex
	// pendingOut holds host->phone bytes that arrived before the phone
	// opened its workspace bridge (no rpc-frame identity yet — the framed
	// send would silently drop them). Flushed on bridge-open.
	pendingOut [][]byte
	// answered records promise-call ids the host replied to, so the bridge
	// can fall back to the built-in handlers for calls the host ignores.
	answered map[int]bool
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
	if os.Getenv("ZCODE_OFFICIAL_HOST") == "0" {
		return false // explicit opt-out
	}
	// Already running? Reuse it — respawning on every relay reconnect kills
	// the host (and its engine child) out from under the phone, which reloads
	// the frontend and immediately re-opens the bridge: a restart loop.
	officialState.mu.Lock()
	if b := officialState.active; b != nil && b.h.Alive() {
		officialState.engine, officialState.sender = engine, sender
		officialState.mu.Unlock()
		b.mu.Lock()
		b.engine, b.sender = engine, sender
		b.mu.Unlock()
		fmt.Println("zcode: official host already running — reusing")
		officialFlushOut()
		return true
	}
	officialState.mu.Unlock()
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	dir := filepath.Join(home, ".zcode", "host-official")
	if _, err := os.Stat(filepath.Join(dir, "shim.mjs")); err != nil {
		fmt.Println("zcode: official host bundle missing (~/.zcode/host-official/shim.mjs) — using built-in handlers")
		return false
	}
	// The host spawns the engine ITSELF, so it needs the exact command we
	// would have run (absolute node + runtime zcode.cjs app-server). Without
	// ZCODE_AGENT_SERVER_COMMAND* the host falls back to the desktop's
	// default engine command, its spawn fails, and every engine-backed
	// channel hangs — the "intermittent serving" symptom.
	nodeAbs, lerr := exec.LookPath(nodeBin)
	if lerr != nil {
		nodeAbs = nodeBin
	}
	env := officialhost.Env(nodeAbs, []string{script, "app-server"}, filepath.Dir(script),
		filepath.Join(home, ".zcode"), "zcode-quick-web-forward", nil)
	h, err := officialhost.StartEnv(nodeBin, dir, env)
	if err != nil {
		fmt.Printf("zcode: official host start failed: %v — using built-in handlers\n", err)
		return false
	}
	b := &officialHostBridge{h: h, engine: engine, sender: sender}
	h.OnLog = func(line string) {
		for _, l := range strings.Split(strings.TrimRight(line, "\n"), "\n") {
			if l != "" {
				fmt.Println("zcode: official-host | " + l)
			}
		}
	}
	h.OnParentPort = func(msg map[string]any) {
		t, _ := msg["type"].(string)
		if t == "log" {
			// The host pipes its internal log lines as parentPort messages —
			// print the payload or engine-spawn/service failures stay invisible.
			if raw, err := json.Marshal(msg); err == nil && len(raw) > 0 {
				line := string(raw)
				if len(line) > 400 {
					line = line[:400] + "…"
				}
				fmt.Println("zcode: official-host | " + line)
			}
			return
		}
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
	// IMPORTANT: never hand the host OUR deviceMid — its realtime bridge
	// connects to the relay under that identity, the relay sees two desktop
	// sessions with one mid and evicts them in turn, and the phone's
	// connection drops every ~60s (every UI view resets). Use a distinct id.
	h.ParentPort(map[string]any{
		"type":                  "init-local",
		"hostId":                "zqf-" + strings.ReplaceAll(hostname(), " ", "-"),
		"deliveryKind":          "desktop_window",
		"deviceMid":             "zqf-host-" + uuidNew(),
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
	officialState.engine = engine
	officialState.sender = sender
	officialState.mu.Unlock()
	fmt.Printf("zcode: OFFICIAL host active (%s) — channel traffic forwarded to the official implementation\n", dir)
	return true
}

// hostForwardEnabled gates the host↔phone pipe. The host answers only part
// of the phone's traffic and emits its own channel bytes; until the serving
// gaps are closed, piping them degrades the phone's bridge into the 2s
// recover loop. Default OFF: the host runs and stays warm, the phone rides
// the proven built-in pipeline. ZCODE_HOST_FORWARD=1 re-enables the pipe.
func hostForwardEnabled() bool {
	// main is OFFICIAL-ONLY: the host->phone return pipe is on by default
	// (it was env-gated off and every host response was dropped at the
	// onPortBytes entry — the phone re-bootstrapped forever).
	return os.Getenv("ZCODE_HOST_FORWARD") != "0"
}

// officialHostActive reports whether channel traffic should be forwarded to
// the official host instead of the built-in handlers. Requires the host to
// be alive AND the pipe enabled (ZCODE_HOST_FORWARD=1).
func officialHostActive() bool {
	if !hostForwardEnabled() {
		return false
	}
	officialState.mu.Lock()
	b := officialState.active
	officialState.mu.Unlock()
	return b != nil && b.h.Alive()
}

// forwardCallToOfficialHost pipes one decoded phone channel call to the
// host's service port.
func forwardCallToOfficialHost(c *relay.ChannelCall) bool {
	return forwardRawToOfficialHost(relay.ChannelCallBytes(c))
}

// forwardRawToOfficialHost pipes raw channel bytes (calls, initialize acks,
// any client message) to the host's service port verbatim.
func forwardRawToOfficialHost(raw []byte) bool {
	officialState.mu.Lock()
	b := officialState.active
	officialState.mu.Unlock()
	if b == nil || !b.h.Alive() {
		return false
	}
	b.h.RawPortData("svc", raw)
	return true
}

// channelPromiseID extracts the call id from a PromiseSuccess channel
// message: [0x04 array][len=2][0x06 kind=201][0x06 id][data]. Returns false
// for any other message kind.
func channelPromiseID(b []byte) (int, bool) {
	if len(b) < 7 || b[0] != 4 || b[1] != 2 || b[2] != 6 {
		return 0, false
	}
	kind, next, ok := leb128(b, 3)
	if !ok || kind != 201 || next >= len(b) || b[next] != 6 {
		return 0, false
	}
	id, _, ok := leb128(b, next+1)
	return id, ok
}

// leb128 reads one LEB128 varint at off; returns the value, the offset just
// past it, and whether a complete varint was present.
func leb128(b []byte, off int) (int, int, bool) {
	id, shift := 0, uint(0)
	for i := off; i < len(b) && i < off+5; i++ {
		v := b[i]
		id |= int(v&0x7f) << shift
		if v&0x80 == 0 {
			return id, i + 1, true
		}
		shift += 7
	}
	return 0, off, false
}

// onPortBytes pipes host service-port bytes back to the phone verbatim —
// responses AND the host's channel initialize; the phone's channel stack
// speaks the same protocol, so the bridge stays a pure pipe.
func (b *officialHostBridge) onPortBytes(portID string, raw []byte) {
	if portID != "svc" || len(raw) == 0 {
		return
	}
	fmt.Printf("zcode: official-host <- svc %d bytes\n", len(raw))
	if !hostForwardEnabled() {
		return // pipe disabled — log only, don't leak host bytes to the phone
	}
	if id, ok := channelPromiseID(raw); ok {
		b.mu.Lock()
		if b.answered == nil {
			b.answered = map[int]bool{}
		}
		b.answered[id] = true
		b.mu.Unlock()
	}
	if b.engine == nil || !b.engine.HasIdentity() {
		// The host speaks before the phone's bridge exists (its channel
		// initialize arrives at attach time). Buffer until bridge-open.
		b.mu.Lock()
		b.pendingOut = append(b.pendingOut, raw)
		b.mu.Unlock()
		return
	}
	// Route through the CURRENT relay sender. An empty send func here
	// silently swallowed every host response — the phone waited forever on
	// its channel calls and re-bootstrapped in a loop (Paired. Loading
	// workspace… with RPCs answered host-side but nothing arriving).
	b.engine.SendRawChannelBytes(raw, senderSend())
}

// senderSend returns a routing func that always targets the relay connection
// current at call time.
func senderSend() func(any) {
	return func(v any) {
		officialState.mu.Lock()
		s := officialState.sender
		officialState.mu.Unlock()
		if s != nil {
			s.send(v)
		}
	}
}

// officialCallAnswered reports whether the host replied to a promise call.
func officialCallAnswered(id int) bool {
	officialState.mu.Lock()
	b := officialState.active
	officialState.mu.Unlock()
	if b == nil {
		return true // not active — never fall back
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.answered[id]
}

// officialFlushOut sends buffered host bytes now that the phone bridge
// identity exists. Called from the workspace-bridge-open handler.
func officialFlushOut() {
	officialState.mu.Lock()
	b := officialState.active
	officialState.mu.Unlock()
	if b == nil {
		return
	}
	b.mu.Lock()
	pending := b.pendingOut
	b.pendingOut = nil
	b.mu.Unlock()
	for _, raw := range pending {
		if b.engine == nil {
			break
		}
		fmt.Printf("zcode: official-host flushing buffered %d bytes\n", len(raw))
		b.engine.SendRawChannelBytes(raw, senderSend())
	}
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
