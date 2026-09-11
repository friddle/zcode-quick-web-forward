package main

import (
	"encoding/json"
	"fmt"
	"github.com/friddle/zcode-quick-web-forward/internal/relay"
	"time"
)

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
		r.recordListen(c.ID)
		fmt.Println("zcode: recovery: onDynamicConversationFrame listen id", c.ID)
	case c.Kind == relay.KindPromise && (c.Name == "subscribeConversationV4" || c.Name == "resyncConversationV4"):
		sid, _ := argMap(c.Arg)["sessionId"].(string)
		if sid == "" {
			// resyncConversationV4 identifies the session only by
			// subscriptionId — map it back through the subscribe acks.
			if sub, _ := argMap(c.Arg)["subscriptionId"].(string); sub != "" {
				sid = r.sessionForSub(sub)
			}
			if sid == "" {
				return
			}
		}
		r.mu.Lock()
		if r.pendingSub == nil {
			r.pendingSub = map[int]string{}
		}
		r.pendingSub[c.ID] = sid
		// A (re)subscribe opens a NEW subscription — the page needs a snapshot
		// for it even if the rows are identical to the last emission, so drop
		// the per-session dedup key.
		delete(r.lastRowsJSON, sid)
		if c.Name == "resyncConversationV4" {
			// The phone answers a resync only with a deliveryKind "recovery"
			// frame; anything else (or silence) trips
			// fault.subscription.recoveryFrameTimedOut and wedges the composer.
			if r.resyncPend == nil {
				r.resyncPend = map[string]bool{}
			}
			r.resyncPend[sid] = true
			// A standalone resync (no fresh subscribe ack, outside the
			// post-send refresh window) has no other trigger — fetch and emit
			// the recovery frame NOW or the phone resync-loops.
			go func(sid string) {
				time.Sleep(120 * time.Millisecond)
				if b.h.Alive() {
					requestRecoverySnapshot(b, sid)
				}
			}(sid)
		}
		r.mu.Unlock()
		fmt.Println("zcode: recovery: tracking", c.Name, "call id", c.ID, "sid", sid)
	case c.Kind == relay.KindPromise && c.ChannelName == "terminal" && c.Name == "create":
		officialState.mu.Lock()
		b := officialState.active
		officialState.mu.Unlock()
		if b != nil && b.rec != nil {
			b.rec.mu.Lock()
			if b.rec.pendingCreate == nil {
				b.rec.pendingCreate = map[int]bool{}
			}
			b.rec.pendingCreate[c.ID] = true
			b.rec.mu.Unlock()
		}
	case c.Kind == relay.KindPromise && c.ChannelName == "zcode-task" &&
		(c.Name == "archiveTask" || c.Name == "unarchiveTask" || c.Name == "pinTask" ||
			c.Name == "unpinTask" || c.Name == "deleteTask"):
		taskID, _ := argMap(c.Arg)["taskId"].(string)
		if taskID == "" {
			return
		}
		op := c.Name[:len(c.Name)-len("Task")] // archive/unarchive/pin/unpin/delete
		r.mu.Lock()
		if r.pendingTask == nil {
			r.pendingTask = map[int]string{}
		}
		r.pendingTask[c.ID] = op + "|" + taskID
		r.mu.Unlock()
	case c.Kind == relay.KindPromise && c.ChannelName == "zcode-agent" && c.Name == "sendConversationCommandV4":
		env, _ := argMap(c.Arg)["envelope"].(map[string]any)
		sid, _ := env["sessionId"].(string)
		typ, _ := env["type"].(string)
		if cid, _ := env["clientId"].(string); cid != "" && sid != "" {
			// Remember the phone page's clientId per session — synthesized
			// commands (switchModelConfig) must reuse it or the host rejects
			// them with fault.command.clientMismatch.
			r.mu.Lock()
			if r.clientBySession == nil {
				r.clientBySession = map[string]string{}
			}
			r.clientBySession[sid] = cid
			r.mu.Unlock()
			// Model override (ZQF_SWITCH_PROVIDER): trigger here, where the
			// clientId is already known — a subscribe-time trigger fires before
			// the page has sent any command, so it falls back to a random
			// clientId and the host rejects it as clientMismatch.
			if switchModelProvider != "" {
				r.mu.Lock()
				if r.modelSwitched == nil {
					r.modelSwitched = map[string]bool{}
				}
				need := !r.modelSwitched[sid]
				if need {
					r.modelSwitched[sid] = true
				}
				r.mu.Unlock()
				if need {
					go func(sid string) {
						time.Sleep(200 * time.Millisecond)
						if b.h.Alive() {
							switchSessionModel(b, sid, switchModelProvider, switchModelName, switchModelThought)
						}
					}(sid)
				}
			}
		}
		if sid != "" && typ == "switchModelConfig" {
			// The page's model picker choice — tracked so the next snapshot's
			// config.provider/model/thought report the switch immediately
			// instead of the stale session snapshot (or the old hardcoded
			// bigmodel/GLM-5.3).
			pl, _ := env["payload"].(map[string]any)
			if p, _ := pl["provider"].(string); p != "" {
				if m, _ := pl["model"].(string); m != "" {
					r.mu.Lock()
					if r.modelTracked == nil {
						r.modelTracked = map[string]map[string]any{}
					}
					r.modelTracked[sid] = map[string]any{"provider": p, "model": m, "thought": pl["thought"]}
					r.mu.Unlock()
					fmt.Printf("zcode: recovery: session %s model -> %s/%s\n", sid, p, m)
				}
			}
		}
		if sid != "" && typ == "switchCollaborationMode" {
			// The page's mode selector state lives in our snapshot's
			// config.mode (previously hardcoded "build") — without tracking
			// this every refresh snapshot flipped the chip back to
			// 变更前确认 even though the host/engine applied the switch.
			pl, _ := env["payload"].(map[string]any)
			if mode, _ := pl["mode"].(string); mode != "" {
				r.mu.Lock()
				if r.modeBySess == nil {
					r.modeBySess = map[string]string{}
				}
				r.modeBySess[sid] = mode
				r.mu.Unlock()
				fmt.Printf("zcode: recovery: session %s mode -> %s\n", sid, mode)
			}
		}
		if sid != "" && typ == "resolveInteraction" {
			// Watch the ack: the engine answers an interaction it no longer
			// has pending with a no-op. Rows responses keep the historical
			// pendingApproval status forever after host/engine restarts, so
			// the snapshot would otherwise re-synthesize a dead permission
			// card the user keeps clicking to no effect.
			pl, _ := env["payload"].(map[string]any)
			iid, _ := pl["interactionId"].(string)
			if iid != "" {
				r.mu.Lock()
				if r.pendingResolve == nil {
					r.pendingResolve = map[int][2]string{}
				}
				r.pendingResolve[c.ID] = [2]string{sid, iid}
				r.mu.Unlock()
			}
		}
		if sid != "" && typ == "sendText" {
			// Instant feedback: the page clears the composer on our early ack
			// and then shows NOTHING until the next snapshot — the first one
			// was 4s away and could be deduped away entirely (rows hadn't
			// changed yet), leaving the user staring at a dead screen. Fetch
			// immediately with tight follow-ups, bypass the rows-unchanged
			// dedupe briefly, and optimistically report running.
			now := time.Now().UnixMilli()
			r.mu.Lock()
			if r.sendAt == nil {
				r.sendAt = map[string]int64{}
			}
			if r.optimisticRun == nil {
				r.optimisticRun = map[string]int64{}
			}
			r.sendAt[sid] = now
			r.optimisticRun[sid] = now + 30000
			r.mu.Unlock()
			go func(sid string) {
				for _, d := range []time.Duration{0, 1200 * time.Millisecond, 2500 * time.Millisecond} {
					time.Sleep(d)
					if !b.h.Alive() {
						return
					}
					requestRecoverySnapshot(b, sid)
				}
			}(sid)
			// the turn streams for a while — refresh the snapshot a few times
			// so the assistant reply renders as it lands
			go scheduleRecoverySnapshots(b, sid)
			// Early-ack: the host pends a sendText ~30s while admitting it
			// (longer when queued), and the page disables its send buttons
			// GLOBALLY while any command is in flight — every other task's
			// composer was frozen meanwhile. Answer accepted immediately;
			// the page's applyAck is idempotent so the late host ack is a
			// harmless duplicate.
			if b.engine != nil && b.engine.HasIdentity() {
				if out, err := json.Marshal(map[string]any{"status": "accepted"}); err == nil {
					b.engine.SendRawChannelBytes(relay.PromiseSuccessBytes(c.ID, out), senderSend())
					fmt.Printf("zcode: recovery: early-acked sendText for %s (call %d)\n", sid, c.ID)
				}
			}
			// While a turn is already running this send is QUEUED, not
			// executed. The host's queue decision never resolves quickly (its
			// RPC pends ~30s), so surface the queued state ourselves: echo the
			// item in queue.items until the message shows up in the rows.
			// On an IDLE session (no observed running turn) the queue-chip
			// presentation read as "every task chases the queue" — there the
			// message renders as a normal optimistic userInput row instead
			// (the other presentation), falling back to the queue chip only
			// if it is still unexecuted after the optimistic window.
			if text, _ := env["payload"].(map[string]any)["text"].(string); text != "" {
				cmdID, _ := env["commandId"].(string)
				clientID, _ := env["clientId"].(string)
				r.mu.Lock()
				if r.turnRunning == nil {
					r.turnRunning = map[string]bool{}
				}
				// A send renders as a queue chip when the session is really
				// mid-turn, or when another send is already optimistically
				// displayed (anything after an in-flight send IS queued).
				// Only a truly idle session gets the optimistic sent-row
				// presentation — a queue chip there read as "every task
				// chases the queue" for messages that were never queued.
				if r.queuedSends == nil {
					// Crash-on-first-mid-turn-send: appending to a nil map
					// killed the whole daemon (systemd revived it, but the
					// queued message was lost).
					r.queuedSends = map[string][]map[string]any{}
				}
				if r.optimisticRow == nil {
					r.optimisticRow = map[string]map[string]any{}
				}
				queuedPath := r.turnRunning[sid] || r.optimisticRow[sid] != nil
				if queuedPath {
					// The phone transport re-delivers commands (new call id,
					// SAME commandId) — one synthesized queue item per delivery
					// rendered the message N times.
					dup := false
					for _, it := range r.queuedSends[sid] {
						if it["sourceCommandId"] == cmdID {
							dup = true
							break
						}
					}
					if !dup {
						r.admSeq++
						item := map[string]any{
							"sourceCommandId": cmdID,
							"queueItemId":     "q-" + cmdID,
							"clientId":        clientID,
							"kind":            "sendText",
							"text":            text,
							"attachments":     []any{},
							"delivery":        map[string]any{"requested": "auto", "admitted": "queue"},
							"order":           map[string]any{"admissionSeq": r.admSeq, "queuePosition": len(r.queuedSends[sid])},
							"steer":           map[string]any{"state": "notRequested"},
							"dispatch":        map[string]any{"state": "queued"},
							"admittedAt":      time.Now().UnixMilli(),
						}
						r.queuedSends[sid] = append(r.queuedSends[sid], item)
						fmt.Printf("zcode: recovery: queued send for %s (%d queued)\n", sid, len(r.queuedSends[sid]))
					}
				} else {
					r.optimisticRow[sid] = map[string]any{
						"text": text, "cmdID": cmdID, "clientID": clientID, "at": now,
					}
					fmt.Printf("zcode: recovery: optimistic sent row for idle session %s\n", sid)
				}
				r.mu.Unlock()
			}
		}
	}
}

// scheduleRecoverySnapshots re-emits the conversation snapshot after a send.
// Dense early refreshes make the reply stream in; a long tail (20s cadence,
// 12 minutes) covers long-running turns — without it the phone freezes on a
// stale "正在执行" tool card when the live delta stream stalls, and the turn
// completion only shows up after a manual reload.

func scheduleRecoverySnapshots(b *officialHostBridge, sid string) {
	for _, d := range []time.Duration{4 * time.Second, 10 * time.Second, 20 * time.Second, 35 * time.Second} {
		time.Sleep(d)
		if !b.h.Alive() {
			return
		}
		requestRecoverySnapshot(b, sid)
	}
	// most turns finish within minutes — a 10s cadence keeps the completion
	// flip (and streaming rows) fresh through that window, then relaxes to 20s
	for i := 0; i < 12; i++ {
		time.Sleep(10 * time.Second)
		if !b.h.Alive() {
			return
		}
		requestRecoverySnapshot(b, sid)
	}
	for i := 0; i < 21; i++ {
		time.Sleep(20 * time.Second)
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
	if len(r.listeners()) == 0 {
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

// deepCopyCall returns an independent copy of a channel call (Arg deep-copied
// via JSON), safe to mutate and re-encode after the original is gone.

func deepCopyCall(c *relay.ChannelCall) *relay.ChannelCall {
	cp := &relay.ChannelCall{Kind: c.Kind, ID: c.ID, ChannelName: c.ChannelName, Name: c.Name}
	if b, err := json.Marshal(c.Arg); err == nil {
		var m map[string]any
		if json.Unmarshal(b, &m) == nil {
			cp.Arg = m
		}
	}
	return cp
}

// replayWithRevision re-issues a revision-sensitive command (fork/feedback/
// retry/edit) with the engine's real conversation revision, after the first
// attempt was rejected as proto.staleRevision for baseRevision 0.
func (r *officialRecovery) replayWithRevision(b *officialHostBridge, pr *pendingRawCall, rev int) {
	if pr.call == nil || !b.h.Alive() {
		return
	}
	env, _ := argMap(pr.call.Arg)["envelope"].(map[string]any)
	env["baseRevision"] = float64(rev)
	if m, ok := pr.call.Arg.(map[string]any); ok {
		m["envelope"] = env
	}
	pr.call.ID = r.mintID()
	out := relay.ChannelCallBytes(pr.call)
	r.mu.Lock()
	if r.pendingRaw == nil {
		r.pendingRaw = map[int]*pendingRawCall{}
	}
	r.pendingRaw[pr.call.ID] = &pendingRawCall{raw: out, typ: pr.typ, sid: pr.sid, call: pr.call}
	r.mu.Unlock()
	forwardRawToOfficialHost(out)
}

// retryUntilReady replays a handshake-rejected call with backoff. Each
// replay reuses the original call id, so the host (once ready) answers the
// same id; that answer (or a host death) ends the loop via the bookkeeping
// cleanup in inspectOfficialResponse.
func (r *officialRecovery) retryUntilReady(b *officialHostBridge, id int, pr *pendingRawCall) {
	for _, d := range []time.Duration{200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond,
		1500 * time.Millisecond, 3 * time.Second, 5 * time.Second, 8 * time.Second, 12 * time.Second} {
		time.Sleep(d)
		r.mu.Lock()
		still := r.retrying[id]
		r.mu.Unlock()
		if !still {
			return // answered in the meantime
		}
		if !b.h.Alive() {
			fmt.Printf("zcode: recovery: retry %d (%s) aborted — host gone\n", id, pr.typ)
			return
		}
		fmt.Printf("zcode: recovery: replaying call %d (%s) after handshake rejection\n", id, pr.typ)
		forwardRawToOfficialHost(pr.raw)
	}
	fmt.Printf("zcode: recovery: retry %d (%s) gave up — host never became ready\n", id, pr.typ)
}

// inspectOfficialResponse watches host replies for the synthetic readSession
// results and the subscribe acks, and drives the synthesized snapshot stream.
