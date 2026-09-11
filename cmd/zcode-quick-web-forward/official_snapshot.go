package main

import (
	"encoding/json"
	"fmt"
	"github.com/friddle/zcode-quick-web-forward/internal/relay"
	"os"
	"strings"
	"time"
)

func buildProjectionSnapshot(sid string, rowsRes map[string]any, queued []any, dead map[string]bool, mode string, optRow map[string]any) map[string]any {
	fullRows, _ := rowsRes["rows"].([]any)
	// The latest turnHeader row carries the live turn state — without it the
	// composer never shows the 停止 button mid-turn (canStop hardcoded false
	// froze the UI into send-only mode).
	phase, canStop := "completedSuccess", false
	for _, r := range fullRows {
		if m, ok := r.(map[string]any); ok && m["kind"] == "turnHeader" {
			if m["state"] == "running" {
				phase, canStop = "running", true
			} else {
				phase, canStop = "completedSuccess", false
			}
		}
	}
	// Drop queue items whose message has started executing (its userInput row
	// is now part of the transcript).
	rows := rowsRes
	if inner, ok := rowsRes["rows"].([]any); ok {
		rows = map[string]any{"window": inner}
	} else if inner, ok := rowsRes["rows"].(map[string]any); ok {
		rows = inner
	}
	// Surface engine approval requests: a toolCall row with status
	// pendingApproval means the turn is blocked on permission. The phone
	// renders its approval card from pendingInteractions; without this the
	// request is invisible and the turn hangs forever.
	interactions := []any{}
	for _, r := range fullRows {
		m, ok := r.(map[string]any)
		if !ok || m["status"] != "pendingApproval" {
			continue
		}
		interactionID, _ := m["approvalInteractionId"].(string)
		if interactionID == "" {
			continue
		}
		if dead[interactionID] {
			// Engine already resolved (or never had) this interaction — a
			// pre-restart residue. Demote to running so the tool card stops
			// rendering the dead 待审批 state.
			m["status"] = "running"
			continue
		}
		input, _ := m["input"].(map[string]any)
		command, _ := input["command"].(string)
		desc, _ := input["description"].(string)
		summary := desc
		if summary == "" {
			summary = command
		}
		interactions = append(interactions, map[string]any{
			"interactionId": interactionID,
			"kind":          "permission",
			"anchorRowId":   m["rowId"],
			"createdAt":     m["createdAt"],
			"payload": map[string]any{
				"kind":       "permission",
				"toolCallId": m["entityId"],
				"toolName":   m["toolName"],
				"summary":    summary,
				"detail":     []any{},
				"options": []any{
					map[string]any{"optionId": "allow_once", "label": "Allow once", "kind": "allowOnce"},
					map[string]any{"optionId": "deny", "label": "Deny", "kind": "deny"},
				},
			},
		})
	}
	window, _ := rows["window"].([]any)
	// Cap the recovery window: long transcripts fragment into many rpc-frames
	// and the client fails reassembly (endless recover loop). The visible
	// tail is what matters.
	const maxSnapshotRows = 24
	if len(window) > maxSnapshotRows {
		window = window[len(window)-maxSnapshotRows:]
	}
	firstRowID := any(nil)
	if len(window) > 0 {
		if m, ok := window[0].(map[string]any); ok {
			firstRowID = m["rowId"]
		}
	}
	// An idle-session send renders as a normal optimistic userInput row at the
	// tail of the window (the "other presentation"): the conversation looks
	// alive immediately instead of showing a queue chip for a message that is
	// not queued at all.
	if optRow != nil {
		window = append(window, optRow)
	}
	// This recipe mirrors with_self_implement's conversationSnapshotFrame —
	// field-for-field the shape this exact phone page renders.
	return map[string]any{
		"protocolVersion": 1,
		"sessionId":       sid,
		"logEpoch":        "0",
		"seq":             1,
		"revision":        0,
		"control": map[string]any{
			"phase": phase, "sessionEnded": false, "canStop": canStop,
			"stopState": "idle", "stopTargetKind": "unknown",
			"activeWorks": []any{}, "lastError": nil, "apiRetry": nil,
		},
		"slashCommands": []any{
			map[string]any{"name": "compact", "description": "压缩当前会话上下文", "inputHint": "[instructions]", "source": "builtin"},
			map[string]any{"name": "plan", "description": "切换到 Plan 模式并可选下发任务", "inputHint": "[task]", "source": "builtin"},
			map[string]any{"name": "goal", "description": "查看或设置当前会话目标", "inputHint": "[pause|resume|clear|replace <objective>|<objective>]", "source": "builtin"},
			map[string]any{"name": "init", "description": "创建或更新工作区 AGENTS.md", "inputHint": "[notes]", "source": "builtin"},
		},
		"availability": map[string]any{
			"fork": map[string]any{"allowed": true}, "compact": map[string]any{"allowed": true},
			"switchModelConfig": map[string]any{"allowed": true}, "setFollowupMode": map[string]any{"allowed": true},
			"queueEdit":     map[string]any{"allowed": true},
			"sendQueuedNow": map[string]any{"allowed": false, "reasonCode": "sendQueuedNowRequiresRunning"},
			"pauseGoal":     map[string]any{"allowed": false, "reasonCode": "noGoalToPause"},
			"resumeGoal":    map[string]any{"allowed": false, "reasonCode": "noGoalToResume"},
		},
		"inputRouting":    map[string]any{"mode": "startNow"},
		"meta":            map[string]any{"title": "", "titleSource": "default"},
		"config":          map[string]any{"provider": "bigmodel", "model": "GLM-5.3", "thought": "max", "thoughtLevels": []any{"low", "high", "max"}, "followupMode": "queue", "mode": mode},
		"modelTransition": nil,
		"usage": map[string]any{
			"contextWindow": map[string]any{"usedTokens": 0, "maxTokens": 1000000, "autoCompactThresholdTokens": nil},
			"cumulative":    map[string]any{"inputTokens": 0, "outputTokens": 0, "cacheReadTokens": 0, "cacheWriteTokens": 0},
		},
		"queue":                  map[string]any{"items": queued, "autoDrain": true},
		"pendingInteractions":    interactions,
		"pendingCommands":        []any{},
		"backgroundWorks":        []any{},
		"subagents":              map[string]any{"revision": 0, "childSessionIds": []any{}, "running": []any{}, "endedTotal": 0},
		"goal":                   nil,
		"plan":                   nil,
		"workspaceHookAdmission": nil,
		"rows": map[string]any{
			"window":     window,
			"totalCount": len(window),
			"firstRowId": firstRowID,
		},
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// sameText compares a queued/optimistic message against a transcript row with
// whitespace trimmed (the engine normalizes the text it stores).

func sameText(a, b string) bool {
	return strings.TrimSpace(a) == strings.TrimSpace(b)
}

// firstJSON truncates raw JSON for a log line.
func firstJSON(b json.RawMessage) string {
	s := string(b)
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

// resyncWaiting reports whether a resyncConversationV4 for sid is still owed
// its recovery-delivery frame (a duplicate-rows skip must not swallow it).
func (r *officialRecovery) resyncWaiting(sid string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.resyncPend[sid]
}

// takeDeliveryKind consumes a pending resync for sid: the next snapshot frame
// must be delivered as "recovery" (what resyncConversationV4 waits for), any
// other emission is "initial".
func (r *officialRecovery) takeDeliveryKind(sid string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.resyncPend[sid] {
		delete(r.resyncPend, sid)
		return "recovery"
	}
	return "initial"
}

// nextSnapSeq returns a strictly increasing snapshot seq per session — the
// client re-bases its delta ledger on each snapshot's seq.
func (r *officialRecovery) nextSnapSeq(sid string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.snapSeqBy == nil {
		r.snapSeqBy = map[string]int{}
	}
	r.snapSeqBy[sid]++
	return r.snapSeqBy[sid]
}

// observeTurnState records the latest turnHeader state from a rows fetch.
// Called BEFORE the duplicate-skip so turnRunning stays fresh even when no
// frame is emitted.

func (r *officialRecovery) observeTurnState(b *officialHostBridge, sid string, rowsRes map[string]any) {
	running := false
	for _, row := range fullRowsOf(rowsRes) {
		if m, ok := row.(map[string]any); ok && m["kind"] == "turnHeader" {
			running = m["state"] == "running"
		}
	}
	r.mu.Lock()
	if r.turnRunning == nil {
		r.turnRunning = map[string]bool{}
	}
	r.turnRunning[sid] = running
	r.mu.Unlock()
	if running {
		r.ensureTurnRefresher(b)
	}
}

// ensureTurnRefresher starts a per-host goroutine that keeps refreshing the
// snapshot while any session's turn is running. The post-send schedule covers
// only ~12 minutes; longer turns (and permission requests that arrive after
// it ends) would otherwise leave the page on stale state. Cadence is
// adaptive: ~5s while the turn is young (the completion of a short turn used
// to sit behind a flat 20s poll — "task completion feels slow"), 15s after.

func (r *officialRecovery) ensureTurnRefresher(b *officialHostBridge) {
	r.mu.Lock()
	if r.refresherStarted {
		r.mu.Unlock()
		return
	}
	r.refresherStarted = true
	r.mu.Unlock()
	go func() {
		for {
			time.Sleep(5 * time.Second)
			if !b.h.Alive() {
				return
			}
			now := time.Now().UnixMilli()
			var sids []string
			r.mu.Lock()
			if r.lastRefresh == nil {
				// assignment to a nil map panics — and took the whole daemon
				// down the first time a turn ran (DEVICE_OFFLINE on the phone)
				r.lastRefresh = map[string]int64{}
			}
			for sid, run := range r.turnRunning {
				if !run {
					continue
				}
				interval := int64(15000)
				if now-r.sendAt[sid] < 90000 {
					interval = 5000
				}
				if now-r.lastRefresh[sid] >= interval {
					r.lastRefresh[sid] = now
					sids = append(sids, sid)
				}
			}
			r.mu.Unlock()
			for _, sid := range sids {
				requestRecoverySnapshot(b, sid)
			}
		}
	}()
}

// modeFor returns the session's collaboration mode (build/edit/plan/yolo),
// defaulting to build until the page sends switchCollaborationMode.

func (r *officialRecovery) modeFor(sid string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if m := r.modeBySess[sid]; m != "" {
		return m
	}
	return "build"
}

// sendFresh reports whether a sendText for sid happened recently enough that
// the rows-unchanged dedupe must not suppress the snapshot (the user needs
// to see their message / the queued state right away).
func (r *officialRecovery) sendFresh(sid string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return time.Now().UnixMilli()-r.sendAt[sid] < 8000
}

// optimisticRunning reports whether we should force phase=running for sid
// (briefly after a send, before the engine's turnHeader catches up).
func (r *officialRecovery) optimisticRunning(sid string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return time.Now().UnixMilli() < r.optimisticRun[sid]
}

// queuedCount reports how many synthesized queue items are pending for sid.
func (r *officialRecovery) queuedCount(sid string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.queuedSends[sid])
}

// optimisticCount reports whether an optimistic sent row is pending for sid.
func (r *officialRecovery) optimisticCount(sid string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if row := r.optimisticRow[sid]; row != nil {
		return 1
	}
	return 0
}

// takeOptimisticRow resolves the optimistic sent row for sid against the
// latest transcript rows. Returns the synthetic userInput row to render, or
// nil when it must not (or no longer) render:
//   - the real userInput row appeared → the sent row is official, drop it;
//   - the optimistic window expired without execution → the send was queued
//     after all (the pre-send turnRunning observation was stale) — fall back
//     to a queue chip so the message stays visible.
//
// The returned row mirrors the official userInput row field-for-field (the
// page validates snapshot rows with a strict schema; a shape mismatch would
// reject the whole snapshot frame).

func (r *officialRecovery) takeOptimisticRow(sid string, rows []any) map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	opt, ok := r.optimisticRow[sid]
	if !ok || opt == nil {
		return nil
	}
	text, _ := opt["text"].(string)
	for _, row := range rows {
		if m, ok := row.(map[string]any); ok && m["kind"] == "userInput" {
			if t, _ := m["text"].(string); sameText(t, text) {
				delete(r.optimisticRow, sid)
				return nil
			}
		}
	}
	at, _ := opt["at"].(int64)
	if time.Now().UnixMilli()-at > 45000 {
		// Still unexecuted well past the optimistic window — it really is
		// queued (mid-turn send that started between our observations).
		delete(r.optimisticRow, sid)
		cmdID, _ := opt["cmdID"].(string)
		clientID, _ := opt["clientID"].(string)
		if r.queuedSends == nil {
			r.queuedSends = map[string][]map[string]any{}
		}
		dup := false
		for _, it := range r.queuedSends[sid] {
			if it["sourceCommandId"] == cmdID {
				dup = true
				break
			}
		}
		if !dup {
			r.admSeq++
			r.queuedSends[sid] = append(r.queuedSends[sid], map[string]any{
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
			})
			fmt.Printf("zcode: recovery: optimistic row for %s expired unexecuted — fell back to queue chip\n", sid)
		}
		return nil
	}
	cmdID, _ := opt["cmdID"].(string)
	clientID, _ := opt["clientID"].(string)
	optID := "msg_zqfopt_" + cmdID
	return map[string]any{
		"rowId":               2_000_000_000,
		"turnId":              optID,
		"entityId":            optID,
		"productTurnId":       optID,
		"visibility":          "visible",
		"createdAt":           at,
		"createdAtSeq":        0,
		"kind":                "userInput",
		"text":                text,
		"origin":              "realUser",
		"sourceCommandId":     cmdID,
		"rootSourceCommandId": cmdID,
		"clientId":            clientID,
	}
}

// fullRowsOf extracts the uncapped rows list from a conversationRowsRangeV4
// result.
func fullRowsOf(rowsRes map[string]any) []any {
	rows, _ := rowsRes["rows"].([]any)
	return rows
}

// takeQueuedItems returns the pending mid-turn queue items for sid, dropping
// any whose text has begun executing (a matching userInput row appeared).
// It also records the observed turn-running state for later sends.

func (r *officialRecovery) takeQueuedItems(sid string, rows []any) []any {
	r.mu.Lock()
	defer r.mu.Unlock()
	running := false
	for _, row := range rows {
		if m, ok := row.(map[string]any); ok && m["kind"] == "turnHeader" {
			running = m["state"] == "running"
		}
	}
	if r.turnRunning == nil {
		r.turnRunning = map[string]bool{}
	}
	r.turnRunning[sid] = running
	// Keep only items whose text has not yet shown up as a userInput row
	// (i.e. the message has not begun executing). Previously an extra
	// unconditional pass copied every queued item into `kept` first, so the
	// queue doubled on each snapshot refresh (1 -> 2 -> 4 -> ...), rendering
	// the same pending message dozens of times in the phone UI.
	var kept []any
	for _, item := range r.queuedSends[sid] {
		text, _ := item["text"].(string)
		started := false
		for _, row := range rows {
			if m, ok := row.(map[string]any); ok && m["kind"] == "userInput" {
				if t, _ := m["text"].(string); sameText(t, text) {
					started = true
					break
				}
			}
		}
		if !started {
			kept = append(kept, item)
		}
	}
	if len(kept) == 0 {
		delete(r.queuedSends, sid)
		return []any{}
	}
	stored := make([]map[string]any, len(kept))
	for i, it := range kept {
		stored[i] = it.(map[string]any)
	}
	if r.queuedSends == nil {
		r.queuedSends = map[string][]map[string]any{}
	}
	r.queuedSends[sid] = stored
	return kept
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
	snap["seq"] = r.nextSnapSeq(sid)
	kind := r.takeDeliveryKind(sid)
	inner := map[string]any{
		"topic":          "conversation/" + sid,
		"subscriptionId": sub,
		"fromSeq":        1,
		"toSeq":          1,
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
		"deliveryKind":        kind,
		"logicalFrameId":      uuidNew(),
		"logicalFrameOrdinal": ordinal,
		"topic":               "conversation/" + sid,
		"subscriptionId":      sub,
		"frame":               inner,
	}
	// discriminator probe: an intentionally invalid envelope should produce an
	// assembly fault on the page if frames reach the validator at all
	if os.Getenv("ZQF_BAD_FRAME") == "1" {
		frame["wireVersion"] = 9
	}
	pb, err := json.Marshal(frame)
	if err != nil {
		return
	}
	out := relay.EventFireBytes(listen, pb)
	fmt.Printf("zcode: recovery: synthesized %s snapshot frame %d bytes for %s (listen %d)\n", kind, len(out), sid, listen)
	if b.engine != nil && b.engine.HasIdentity() {
		b.engine.SendRawChannelBytes(out, senderSend())
	}
}
