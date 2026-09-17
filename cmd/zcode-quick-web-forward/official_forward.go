package main

import (
	"encoding/json"
	"fmt"
	"github.com/friddle/zcode-quick-web-forward/internal/officialhost"
	"github.com/friddle/zcode-quick-web-forward/internal/relay"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// verboseLogs mirrors ZQF_RELAY_DEBUG/ZQF_DEBUG: full frame dumps, per-call
// inject dumps, rows payloads and host info-logs. Default (off) keeps only
// lifecycle, errors and rescue events.
var verboseLogs = os.Getenv("ZQF_RELAY_DEBUG") != "" || os.Getenv("ZQF_DEBUG") != ""

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
	rec := &officialRecovery{}
	rec.initMaps()
	b := &officialHostBridge{h: h, engine: engine, sender: sender, rec: rec}
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
			if l == "" {
				continue
			}
			markServicesReady(l)
			// host info spam (readSession polls etc.) only matters in debug;
			// warnings/errors/panics always surface.
			if verboseLogs || strings.Contains(l, `"level":"warn"`) ||
				strings.Contains(l, `"level":"error"`) || strings.Contains(l, "panic") {
				fmt.Println("zcode: official-host | " + l)
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
				if verboseLogs || strings.Contains(line, `"level":"warn"`) || strings.Contains(line, `"level":"error"`) {
					fmt.Println("zcode: official-host | " + line)
				}
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
			if verboseLogs {
				fmt.Printf("zcode: official-host parentPort FULL %s\n", line)
			}
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
	loadPersistedPageClient()
	watchPendingSendFile()
	watchPendingCreateFile()
	startTurnWatchdog(b)
	startMutexCanary()
	fmt.Printf("zcode: OFFICIAL host active (%s) — channel traffic forwarded to the official implementation\n", dir)
	return true
}

// watchPendingSendFile polls a small outbox file and submits its content as
// a STANDARD sendConversationCommandV4 (type from line 2: "sendText"/"stop",
// text from line 3+) through the regular forward path. Format:
//
//	line 1: sessionId
//	line 2: command type (sendText | stop)
//	line 3+: payload text (sendText)
//
// Exists for headless recovery only — a wedged agent runtime (turn killed by
// AiSdkModelAdapterError leaves the runtime queueing every later send behind
// it) is cleared with the standard stop command, and a message captured
// before a restart is resubmitted the same way the page would. The file is
// consumed on read.
func watchPendingSendFile() {
	path := os.Getenv("ZQF_PENDING_SEND")
	if path == "" {
		// Next to the relay state (webremote-state.json) on every deployment;
		// the historic /root/data path was CP-installation specific.
		if cache, err := os.UserCacheDir(); err == nil {
			path = filepath.Join(cache, "zcode-quick-web-forward", "pending-send.txt")
		} else {
			path = "zqf-pending-send.txt"
		}
	}
	submit := func() {
		b, err := os.ReadFile(path)
		if err != nil {
			return
		}
		_ = os.Remove(path)
		lines := strings.SplitN(string(b), "\n", 3)
		if len(lines) < 2 || strings.TrimSpace(lines[0]) == "" {
			return
		}
		sid := strings.TrimSpace(lines[0])
		typ := strings.TrimSpace(lines[1])
		text := ""
		if len(lines) == 3 {
			text = strings.TrimRight(lines[2], "\n")
		}
		// Two-line form (sid + text) means "deliver this message" — line 2
		// is the TEXT, not a type. The strict 3-line form stays canonical,
		// but a hand-written 2-line file must not be swallowed silently.
		if typ != "sendText" && typ != "stop" && len(lines) == 2 {
			text = lines[1]
			typ = "sendText"
		}
		if typ == "" {
			typ = "sendText"
		}
		if typ == "mode" {
			// `mode` flips the session's collaboration mode (build/edit/
			// plan/yolo) — the page's own switchCollaborationMode never
			// reaches the engine behind the composer's send gate, so the
			// outbox is the reliable path. `text` carries the mode name.
			mode := strings.TrimSpace(text)
			if mode == "" {
				return
			}
			typ = "switchCollaborationMode"
			text = mode
		}
		if typ != "stop" && typ != "switchCollaborationMode" && strings.TrimSpace(text) == "" {
			return
		}
		go func() {
			// Give the host's service port a moment after a fresh boot; the
			// forward path buffers until ready anyway.
			time.Sleep(3 * time.Second)
			payload := map[string]any{}
			if typ == "sendText" {
				payload["text"] = text
			}
			if typ == "switchCollaborationMode" {
				payload["mode"] = strings.TrimSpace(text)
			}
			fmt.Printf("zcode: recovery: outbox submit %s (%d chars) to %s\n", typ, len(text), sid)
			officialInjectCommand(sid, typ, payload, "")
		}()
	}
	go func() {
		for {
			submit()
			time.Sleep(2 * time.Second)
		}
	}()
}

// watchPendingCreateFile mirrors the outbox for NEW sessions: line 1 is the
// workspace path, line 2+ the first user message. Exists because the
// web-remote composer gates NEW-task sends behind the desktop coding-plan
// account state (model picker disabled = send disabled); a headless operator
// submits a task by dropping this file instead of driving the gated UI.
func watchPendingCreateFile() {
	path := os.Getenv("ZQF_PENDING_CREATE")
	if path == "" {
		if cache, err := os.UserCacheDir(); err == nil {
			path = filepath.Join(cache, "zcode-quick-web-forward", "pending-create.txt")
		} else {
			path = "zqf-pending-create.txt"
		}
	}
	submit := func() {
		b, err := os.ReadFile(path)
		if err != nil {
			return
		}
		_ = os.Remove(path)
		lines := strings.SplitN(string(b), "\n", 2)
		ws := strings.TrimSpace(lines[0])
		text := ""
		if len(lines) == 2 {
			text = strings.TrimRight(lines[1], "\n")
		}
		if ws == "" || strings.TrimSpace(text) == "" {
			return
		}
		// Optional first line of the body: `mode=<build|edit|plan|yolo>`
		// seeds the session's collaboration mode (firstInput.mode) so
		// headless tasks don't stall on permission prompts. Default comes
		// from ZQF_CREATE_MODE (unset = engine default).
		mode := os.Getenv("ZQF_CREATE_MODE")
		if strings.HasPrefix(text, "mode=") {
			rest := text
			if i := strings.IndexByte(rest, '\n'); i >= 0 {
				mode = strings.TrimSpace(rest[len("mode="):i])
				text = strings.TrimLeft(rest[i+1:], "\n")
			} else {
				mode = strings.TrimSpace(rest[len("mode="):])
				text = ""
			}
		}
		if strings.TrimSpace(text) == "" {
			return
		}
		go func() {
			time.Sleep(3 * time.Second)
			officialInjectCreateSession(ws, text, mode)
		}()
	}
	go func() {
		for {
			submit()
			time.Sleep(2 * time.Second)
		}
	}()
}

// officialInjectCreateSession injects the page's createSession flow (a
// sendConversationCommandV4 envelope with type=createSession) using the
// page's persisted client identity, so the minted session belongs to the
// same client the phone talks as.
func officialInjectCreateSession(workspace, text, mode string) {
	officialState.mu.Lock()
	b := officialState.active
	clientID := officialState.persistedClientID
	officialState.mu.Unlock()
	if b == nil {
		fmt.Println("zcode: recovery: create-session inject skipped (no host)")
		return
	}
	if clientID == "" {
		if rec := b.rec; rec != nil {
			rec.mu.Lock()
			for _, c := range rec.clientBySession {
				if c != "" {
					clientID = c
					break
				}
			}
			rec.mu.Unlock()
		}
	}
	if clientID == "" {
		clientID = "client-" + uuidNew()
	}
	first := map[string]any{"text": text}
	if mode != "" {
		first["mode"] = mode
	}
	env := map[string]any{
		"commandId": uuidNew(),
		"clientId":  clientID,
		// Nullable but REQUIRED key — omitting it makes the engine's strict
		// schema drop the whole command silently.
		"sessionId": nil,
		"type":      "createSession",
		"payload": map[string]any{
			"workspaceId": workspace,
			"firstInput":  first,
		},
		"issuedAt": time.Now().UnixMilli(),
	}
	arg := map[string]any{"workspacePath": workspace, "envelope": env}
	c := &relay.ChannelCall{Kind: relay.KindPromise, ID: b.rec.mintID(),
		ChannelName: "zcode-agent", Name: "sendConversationCommandV4", Arg: arg}
	fmt.Printf("zcode: recovery: injecting createSession for %s (%d chars)\n", workspace, len(text))
	forwardCallToOfficialHost(c)
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
	// The phone page registers its onDynamic* EventListens with args
	// [undefined] — no workspace context. The host then routes the listener
	// to an empty workspace target and NO conversation/workspace frames are
	// ever delivered to it (desktop renderers attach the workspace
	// automatically). Fill in the workspace for workspace-scoped listens.
	if c.Kind == relay.KindEventListen &&
		(c.ChannelName == "zcode-agent" || c.ChannelName == "zcode-task") &&
		strings.HasPrefix(c.Name, "onDynamic") {
		m := argMap(c.Arg)
		if m["workspacePath"] == nil || m["workspacePath"] == "" {
			officialState.mu.Lock()
			ws := officialState.workspace
			officialState.mu.Unlock()
			if ws != "" {
				m["workspacePath"] = ws
				if m["workspaceKey"] == nil || m["workspaceKey"] == "" {
					m["workspaceKey"] = ws
				}
				c.Arg = m
				fmt.Printf("zcode: recovery: injected ws into listen %s.%s (id %d)\n", c.ChannelName, c.Name, c.ID)
			}
		}
	}
	// The page registers terminal.onDynamic* listens BEFORE terminal.create
	// (args without an id) — the host's getTerminal throws on the missing id
	// and the throw is an uncaughtException that kills the whole host. Cache
	// those listens and forward them once a create reply supplies the id.
	if c.Kind == relay.KindEventListen && c.ChannelName == "terminal" &&
		strings.HasPrefix(c.Name, "onDynamic") {
		officialState.mu.Lock()
		b := officialState.active
		officialState.mu.Unlock()
		if b != nil && b.rec != nil {
			b.rec.cacheTerminalListen(c)
			fmt.Println("zcode: recovery: cached terminal listen", c.Name, "(waiting for create id)")
		}
		return true
	}
	// listArchivedTasks interception removed: the host now reads the shared
	// tasks-index.sqlite itself (including archived rows), and the daemon's
	// item shape didn't match the page's task-row schema — the 归档 panel
	// stayed empty ("暂无归档任务") with the synthesized answer.
	{
		// Revision-sensitive conversation commands (forkAssistant,
		// setAssistantFeedback, retryTurn, editUserQuery, applyFileRewind)
		// are rejected by the engine as proto.staleRevision because the page
		// computes baseRevision from our synthesized snapshot, which reports
		// revision 0 forever. Inject the real revision learned from the
		// engine's stale responses.
		if c.Kind == relay.KindPromise && c.ChannelName == "zcode-agent" && c.Name == "sendConversationCommandV4" {
			if rec := officialActiveRec(); rec != nil {
				if env, ok := argMap(c.Arg)["envelope"].(map[string]any); ok {
					sid, _ := env["sessionId"].(string)
					typ, _ := env["type"].(string)
					// The phone transport re-delivers conversation commands (new
					// call id, SAME commandId). The engine queues each delivery
					// as its own queue entry — a re-delivered mid-turn sendText
					// then dispatched twice, and a deleteQueueItem only removed
					// one copy (taken-back messages still executed). Forward
					// each envelope exactly once; the page's ack handling is
					// idempotent so re-deliveries need no answer of their own.
					if cid, _ := env["commandId"].(string); cid != "" && sid != "" {
						key := sid + "|" + typ + "|" + cid
						rec.mu.Lock()
						nowMs := time.Now().UnixMilli()
						first := rec.seenCommand[key] == 0
						if first {
							rec.seenCommand[key] = nowMs
						}
						if len(rec.seenCommand) > 4096 {
							for k, ts := range rec.seenCommand {
								if nowMs-ts > 300_000 {
									delete(rec.seenCommand, k)
								}
							}
						}
						rec.mu.Unlock()
						if !first {
							fmt.Printf("zcode: recovery: dropped re-delivered %s command %s for %s\n", typ, cid, sid)
							return true
						}
					}
					// A turn that dies while items sit in its queue leaves the
					// session's queue HELD; the engine then rejects every
					// further send with "held queue requires
					// heldQueueDisposition (clearQueueAndSend |
					// keepQueueAndSend)" — the phone page has no dialog for
					// that state, so each send just pended 30s and vanished.
					// The phone's newest send expresses "proceed now": answer
					// the engine's standard question with clearQueueAndSend
					// (the held items are the user's own stale followups;
					// resubmission keeps the newest text).
					if typ == "sendText" {
						if _, has := env["heldQueueDisposition"]; !has {
							env["heldQueueDisposition"] = "clearQueueAndSend"
							if m, ok := c.Arg.(map[string]any); ok {
								m["envelope"] = env
							} else {
								c.Arg = argMap(c.Arg)
							}
							fmt.Println("zcode: recovery: set heldQueueDisposition=clearQueueAndSend")
						}
					}
					// The stop guard aborts with fault.guard.stopTargetChanged
					// when expectedForegroundExecutionId doesn't equal the
					// engine's live one (runtime_command_<counter>, unknowable
					// to our synthesized snapshot — it used to advertise
					// runtime_command_<sessionId>, so every stop button press
					// was rejected). The field is optional: absent means "stop
					// whatever is running", which is what the phone means.
					if typ == "stop" {
						if pl, ok := env["payload"].(map[string]any); ok {
							if _, has := pl["expectedForegroundExecutionId"]; has {
								delete(pl, "expectedForegroundExecutionId")
								if m, ok := c.Arg.(map[string]any); ok {
									m["envelope"] = env
								} else {
									c.Arg = argMap(c.Arg)
								}
								fmt.Println("zcode: recovery: stripped expectedForegroundExecutionId from stop")
							}
						}
					}
					if br, _ := env["baseRevision"].(float64); br == 0 && sid != "" {
						rec.mu.Lock()
						rev := rec.sessionRevision[sid]
						rec.mu.Unlock()
						if rev > 0 {
							env["baseRevision"] = float64(rev)
							if m, ok := c.Arg.(map[string]any); ok {
								m["envelope"] = env
							} else {
								c.Arg = argMap(c.Arg)
							}
							fmt.Printf("zcode: recovery: injected baseRevision %d for %s\n", rev, sid)
						}
					}
					// The edit-retry/rewind family (editUserQuery, retryTurn,
					// applyFileRewind, forkAssistant, setAssistantFeedback)
					// additionally requires an EXACT baseLogEpoch — the engine
					// rejects a stale one with proto.staleLogEpoch before even
					// looking at baseRevision. Repair an epoch the page took
					// from an early snapshot (advertised "0" before the
					// subscribe ack taught us the real one). Only touch the
					// field when the page itself sent it: the envelope schemas
					// of other command types don't carry it.
					switch typ {
					case "editUserQuery", "retryTurn", "applyFileRewind", "forkAssistant", "setAssistantFeedback":
						if learned := rec.epochFor(sid); learned != "" {
							if ble, has := env["baseLogEpoch"].(string); has && ble != learned {
								env["baseLogEpoch"] = learned
								if m, ok := c.Arg.(map[string]any); ok {
									m["envelope"] = env
								} else {
									c.Arg = argMap(c.Arg)
								}
								fmt.Printf("zcode: recovery: injected baseLogEpoch %s for %s (%s)\n", learned, sid, typ)
							}
						}
					}
				}
			}
		}
		b, _ := json.Marshal(c.Arg)
		out := relay.ChannelCallBytes(c)
		// Remember the encoded frame of every event-listen: the host scopes
		// listens per service-port attachment, so after a reattach the new
		// port starts deaf unless the listens are replayed onto it (see
		// officialReattach).
		if c.Kind == relay.KindEventListen {
			if rec := officialActiveRec(); rec != nil {
				rec.recordListenFrame(c.ID, out)
			}
		}
		// Remember the encoded bytes of promise calls: if the host answers
		// fault.connection.handshakeRequired (phone sent before the host
		// pipe finished handshaking — typical right after a daemon restart)
		// the call is otherwise silently lost.
		if c.Kind == relay.KindPromise {
			if rec := officialActiveRec(); rec != nil {
				env, _ := argMap(c.Arg)["envelope"].(map[string]any)
				typ, _ := env["type"].(string)
				sid, _ := env["sessionId"].(string)
				rec.mu.Lock()
				if rec.pendingRaw == nil {
					rec.pendingRaw = map[int]*pendingRawCall{}
				}
				rec.pendingRaw[c.ID] = &pendingRawCall{raw: out, typ: typ, sid: sid, call: deepCopyCall(c)}
				rec.mu.Unlock()
			}
		}
		if verboseLogs {
			fmt.Printf("zcode: inject ws into %s.%s -> %s | frame %d bytes: %x\n", c.ChannelName, c.Name, string(b), len(out), out[:min(48, len(out))])
		}
		return forwardRawToOfficialHost(out)
	}
}

// pendingRawCall remembers an encoded promise call for handshake retry.

func officialActiveRec() *officialRecovery {
	officialState.mu.Lock()
	b := officialState.active
	officialState.mu.Unlock()
	if b == nil {
		return nil
	}
	return b.rec
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

// switchSessionModel sends a switchModelConfig conversation command for one
// session, moving it off a rate-limited provider. The engine applies model
// switches per-session (host requires a sessionId), so a fresh subscribe is
// the natural trigger.
func switchSessionModel(b *officialHostBridge, sid, provider, model, thought string) {
	r := b.rec
	officialState.mu.Lock()
	ws := officialState.workspace
	officialState.mu.Unlock()
	if ws == "" || sid == "" || provider == "" || model == "" {
		return
	}
	if thought == "" {
		thought = "max"
	}
	r.mu.Lock()
	cid := r.clientBySession[sid]
	r.mu.Unlock()
	if cid == "" {
		cid = "client-" + uuidNew()
	}
	raw := relay.ChannelCallBytes(&relay.ChannelCall{
		Kind: relay.KindPromise, ID: r.mintID(),
		ChannelName: "zcode-agent", Name: "sendConversationCommandV4",
		Arg: map[string]any{
			"workspacePath": ws,
			"envelope": map[string]any{
				"commandId":    uuidNew(),
				"clientId":     cid,
				"sessionId":    sid,
				"baseRevision": 0,
				"type":         "switchModelConfig",
				"payload": map[string]any{
					"provider": provider,
					"model":    model,
					"thought":  thought,
				},
				"issuedAt": time.Now().UnixMilli(),
			},
		},
	})
	forwardRawToOfficialHost(raw)
	fmt.Printf("zcode: switchModelConfig %s/%s thought=%s for %s\n", provider, model, thought, sid)
}

// Switch-model override, sourced from three env vars (no quotes/JSON so
// systemd's Environment= parsing can't mangle them):
//
//	ZQF_SWITCH_PROVIDER=siliconflow
//	ZQF_SWITCH_MODEL_NAME=deepseek-ai/DeepSeek-V3.1
//	ZQF_SWITCH_THOUGHT=max
var switchModelProvider = os.Getenv("ZQF_SWITCH_PROVIDER")
var switchModelName = os.Getenv("ZQF_SWITCH_MODEL_NAME")
var switchModelThought = os.Getenv("ZQF_SWITCH_THOUGHT")
