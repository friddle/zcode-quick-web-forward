// Official-host integration (the DEFAULT): when
// ~/.zcode/host-official/shim.mjs exists, phone channel traffic is forwarded
// to the DESKTOP APP's official web-remote host (running under Node via
// official-host/shim.mjs) instead of the hand-rolled channel handlers. The
// host spawns the engine itself (ZCODE_AGENT_SERVER_COMMAND), so tasks,
// uploads, automations, file service and the v4 projection are the official
// implementation end to end.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
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
	// svcPort is the CURRENT emulated service-port id. Each phone bridge-open
	// re-attaches with a FRESH attachmentId — the host rejects (or ignores)
	// a re-attach of an already-seen/disposed id, which silently killed the
	// pipe on the second pairing (page reload = stuck at Paired. Loading…).
	svcPort   string
	attachSeq int
	// ready flips when the host logs "local services ready, all channels
	// registered" — its workspace initialization (initializeWorkspace) is
	// async and channel messages that arrive earlier hit an empty registry,
	// where resolveWorkspaceKey(undefined) is an uncaughtException that kills
	// the host. Phone frames are buffered until then.
	ready     atomic.Bool
	pendingIn [][]byte
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
	// register EARLY: the host's "local services ready" log (which flips the
	// pipe's ready gate) can fire while this function is still inside
	// WaitReady/attach — the callback resolves the bridge via officialState.
	officialState.mu.Lock()
	officialState.active = b
	officialState.mu.Unlock()
	h.OnLog = func(line string) {
		for _, l := range strings.Split(strings.TrimRight(line, "\n"), "\n") {
			if l != "" {
				fmt.Println("zcode: official-host | " + l)
			}
			// The host's own boot gate: workspace initialization finished and
			// every channel is registered. Only then is the svc pipe safe.
			if strings.Contains(l, "local services ready, all channels registered") && officialState.active != nil {
				ob := officialState.active
				ob.mu.Lock()
				pending := ob.pendingIn
				ob.pendingIn = nil
				ob.mu.Unlock()
				ob.ready.Store(true)
				fmt.Println("zcode: official-host READY — svc pipe open")
				for _, raw := range pending {
					ob.mu.Lock()
					port := ob.svcPort
					if port == "" {
						port = "svc"
					}
					ob.mu.Unlock()
					ob.h.RawPortData(port, raw)
				}
				if len(pending) > 0 {
					fmt.Printf("zcode: official-host flushed %d buffered phone frames (services ready)\n", len(pending))
				}
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
				maybeAnswerRuntimeHeadersRequest(line)
				if len(line) > 4000 {
					line = line[:4000] + "…"
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
		// seed the host's workspace registry — without it
		// resolveWorkspaceKey throws on the first workspace-scoped subscribe
		"workspacePath": workspace,
	}, "taskport")
	h.PortOpen("taskport")
	// attach-service-port: the renderer-equivalent service port. Phone channel
	// calls go in as raw channel bytes; responses come back the same way.
	// The scope schema is STRICT: exactly {"kind":"local"} — extra keys make
	// the host reject the whole message ("Unrecognized keys") and the svc
	// port never attaches (every phone call forwarded into the void). The
	// workspace identity comes from init-local's workspacePath above.
	h.ParentPort(map[string]any{
		"type":         "attach-service-port",
		"requestId":    uuidNew(),
		"attachmentId": "zqf-svc",
		"clientMode":   "desktop-continuous",
		"scope":        map[string]any{"kind": "local"},
	}, "svc")
	h.PortOpen("svc")

	officialState.mu.Lock()
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
	if !b.ready.Load() {
		// host still initializing its workspaces — hold the frame
		b.mu.Lock()
		b.pendingIn = append(b.pendingIn, raw)
		b.mu.Unlock()
		fmt.Printf("zcode: official-host buffering phone frame %d bytes (services not ready)\n", len(raw))
		return true
	}
	b.mu.Lock()
	port := b.svcPort
	if port == "" {
		port = "svc"
	}
	b.mu.Unlock()
	b.h.RawPortData(port, raw)
	return true
}

// ---------------------------------------------------------------------------
// provider runtime headers auto-answer.
//
// Before the engine's first model call of a turn it asks the host for
// "provider runtime headers" (interaction/requestProviderRuntimeHeaders). The
// host forwards that request to the DESKTOP RENDERER, which answers via
// zcode-agent.respondProviderRuntimeHeaders with the user's auth headers. We
// have no renderer: the request used to dangle forever and the turn stalled
// with "Working…" and no model call ever starting. The engine falls back to
// its own credential chain when the answer says headersApplied=false (the CLI
// proves that chain works), so we synthesize exactly that answer.
//
// The trigger is the host's own log line (it prints requestId + sessionId);
// requestId format: "<sessionId>:provider-runtime-headers:<millis>".
// ---------------------------------------------------------------------------

var runtimeHeadersAnswered sync.Map

// captchaParamFile is where the captcha runner drops a fresh Aliyun verify
// param (single-use, F008). Overridable for tests.
func captchaParamFile() string {
	if p := os.Getenv("ZQF_CAPTCHA_PARAM_FILE"); p != "" {
		return p
	}
	return "/tmp/zqwf-captcha-param.txt"
}

func maybeAnswerRuntimeHeadersRequest(logLine string) {
	const marker = `收到 ZCode provider runtime headers 请求`
	if !strings.Contains(logLine, marker) {
		return
	}
	m := regexp.MustCompile(`requestId\\+":\\"([A-Za-z0-9:_-]+)`).FindStringSubmatch(logLine)
	if m == nil {
		log.Println("zcode: runtime-headers request seen but requestId not parsed")
		return
	}
	requestID := m[1]
	if _, dup := runtimeHeadersAnswered.LoadOrStore(requestID, true); dup {
		return
	}
	sessionID := requestID
	if i := strings.Index(requestID, ":provider-runtime-headers:"); i > 0 {
		sessionID = requestID[:i]
	}
	officialState.mu.Lock()
	ws := officialState.workspace
	officialState.mu.Unlock()
	if ws == "" || sessionID == requestID {
		return
	}
	// NOTE: the request/response keys resolve the workspace from the TOP-LEVEL
	// workspacePath/workspaceIdentity fields (resolveWorkspaceKey of the args
	// object itself) — the desktop renderer passes them flat, not nested under
	// a "workspace" key.
	//
	// The plan gateway (provider_code 3007) REQUIRES the Aliyun captcha header
	// on every model request (F008: each verify param is single-use). The
	// captcha runner (chrome-driverless on 9333 + /tmp/zqwf-run-captcha.py)
	// produces one verify param into ZQF_CAPTCHA_PARAM_FILE; we attach it here
	// and consume the file, so a stale param can never be double-submitted.
	// Without a fresh param we still answer (fast visible failure instead of
	// an infinite "Working…").
	headers := "{}"
	if p := captchaParamFile(); p != "" {
		if b, err := os.ReadFile(p); err == nil && len(bytes.TrimSpace(b)) > 0 {
			param := strings.TrimSpace(string(b))
			headers = fmt.Sprintf(`{"X-Aliyun-Captcha-Verify-Param":%q}`, param)
			_ = os.Remove(p) // F008: single-use — never resubmit
			fmt.Printf("zcode: attaching captcha verify param (%d chars) from %s\n", len(param), p)
		}
	}
	args := fmt.Sprintf(`{"workspacePath":%q,"sessionId":%q,"requestId":%q,"response":{"headersApplied":true,"runtimeProviderHeaders":%s}}`,
		ws, sessionID, requestID, headers)
	frame := buildChannelCall("zcode-agent", "respondProviderRuntimeHeaders", args, 0x4000)
	if forwardRawToOfficialHost(frame) {
		fmt.Printf("zcode: answered provider runtime headers request %s (headersApplied=false)\n", requestID)
	}
}

// appendVQL appends v as a LEB128-style varint (VSCode rpc wire encoding).
func appendVQL(buf []byte, v int) []byte {
	for {
		b := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			buf = append(buf, b|0x80)
		} else {
			return append(buf, b)
		}
	}
}

// buildChannelCall encodes one channel PromiseCall frame:
//
//	[4,4] [6,100] [6,id] [1,len]channel [1,len]method  args
//	args := [4,1] [5,len]json   (one-element array holding the params object —
//	the host's call adapters spread the wire arg positionally)
func buildChannelCall(channel, method, argsJSON string, id int) []byte {
	var buf []byte
	buf = append(buf, 0x04, 0x04, 0x06, 0x64, 0x06)
	buf = appendVQL(buf, id)
	buf = append(buf, 0x01)
	buf = appendVQL(buf, len(channel))
	buf = append(buf, channel...)
	buf = append(buf, 0x01)
	buf = appendVQL(buf, len(method))
	buf = append(buf, method...)
	buf = append(buf, 0x04, 0x01, 0x05)
	buf = appendVQL(buf, len(argsJSON))
	buf = append(buf, argsJSON...)
	return buf
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
	if len(raw) == 0 {
		return
	}
	b.mu.Lock()
	want := b.svcPort
	if want == "" {
		want = "svc"
	}
	b.mu.Unlock()
	if portID != want {
		return // stale port from a previous bridge generation
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

// officialReattach attaches a FRESH service port on the live host. Called on
// every workspace-bridge-open; a repeated attach with the same attachmentId
// does not survive the host's dispose of the previous bridge generation.
func officialReattach() {
	officialState.mu.Lock()
	b := officialState.active
	officialState.mu.Unlock()
	if b == nil || !b.h.Alive() {
		return
	}
	b.mu.Lock()
	b.attachSeq++
	id := fmt.Sprintf("zqf-svc-%d", b.attachSeq)
	b.svcPort = id
	b.mu.Unlock()
	b.h.ParentPort(map[string]any{
		"type":         "attach-service-port",
		"requestId":    uuidNew(),
		"attachmentId": id,
		"clientMode":   "desktop-continuous",
		"scope":        map[string]any{"kind": "local"},
	}, id)
	b.h.PortOpen(id)
	fmt.Printf("zcode: official-host reattached service port %s\n", id)
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
