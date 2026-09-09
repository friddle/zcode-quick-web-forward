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
	// rec is the conversation-recovery synthesizer (see officialRecovery).
	rec *officialRecovery
}

// officialRecovery synthesizes the conversation topic frames (snapshot/deltas)
// the phone page's v4 store needs to render a conversation. The official host
// accepts our subscribeConversationV4 (rpc ack ok, mode=snapshot) but never
// pushes the topic frames — on the desktop those flow through the main
// process's relay bridge, which our headless pipe does not implement. We
// recover by calling zcode-session.readSession ourselves after each
// subscribe/resync (and after every sendText, so replies render) and wrapping
// the result into the frame shape the client expects, delivered as an event
// to the page's onDynamicConversationFrame listener.
type officialRecovery struct {
	mu           sync.Mutex
	listenID     int               // EventListen id for onDynamicConversationFrame
	pendingSub   map[int]string    // subscribe/resync call id -> sessionId
	pendingRead  map[int]string    // synthetic readSession call id -> sessionId
	subBySession map[string]string // sessionId -> subscriptionId (from acks)
	epochBySess  map[string]string // sessionId -> logEpoch (from subscribe acks)
	nextID       int               // synthetic promise-call id counter
	wireOrdinal  int               // logicalFrameOrdinal of synthesized wire frames
	lastID       int               // id of the most recently minted synthetic call
	pendingRows  map[int]string    // synthetic rowsRange call id -> sessionId
	snaps        map[string]json.RawMessage
	snapsOrder   []string
}

// mintID allocates a synthetic promise-call id (high range, never clashes
// with the page's own call ids).
func (r *officialRecovery) mintID() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.nextID < 100000 {
		r.nextID = 100000
	}
	r.nextID++
	r.lastID = r.nextID
	return r.lastID
}

// stashSnap remembers a readSession result for the rows-merge step.
func (r *officialRecovery) stashSnap(sid string, raw json.RawMessage) {
	r.mu.Lock()
	if r.snaps == nil {
		r.snaps = map[string]json.RawMessage{}
		r.snapsOrder = []string{}
	}
	r.snaps[sid] = raw
	r.snapsOrder = append(r.snapsOrder, sid)
	if len(r.snapsOrder) > 8 {
		delete(r.snaps, r.snapsOrder[0])
		r.snapsOrder = r.snapsOrder[1:]
	}
	r.mu.Unlock()
}

// snapFor returns the stashed readSession snapshot of a session.
func (r *officialRecovery) snapFor(sid string) json.RawMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snaps[sid]
}

// mergeRowsIntoSnapshot injects the conversation projection rows the client
// renders (state.rows{window,totalCount,firstRowId}) into a readSession
// snapshot, plus the empty pending-commands/queue collections it reconciles.
func mergeRowsIntoSnapshot(snap map[string]any, rowsRes map[string]any) {
	rows := rowsRes
	if inner, ok := rowsRes["rows"].([]any); ok {
		rows = map[string]any{"window": inner}
	} else if inner, ok := rowsRes["rows"].(map[string]any); ok {
		rows = inner
	}
	window, _ := rows["window"].([]any)
	firstRowID := any(nil)
	if len(window) > 0 {
		if m, ok := window[0].(map[string]any); ok {
			firstRowID = m["rowId"]
		}
	}
	rows["totalCount"] = len(window)
	rows["firstRowId"] = firstRowID
	snap["rows"] = rows
	if _, ok := snap["pendingCommands"]; !ok {
		snap["pendingCommands"] = []any{}
	}
	if _, ok := snap["queue"]; !ok {
		snap["queue"] = map[string]any{"items": []any{}}
	}
	if _, ok := snap["optimisticCommands"]; !ok {
		snap["optimisticCommands"] = []any{}
	}
}

func (r *officialRecovery) epochFor(sid string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.epochBySess[sid]
}

func (r *officialRecovery) setEpoch(sid, epoch string) {
	if epoch == "" {
		return
	}
	r.mu.Lock()
	if r.epochBySess == nil {
		r.epochBySess = map[string]string{}
	}
	r.epochBySess[sid] = epoch
	r.mu.Unlock()
}

func (r *officialRecovery) listener() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.listenID
}

func (r *officialRecovery) subFor(sid string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.subBySession[sid]
}

func (r *officialRecovery) setSub(sid, sub string) {
	r.mu.Lock()
	if r.subBySession == nil {
		r.subBySession = map[string]string{}
	}
	r.subBySession[sid] = sub
	r.mu.Unlock()
}

// argMap normalizes a ChannelCall argument into a map.
func argMap(arg any) map[string]any {
	switch a := arg.(type) {
	case map[string]any:
		return a
	case json.RawMessage:
		m := map[string]any{}
		if len(a) > 0 {
			_ = json.Unmarshal(a, &m)
		}
		return m
	case nil:
		return map[string]any{}
	default:
		if b, err := json.Marshal(a); err == nil {
			m := map[string]any{}
			_ = json.Unmarshal(b, &m)
			return m
		}
		return map[string]any{}
	}
}

// trackOfficialRecoveryCall watches the phone's channel calls for the
// conversation listen registration, subscribe/resync calls and sendText —
// the triggers of the synthesized snapshot stream.
func trackOfficialRecoveryCall(c *relay.ChannelCall) {
	officialState.mu.Lock()
	b := officialState.active
	officialState.mu.Unlock()
	if b == nil || b.rec == nil {
		return
	}
	r := b.rec
	switch {
	case c.Kind == relay.KindEventListen && c.ChannelName == "zcode-agent" && c.Name == "onDynamicConversationFrame":
		r.mu.Lock()
		r.listenID = c.ID
		r.mu.Unlock()
		fmt.Println("zcode: recovery: onDynamicConversationFrame listen id", c.ID)
	case c.Kind == relay.KindPromise && (c.Name == "subscribeConversationV4" || c.Name == "resyncConversationV4"):
		sid, _ := argMap(c.Arg)["sessionId"].(string)
		if sid == "" {
			return
		}
		r.mu.Lock()
		if r.pendingSub == nil {
			r.pendingSub = map[int]string{}
		}
		r.pendingSub[c.ID] = sid
		r.mu.Unlock()
		fmt.Println("zcode: recovery: tracking", c.Name, "call id", c.ID, "sid", sid)
	case c.Kind == relay.KindPromise && c.ChannelName == "zcode-agent" && c.Name == "sendConversationCommandV4":
		env, _ := argMap(c.Arg)["envelope"].(map[string]any)
		sid, _ := env["sessionId"].(string)
		typ, _ := env["type"].(string)
		if sid != "" && typ == "sendText" {
			// the turn streams for a while — refresh the snapshot a few times
			// so the assistant reply renders as it lands
			go scheduleRecoverySnapshots(b, sid)
		}
	}
}

// scheduleRecoverySnapshots re-emits the conversation snapshot after a send.
func scheduleRecoverySnapshots(b *officialHostBridge, sid string) {
	for _, d := range []time.Duration{4 * time.Second, 10 * time.Second, 20 * time.Second, 35 * time.Second} {
		time.Sleep(d)
		if !b.h.Alive() {
			return
		}
		requestRecoverySnapshot(b, sid)
	}
}

// requestRecoverySnapshot issues a synthetic zcode-session.readSession call
// over the service port. The reply is matched in inspectOfficialResponse.
func requestRecoverySnapshot(b *officialHostBridge, sid string) {
	r := b.rec
	if r.listener() == 0 {
		return // page has not registered its listener yet
	}
	officialState.mu.Lock()
	ws := officialState.workspace
	officialState.mu.Unlock()
	if ws == "" {
		return
	}
	raw := relay.ChannelCallBytes(&relay.ChannelCall{
		Kind: relay.KindPromise, ID: r.mintID(),
		ChannelName: "zcode-session", Name: "readSession",
		Arg: map[string]any{"workspacePath": ws, "sessionId": sid, "messageLimit": 50},
	})
	r.mu.Lock()
	if r.pendingRead == nil {
		r.pendingRead = map[int]string{}
	}
	r.pendingRead[r.lastID] = sid
	r.mu.Unlock()
	forwardRawToOfficialHost(raw)
}

// requestConversationRows fetches the official transcript rows for a session
// (the same call the desktop renderer makes after applying a base snapshot).
func requestConversationRows(b *officialHostBridge, sid string) {
	r := b.rec
	officialState.mu.Lock()
	ws := officialState.workspace
	officialState.mu.Unlock()
	raw := relay.ChannelCallBytes(&relay.ChannelCall{
		Kind: relay.KindPromise, ID: r.mintID(),
		ChannelName: "zcode-agent", Name: "conversationRowsRangeV4",
		Arg: map[string]any{"workspacePath": ws, "sessionId": sid, "limit": 200},
	})
	r.mu.Lock()
	if r.pendingRows == nil {
		r.pendingRows = map[int]string{}
	}
	r.pendingRows[r.lastID] = sid
	r.mu.Unlock()
	forwardRawToOfficialHost(raw)
}

// inspectOfficialResponse watches host replies for the synthetic readSession
// results and the subscribe acks, and drives the synthesized snapshot stream.
func inspectOfficialResponse(raw []byte) {
	officialState.mu.Lock()
	b := officialState.active
	officialState.mu.Unlock()
	if b == nil || b.rec == nil {
		return
	}
	kind, id, data, ok := relay.ParseChannelResponse(raw)
	if !ok || kind != relay.KindPromiseOK {
		return
	}
	r := b.rec
	r.mu.Lock()
	sid, isSub := r.pendingSub[id]
	if isSub {
		delete(r.pendingSub, id)
	}
	rsid, isRead := r.pendingRead[id]
	if isRead {
		delete(r.pendingRead, id)
	}
	r.mu.Unlock()
	switch {
	case isSub && len(data) > 0:
		var res struct {
			Ack struct {
				SubscriptionID string `json:"subscriptionId"`
				LogEpoch       string `json:"logEpoch"`
			} `json:"ack"`
		}
		if json.Unmarshal(data, &res) == nil && res.Ack.SubscriptionID != "" {
			r.setSub(sid, res.Ack.SubscriptionID)
			r.setEpoch(sid, res.Ack.LogEpoch)
			fmt.Println("zcode: recovery: subscription", res.Ack.SubscriptionID, "epoch", res.Ack.LogEpoch, "sid", sid)
			requestRecoverySnapshot(b, sid)
		}
	case isRead && len(data) > 0:
		// stash the session snapshot, then fetch the official transcript rows
		r.stashSnap(rsid, data)
		requestConversationRows(b, rsid)
	}
	r.mu.Lock()
	rowsid, isRows := r.pendingRows[id]
	if isRows {
		delete(r.pendingRows, id)
	}
	r.mu.Unlock()
	if isRows && len(data) > 0 {
		snapRaw := r.snapFor(rowsid)
		if snapRaw != nil {
			var snap map[string]any
			if err := json.Unmarshal(snapRaw, &snap); err != nil || snap == nil {
				return
			}
			var rowsRes map[string]any
			_ = json.Unmarshal(data, &rowsRes)
			fmt.Printf("zcode: recovery: rowsRange result keys %v head %s\n", keysOf(rowsRes), firstJSON(data))
			mergeRowsIntoSnapshot(snap, rowsRes)
			emitRecoverySnapshot(b, rowsid, snap)
		}
	}
}

// keysOf returns the top-level keys of a decoded JSON object (diagnostics).
func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// firstJSON truncates raw JSON for a log line.
func firstJSON(b json.RawMessage) string {
	s := string(b)
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

// emitRecoverySnapshot wraps a readSession result into the conversation topic
// frame shape (topic/subscriptionId/seq/payload{kind:snapshot,snapshot}) and
// delivers it as the onDynamicConversationFrame event the page listens on.
func emitRecoverySnapshot(b *officialHostBridge, sid string, snap map[string]any) {
	r := b.rec
	listen := r.listener()
	if listen == 0 {
		return
	}
	sub := r.subFor(sid)
	if sub == "" {
		sub = "sub-synth-" + uuidNew()
		r.setSub(sid, sub)
	}
	// protocol echo is not part of the client's snapshot schema (strict)
	delete(snap, "protocol")
	// The client's frame schema demands fromSeq==0 for snapshots, and its
	// strict variant requires snapshot.logEpoch == frame.logEpoch.
	epoch := r.epochFor(sid)
	if epoch != "" {
		snap["logEpoch"] = epoch
	}
	inner := map[string]any{
		"topic":          "conversation/" + sid,
		"subscriptionId": sub,
		"logEpoch":       epoch,
		"fromSeq":        0,
		"toSeq":          0,
		"sentAt":         time.Now().UnixMilli(),
		"payload":        map[string]any{"kind": "snapshot", "snapshot": snap},
	}
	r.mu.Lock()
	r.wireOrdinal++
	ordinal := r.wireOrdinal
	r.mu.Unlock()
	// The phone transport validates a wireVersion-3 envelope (complete or
	// fragment assembly) before the logical frame reaches the store.
	frame := map[string]any{
		"wireVersion":         3,
		"kind":                "complete",
		"deliveryKind":        "continuous",
		"logicalFrameId":      uuidNew(),
		"logicalFrameOrdinal": ordinal,
		"topic":               "conversation/" + sid,
		"subscriptionId":      sub,
		"frame":               inner,
	}
	pb, err := json.Marshal(frame)
	if err != nil {
		return
	}
	out := relay.EventFireBytes(listen, pb)
	fmt.Printf("zcode: recovery: synthesized snapshot frame %d bytes for %s\n", len(out), sid)
	if b.engine != nil && b.engine.HasIdentity() {
		b.engine.SendRawChannelBytes(out, senderSend())
	}
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
	// --stdio is REQUIRED: the desktop spawns `app-server --stdio`; without
	// the flag the engine runs in a mode where the v4 conversation gateway
	// never publishes frames (subscribe acks but no snapshot/stream ever).
	// Engine CWD must be the WORKSPACE (the desktop runs zcode-cli with the
	// opened workspace as cwd): the engine scopes its session store by its
	// project directory, and a runtime-dir cwd makes every hydrate fail with
	// "Session not found" — conversation snapshots come back empty.
	env := officialhost.Env(nodeAbs, []string{script, "app-server", "--stdio"}, workspace,
		filepath.Join(home, ".zcode"), "zcode-quick-web-forward", nil)
	h, err := officialhost.StartEnv(nodeBin, dir, env)
	if err != nil {
		fmt.Printf("zcode: official host start failed: %v — using built-in handlers\n", err)
		return false
	}
	b := &officialHostBridge{h: h, engine: engine, sender: sender, rec: &officialRecovery{}}
	// register EARLY: the host's "local services ready" log (which flips the
	// pipe's ready gate) can fire while this function is still inside
	// WaitReady/attach — the callback resolves the bridge via officialState.
	officialState.mu.Lock()
	officialState.active = b
	officialState.mu.Unlock()
	// markServicesReady flips the pipe gate when the host finishes booting.
	// Host logs arrive via OnParentPort (JSON type:"log"); OnLog only sees
	// raw stderr, but we check both to be safe.
	markServicesReady := func(line string) {
		if officialState.active == nil ||
			!strings.Contains(line, "local services ready, all channels registered") {
			return
		}
		ob := officialState.active
		if ob.ready.Swap(true) {
			return
		}
		fmt.Println("zcode: official-host READY — svc pipe open")
		ob.mu.Lock()
		pending := ob.pendingIn
		ob.pendingIn = nil
		ob.mu.Unlock()
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
	h.OnLog = func(line string) {
		for _, l := range strings.Split(strings.TrimRight(line, "\n"), "\n") {
			if l != "" {
				fmt.Println("zcode: official-host | " + l)
				markServicesReady(l)
			}
		}
	}
	h.OnParentPort = func(msg map[string]any) {
		t, _ := msg["type"].(string)
		if t == "log" {
			// host internal logs ride parentPort JSON — print them or
			// engine-spawn/service failures stay invisible
			raw, err := json.Marshal(msg)
			if err == nil {
				line := string(raw)
				markServicesReady(line)
				maybeAnswerRuntimeHeadersRequest(line)
				if len(line) > 4000 {
					line = line[:4000] + "…"
				}
				fmt.Println("zcode: official-host | " + line)
			}
			return
		}
		fmt.Printf("zcode: official-host parentPort << %s\n", t)
		// full payload for non-log parentPort traffic — these carry the
		// realtime/session-route plumbing we may need to answer
		raw, err := json.Marshal(msg)
		if err == nil {
			line := string(raw)
			if len(line) > 1200 {
				line = line[:1200] + "…"
			}
			fmt.Printf("zcode: official-host parentPort FULL %s\n", line)
		}
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
	// recovery synthesizer bookkeeping must see every call (even broadcast)
	trackOfficialRecoveryCall(c)
	// The phone page sends its channel calls WITHOUT the workspace context —
	// on the desktop the renderer's channel client attaches it automatically.
	// Without it the host's workspace resolvers throw (resolveWorkspaceKey /
	// reading 'workspacePath'), which crashed the host outright for the
	// dynamic-frame listeners and fails every task/session call otherwise.
	// broadcast.onMessage is a true no-arg listen — leave it alone.
	if c.ChannelName != "broadcast" {
		b, _ := json.Marshal(c.Arg)
		out := relay.ChannelCallBytes(c)
		fmt.Printf("zcode: inject ws into %s.%s -> %s | frame %d bytes: %x\n", c.ChannelName, c.Name, string(b), len(out), out[:min(48, len(out))])
		return forwardRawToOfficialHost(out)
		officialState.mu.Lock()
		ws := officialState.workspace
		officialState.mu.Unlock()
		if ws != "" {
			var m map[string]any
			switch a := c.Arg.(type) {
			case map[string]any:
				m = a
			case json.RawMessage:
				m = map[string]any{}
				if len(a) > 0 {
					_ = json.Unmarshal(a, &m) // "null" leaves m nil
				}
				if m == nil {
					m = map[string]any{}
				}
			case nil:
				m = map[string]any{}
			default:
				m = map[string]any{}
				if b, err := json.Marshal(a); err == nil {
					_ = json.Unmarshal(b, &m)
				}
				if m == nil {
					m = map[string]any{}
				}
			}
			if m != nil {
				if _, ok := m["workspacePath"]; !ok {
					m["workspacePath"] = ws
				}
				if _, ok := m["workspaceKey"]; !ok {
					m["workspaceKey"] = ws
				}
				c.Arg = m
			}
		}
	}
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
	inspectOfficialResponse(raw)
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
