package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/friddle/zcode-quick-web-forward/internal/relay"
	"github.com/friddle/zcode-quick-web-forward/internal/zcode"
	"os"
	"strings"
	"time"
)

func inspectOfficialResponse(raw []byte) {
	officialState.mu.Lock()
	b := officialState.active
	officialState.mu.Unlock()
	if b == nil || b.rec == nil {
		return
	}
	kind, id, data, ok := relay.ParseChannelResponse(raw)
	if !ok {
		return
	}
	r := b.rec
	if kind == relay.KindEventFire {
		// The engine publishes v4/conversation/frame through the host onto
		// this event stream — frames we never synthesize ourselves. Capture
		// one for diagnostics (schema ground truth for live state merging).
		if len(data) > 0 && bytes.Contains(data, []byte(`"conversation/`)) {
			if !bytes.Contains(data, []byte(`"logicalFrameOrdinal"`)) { // skip our own synthesized wire frames
				_ = os.WriteFile("/tmp/zqf-live-frame.json", data, 0644)
				fmt.Printf("zcode: recovery: LIVE engine frame %d bytes captured\n", len(data))
			}
		}
		// A live conversation frame that carries a fresh pendingApproval row
		// means the engine is now blocked on a permission. The page renders
		// approval cards only from our snapshot's pendingInteractions list —
		// without a prompt refresh the card (and the engine) waits until the
		// next scheduled snapshot, or forever if that window has passed.
		if len(data) > 0 && bytes.Contains(data, []byte(`"status":"pendingApproval"`)) {
			r.mu.Lock()
			sids := make([]string, 0, len(r.subBySession))
			for sid := range r.subBySession {
				sids = append(sids, sid)
			}
			now := time.Now().Unix()
			due := now-r.lastKick >= 3
			if due {
				r.lastKick = now
			}
			r.mu.Unlock()
			if due {
				for _, sid := range sids {
					go requestRecoverySnapshot(b, sid)
				}
			}
		}
		// Turn lifecycle events stream past here live. A turn.completed /
		// turn.failed frame is the ENGINE's own completion signal — refreshing
		// immediately ends the "turn is done but the phone shows 正在执行 for
		// another 20s poll interval" lag.
		if len(data) > 0 && (bytes.Contains(data, []byte(`"type":"turn.completed"`)) ||
			bytes.Contains(data, []byte(`"type":"turn.failed"`))) {
			if sid := sidFromFrameBytes(data); sid != "" {
				r.mu.Lock()
				if r.lastTurnDone == nil {
					r.lastTurnDone = map[string]int64{}
				}
				nowMs := time.Now().UnixMilli()
				due := nowMs-r.lastTurnDone[sid] >= 1000
				if due {
					r.lastTurnDone[sid] = nowMs
				}
				r.mu.Unlock()
				if due {
					fmt.Println("zcode: recovery: turn lifecycle event — instant snapshot for", sid)
					go requestRecoverySnapshot(b, sid)
				}
			}
		}
		return
	}
	if kind != relay.KindPromiseOK && kind != relay.KindPromiseErr {
		return
	}
	// Handshake-retry bookkeeping. A call rejected with
	// fault.connection.handshakeRequired never reached the host's agent
	// service — replay it with backoff instead of dropping it on the floor.
	if handshakeFailed := kind == relay.KindPromiseErr && len(data) > 0 &&
		bytes.Contains(data, []byte("handshakeRequired")); handshakeFailed {
		r.mu.Lock()
		pr := r.pendingRaw[id]
		already := r.retrying[id]
		if pr != nil && !already {
			if r.retrying == nil {
				r.retrying = map[int]bool{}
			}
			r.retrying[id] = true
		}
		r.mu.Unlock()
		if pr != nil && !already {
			go r.retryUntilReady(b, id, pr)
		}
		if pr != nil {
			return // retry loop owns this id
		}
	} else {
		r.mu.Lock()
		pr := r.pendingRaw[id]
		wasRetried := r.retrying[id]
		delete(r.pendingRaw, id)
		delete(r.retrying, id)
		r.mu.Unlock()
		// The engine answers revision-sensitive commands (fork/feedback/
		// retry/edit) with a PromiseSuccess whose body is status:"stale" +
		// revisionAtDecision — because the page sent baseRevision 0. Learn
		// the real revision and replay the command with it, so the fork/
		// feedback actually lands instead of silently no-oping.
		//
		// Every sendConversationCommandV4 ack carries revisionAtDecision, not
		// just the stale ones — harvest it wherever it appears so the
		// synthesized snapshot can advertise a baseRevision the engine
		// accepts on the FIRST press (queue ops are exact-match).
		if kind == relay.KindPromiseOK && pr != nil && pr.sid != "" && len(data) > 0 {
			var st struct {
				Status             string `json:"status"`
				RevisionAtDecision int    `json:"revisionAtDecision"`
			}
			if json.Unmarshal(data, &st) == nil && st.RevisionAtDecision > 0 {
				r.mu.Lock()
				if r.sessionRevision == nil {
					r.sessionRevision = map[string]int{}
				}
				r.sessionRevision[pr.sid] = st.RevisionAtDecision
				r.mu.Unlock()
				if st.Status == "stale" {
					fmt.Printf("zcode: recovery: learned revision %d for %s (was %s) — replaying\n", st.RevisionAtDecision, pr.sid, pr.typ)
					go r.replayWithRevision(b, pr, st.RevisionAtDecision)
				}
			}
		}
		if pr != nil && wasRetried && kind == relay.KindPromiseOK && pr.typ == "sendText" && pr.sid != "" {
			go scheduleRecoverySnapshots(b, pr.sid)
		}
	}
	r.mu.Lock()
	taskMut, isTask := r.pendingTask[id]
	if isTask {
		delete(r.pendingTask, id)
	}
	r.mu.Unlock()
	if isTask && kind == relay.KindPromiseErr {
		// The official host only resolves list mutations against tasks it
		// created this run ("无法解析唯一 source"); older tasks it still
		// serves from the shared index. Apply the mutation here so the
		// phone's archive/pin/delete buttons work for those too.
		op, taskID, _ := strings.Cut(taskMut, "|")
		if err := zcode.MutateTask(taskID, op); err != nil {
			fmt.Printf("zcode: recovery: task mutation fallback %s %s failed: %v\n", op, taskID, err)
		} else {
			fmt.Printf("zcode: recovery: task mutation fallback applied %s %s\n", op, taskID)
		}
		return
	}
	r.mu.Lock()
	isCreate := r.pendingCreate[id]
	if isCreate {
		delete(r.pendingCreate, id)
	}
	sid, isSub := r.pendingSub[id]
	if isSub {
		delete(r.pendingSub, id)
	}
	rsid, isRead := r.pendingRead[id]
	if isRead {
		delete(r.pendingRead, id)
	}
	resolve, isResolve := r.pendingResolve[id]
	if isResolve {
		delete(r.pendingResolve, id)
	}
	r.mu.Unlock()
	if isResolve && kind == relay.KindPromiseOK && len(data) > 0 {
		var res struct {
			Status string `json:"status"`
		}
		if json.Unmarshal(data, &res) == nil {
			sid, iid := resolve[0], resolve[1]
			fmt.Printf("zcode: recovery: resolveInteraction %s ack status %s\n", iid, res.Status)
			if res.Status == "noop" || res.Status == "rejected" {
				// The engine has no such pending interaction — the row's
				// pendingApproval status is stale (pre-restart residue).
				// Blacklist it: stop synthesizing the card, unblock the
				// snapshot flow, and push a clean snapshot immediately.
				r.mu.Lock()
				if r.deadInteractions == nil {
					r.deadInteractions = map[string]bool{}
				}
				r.deadInteractions[iid] = true
				delete(r.lastRowsJSON, sid)
				r.mu.Unlock()
				go requestRecoverySnapshot(b, sid)
			} else if res.Status == "accepted" || res.Status == "duplicate" {
				// Refresh promptly so the answered card clears without
				// waiting for the next scheduled snapshot.
				go func(sid string) {
					time.Sleep(600 * time.Millisecond)
					requestRecoverySnapshot(b, sid)
				}(sid)
			}
		}
	}
	if isCreate && len(data) > 0 {
		var res struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(data, &res) == nil && res.ID != "" {
			r.mu.Lock()
			r.lastTermID = res.ID
			r.mu.Unlock()
			fmt.Println("zcode: recovery: terminal created", res.ID)
			r.flushTerminalListens(res.ID)
		}
	}
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
			go func(sid string) {
				for _, d := range []time.Duration{3 * time.Second, 8 * time.Second} {
					time.Sleep(d)
					if b.h.Alive() {
						requestRecoverySnapshot(b, sid)
					}
				}
			}(sid)
		}
	case isRead && len(data) > 0:
		// stash the session snapshot, then fetch the official transcript rows
		_ = os.WriteFile("/tmp/zqf-snap.json", data, 0644)
		r.stashSnap(rsid, data)
		requestConversationRows(b, rsid)
		go requestConversationPlans(b, rsid)
	}
	r.mu.Lock()
	rowsid, isRows := r.pendingRows[id]
	if isRows {
		delete(r.pendingRows, id)
	}
	plansid, isPlans := r.pendingPlans[id]
	if isPlans {
		delete(r.pendingPlans, id)
	}
	r.mu.Unlock()
	if isPlans && kind == relay.KindPromiseOK && len(data) > 0 {
		r.stashPlans(plansid, data)
	}
	if isRows && len(data) > 0 {
		var rowsRes map[string]any
		_ = json.Unmarshal(data, &rowsRes)
		fmt.Printf("zcode: recovery: rowsRange result keys %v head %s\n", keysOf(rowsRes), firstJSON(data))
		_ = os.WriteFile("/tmp/zqf-rows.json", data, 0644)
		r.observeTurnState(b, rowsid, rowsRes)
		// Never suppress a snapshot that carries a pending approval — the
		// page can't answer what it never sees. Interactions the engine
		// already reported as resolved don't count (stale rows).
		r.mu.Lock()
		dead := make(map[string]bool, len(r.deadInteractions))
		for k, v := range r.deadInteractions {
			dead[k] = v
		}
		r.mu.Unlock()
		blocked := false
		for _, row := range fullRowsOf(rowsRes) {
			if m, ok := row.(map[string]any); ok && m["status"] == "pendingApproval" {
				if iid, _ := m["approvalInteractionId"].(string); iid != "" && dead[iid] {
					continue
				}
				blocked = true
				break
			}
		}
		// Resolve the optimistic sent row BEFORE the duplicate-skip: an
		// expired optimistic row converts into a queue item, which must lift
		// the suppression or the fallback chip would never render.
		optRow := r.takeOptimisticRow(rowsid, fullRowsOf(rowsRes))
		if r.sameAsLast(rowsid, data) && !r.resyncWaiting(rowsid) && !r.sendFresh(rowsid) && r.queuedCount(rowsid) == 0 && r.optimisticCount(rowsid) == 0 && r.lastQEmitted[rowsid] == 0 && !blocked {
			fmt.Println("zcode: recovery: rows unchanged — skipping duplicate snapshot")
			return
		}
		r.rememberRows(rowsid, data)
		queued := r.takeQueuedItems(rowsid, fullRowsOf(rowsRes))
		facts := r.factsFor(rowsid)
		mode := r.modeFor(rowsid)
		if mode == "" {
			if facts.mode != "" {
				mode = facts.mode
			} else {
				mode = "build"
			}
		}
		snap := buildProjectionSnapshot(rowsid, rowsRes, queued, dead, mode, optRow, facts, r.learnedRevision(rowsid))
		if r.optimisticRunning(rowsid) {
			// Send just happened and the engine's turnHeader hasn't caught
			// up — report running so the composer flips to 停止生成 instead
			// of showing a dead idle state.
			if ctl, ok := snap["control"].(map[string]any); ok && ctl["phase"] != "running" {
				ctl["phase"] = "running"
				ctl["canStop"] = true
			}
		}
		r.mu.Lock()
		if r.lastQEmitted == nil {
			r.lastQEmitted = map[string]int{}
		}
		r.lastQEmitted[rowsid] = len(r.queuedSends[rowsid])
		r.mu.Unlock()
		if epoch := r.epochFor(rowsid); epoch != "" {
			snap["logEpoch"] = epoch
		}
		if st, ok := snap["rows"].(map[string]any); ok {
			fmt.Printf("zcode: recovery: rows window %d\n", len(st["window"].([]any)))
		}
		emitRecoverySnapshot(b, rowsid, snap)
	}
}

// sidFromFrameBytes best-effort extracts the session id a live frame belongs
// to: conversation frames carry topic "conversation/<sessionId>", engine
// lifecycle events carry a sessionId field. Empty when neither appears.

func sidFromFrameBytes(data []byte) string {
	for _, marker := range []string{`"topic":"conversation/`, `"sessionId":"`} {
		i := bytes.Index(data, []byte(marker))
		if i < 0 {
			continue
		}
		rest := data[i+len(marker):]
		if end := bytes.IndexByte(rest, '"'); end > 0 {
			return string(rest[:end])
		}
	}
	return ""
}

// keysOf returns the top-level keys of a decoded JSON object (diagnostics).
