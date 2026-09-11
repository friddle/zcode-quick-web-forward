package main

import (
	"encoding/json"
	"fmt"
	"github.com/friddle/zcode-quick-web-forward/internal/relay"
	"os"
	"strings"
	"time"
)

// sessionFacts carries the real session state extracted from the stashed
// zcode-session.readSession result — everything the projection snapshot used
// to hardcode (title, model config, usage meter, slash commands).

type sessionFacts struct {
	title         string
	status        string // session.status: idle/running/...
	providerID    string
	modelID       string
	thought       string
	thoughtLevels []any
	mode          string // settings.mode.current (session-level fallback)
	ctxUsed       int
	ctxWindow     int
	slash         []any
	goal          any // session.target — the engine's goal object (objective/status/...)
}

// parseSessionFacts decodes a readSession payload; nil/invalid input yields
// zero fields (callers fall back to the previous defaults).

func parseSessionFacts(raw json.RawMessage) *sessionFacts {
	f := &sessionFacts{}
	if len(raw) == 0 {
		return f
	}
	var snap struct {
		Session struct {
			Title  string `json:"title"`
			Status string `json:"status"`
			Mode   string `json:"mode"`
			Target any    `json:"target"`
			Model  struct {
				ProviderID string `json:"providerId"`
				ModelID    string `json:"modelId"`
			} `json:"model"`
		} `json:"session"`
		Settings struct {
			Model struct {
				Current struct {
					ProviderID string `json:"providerId"`
					ModelID    string `json:"modelId"`
				} `json:"current"`
				Available []struct {
					Label         string `json:"label"`
					ContextWindow int    `json:"contextWindow"`
				} `json:"available"`
			} `json:"model"`
			ThoughtLevel struct {
				Enabled   bool   `json:"enabled"`
				Current   string `json:"current"`
				Available []struct {
					Value string `json:"value"`
				} `json:"available"`
			} `json:"thoughtLevel"`
			Mode struct {
				Current string `json:"current"`
			} `json:"mode"`
		} `json:"settings"`
		Projection struct {
			TotalTokenCount int `json:"totalTokenCount"`
			ContextUsed     int `json:"contextUsed"`
			ContextWindow   int `json:"contextWindow"`
		} `json:"projection"`
		SlashCommands []any `json:"slashCommands"`
	}
	if json.Unmarshal(raw, &snap) != nil {
		return f
	}
	f.title = snap.Session.Title
	f.status = snap.Session.Status
	f.mode = snap.Session.Mode
	f.goal = snap.Session.Target
	f.providerID = snap.Settings.Model.Current.ProviderID
	if f.providerID == "" {
		f.providerID = snap.Session.Model.ProviderID
	}
	f.modelID = snap.Settings.Model.Current.ModelID
	if f.modelID == "" {
		f.modelID = snap.Session.Model.ModelID
	}
	if snap.Settings.ThoughtLevel.Enabled {
		f.thought = snap.Settings.ThoughtLevel.Current
		for _, l := range snap.Settings.ThoughtLevel.Available {
			if l.Value != "" {
				f.thoughtLevels = append(f.thoughtLevels, l.Value)
			}
		}
	}
	if f.mode == "" {
		f.mode = snap.Settings.Mode.Current
	}
	f.ctxUsed = snap.Projection.ContextUsed
	f.ctxWindow = snap.Projection.ContextWindow
	if len(snap.SlashCommands) > 0 {
		f.slash = snap.SlashCommands
	}
	return f
}

// factsFor parses the stashed readSession payload for sid and overlays the
// page's own switchModelConfig choice (authoritative until the session
// snapshot catches up with the switch).
func (r *officialRecovery) factsFor(sid string) *sessionFacts {
	f := parseSessionFacts(r.snapFor(sid))
	r.mu.Lock()
	m := r.modelTracked[sid]
	r.mu.Unlock()
	if m != nil {
		if p, _ := m["provider"].(string); p != "" {
			f.providerID = p
		}
		if mm, _ := m["model"].(string); mm != "" {
			f.modelID = mm
		}
		if t, _ := m["thought"].(string); t != "" {
			f.thought = t
		}
	}
	return f
}

func buildProjectionSnapshot(sid string, rowsRes map[string]any, queued []any, dead map[string]bool, mode string, optRow map[string]any, facts *sessionFacts, rev int) map[string]any {
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
	// Real session state (readSession stash) replaces the former hardcoded
	// title/model/usage/slash placeholders; fall back to the previous
	// defaults when the session has not reported them (brand-new sessions).
	if facts == nil {
		facts = &sessionFacts{}
	}
	if facts.status == "running" {
		// a session the engine reports running but whose rows carry no
		// turnHeader yet (fresh turn) still means running
		phase, canStop = "running", true
	}
	provider, model, thought := facts.providerID, facts.modelID, facts.thought
	if provider == "" {
		provider = "bigmodel"
	}
	if model == "" {
		model = "GLM-5.3"
	}
	if thought == "" {
		thought = "max"
	}
	levels := facts.thoughtLevels
	if len(levels) == 0 {
		levels = []any{"low", "high", "max"}
	}
	maxCtx := facts.ctxWindow
	if maxCtx <= 0 {
		maxCtx = 1000000
	}
	slash := facts.slash
	if len(slash) == 0 {
		slash = []any{
			map[string]any{"name": "compact", "description": "压缩当前会话上下文", "inputHint": "[instructions]", "source": "builtin"},
			map[string]any{"name": "plan", "description": "切换到 Plan 模式并可选下发任务", "inputHint": "[task]", "source": "builtin"},
			map[string]any{"name": "goal", "description": "查看或设置当前会话目标", "inputHint": "[pause|resume|clear|replace <objective>|<objective>]", "source": "builtin"},
			map[string]any{"name": "init", "description": "创建或更新工作区 AGENTS.md", "inputHint": "[notes]", "source": "builtin"},
		}
	}
	goal := clientGoal(facts.goal)
	// Running-state shapes mirror the OFFICIAL desktop capture (testdata/
	// official-projection.json statePatches.running): activeWorks carries a
	// primaryTurn entry, stopState/stopTargetKind become stoppable/assistant,
	// and inputRouting flips to enqueue so new messages compose into the
	// queue instead of starting a parallel turn.
	activeWorks := []any{}
	stopState, stopTarget := "idle", "unknown"
	routing := "startNow"
	if phase == "running" {
		stopState, stopTarget = "stoppable", "assistant"
		routing = "enqueue"
		activeWorks = []any{map[string]any{
			"kind":                  "primaryTurn",
			"foregroundExecutionId": "runtime_command_" + sid,
			"startedAt":             time.Now().UnixMilli(),
		}}
	}
	// This recipe mirrors with_self_implement's conversationSnapshotFrame —
	// field-for-field the shape this exact phone page renders.
	return map[string]any{
		"protocolVersion": 1,
		"sessionId":       sid,
		"logEpoch":        "0",
		"seq":             1,
		// The page takes its next commands' baseRevision from this field, and
		// the engine compares it EXACTLY against the live conversation revision
		// (baseRevision !== revision → ack stale, proto.staleRevision) —
		// advertising 0 made every revision-checked command (queue ops,
		// switchModelConfig, pauseGoal, …) fail on first press. rev is the
		// latest revision learned from command acks (see harvest in
		// inspectOfficialResponse).
		"revision": rev,
		"control": map[string]any{
			"phase": phase, "sessionEnded": false, "canStop": canStop,
			"stopState": stopState, "stopTargetKind": stopTarget,
			"activeWorks": activeWorks, "lastError": nil, "apiRetry": nil,
		},
		"availability": map[string]any{
			// fork/compact stay always-allowed: the 0.7.0 phone page's schema
			// predates the official gating shapes (the 3.10 desktop capture
			// shows disallowed+reasonCode, but feeding that shape here trips
			// fault.subscription.recoveryFailed on the old strict schema)
			"fork":              map[string]any{"allowed": true},
			"compact":           map[string]any{"allowed": true},
			"switchModelConfig": map[string]any{"allowed": true}, "setFollowupMode": map[string]any{"allowed": true},
			"queueEdit":     map[string]any{"allowed": true},
			"sendQueuedNow": avail(phase == "running", "sendQueuedNowRequiresRunning"),
			"pauseGoal":     goalAvailability(facts.goal, "active"),
			"resumeGoal":    goalAvailability(facts.goal, "paused"),
		},
		"inputRouting":    map[string]any{"mode": routing},
		"meta":            map[string]any{"title": facts.title, "titleSource": "default"},
		"config":          map[string]any{"provider": provider, "model": model, "thought": thought, "thoughtLevels": levels, "followupMode": "queue", "mode": mode},
		"modelTransition": nil,
		"usage": map[string]any{
			"contextWindow": map[string]any{"usedTokens": facts.ctxUsed, "maxTokens": maxCtx, "autoCompactThresholdTokens": nil},
			"cumulative":    map[string]any{"inputTokens": 0, "outputTokens": 0, "cacheReadTokens": 0, "cacheWriteTokens": 0},
		},
		"queue":               map[string]any{"items": queued, "autoDrain": true},
		"pendingInteractions": interactions,
		"pendingCommands":     []any{},
		// backgroundWorks/subagents keep the long-standing synthetic shapes:
		// the 0.7.0 page schema predates nullable-null here too (official
		// 3.10 desktop sends null; feeding null trips the old strict schema)
		"backgroundWorks":        []any{},
		"subagents":              map[string]any{"revision": 0, "childSessionIds": []any{}, "running": []any{}, "endedTotal": 0},
		"goal":                   goal,
		"plan":                   nil,
		"workspaceHookAdmission": nil,
		"rows": map[string]any{
			"window":     window,
			"totalCount": len(window),
			"firstRowId": firstRowID,
		},
		"slashCommands": slash,
	}
}

// clientGoal reshapes the engine's session.target (goal) object into the
// client's goal schema. The shapes differ in three load-bearing ways and a
// raw pass-through gets the whole snapshot rejected
// (fault.subscription.recoveryFailed):
//   - status enum: engine reports active|paused|budget_limited|complete; the
//     client only accepts active|paused|verifying|verified|notSatisfied|failed
//   - iteration (number) and verifications (array) are REQUIRED client-side
//     and absent on the engine object
//   - engine-only fields (sessionId, tokenBudget, tokensUsed, ...) must go
//
// Returns nil when there is no goal or the status has no client equivalent.

func clientGoal(target any) any {
	t, ok := target.(map[string]any)
	if !ok {
		return nil
	}
	engineStatus, _ := t["status"].(string)
	var status string
	switch engineStatus {
	case "active":
		status = "active"
	case "paused", "budget_limited":
		status = "paused"
	case "complete":
		status = "verified"
	default:
		return nil
	}
	return map[string]any{
		"targetId":             t["targetId"],
		"objective":            t["objective"],
		"summaryTitle":         t["summaryTitle"],
		"timeUsedSeconds":      t["timeUsedSeconds"],
		"activeRunStartedAtMs": t["activeRunStartedAtMs"],
		"status":               status,
		"iteration":            0,
		"verifications":        []any{},
	}
}

// goalAvailability gates pause/resume buttons off the goal object the engine
// reports in session.target (status: active|paused|budget_limited|complete).

func goalAvailability(goal any, wantStatus string) map[string]any {
	m, ok := goal.(map[string]any)
	if !ok {
		return map[string]any{"allowed": false, "reasonCode": "noGoalToPause"}
	}
	status, _ := m["status"].(string)
	if status == wantStatus {
		return map[string]any{"allowed": true}
	}
	if wantStatus == "paused" {
		return map[string]any{"allowed": false, "reasonCode": "goalNotPaused"}
	}
	return map[string]any{"allowed": false, "reasonCode": "noGoalToPause"}
}

// avail builds an availability entry; the reasonCode appears only when
// disallowed (matching the official desktop's snapshot shapes).

func avail(allowed bool, reason string) map[string]any {
	if allowed {
		return map[string]any{"allowed": true}
	}
	return map[string]any{"allowed": false, "reasonCode": reason}
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

// modeFor returns the session's collaboration mode (build/edit/plan/yolo).
// The page's own switchCollaborationMode choice wins; an empty result means
// the caller should fall back to the session's reported mode, then build.

func (r *officialRecovery) modeFor(sid string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.modeBySess[sid]
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

// learnedRevision returns the freshest conversation revision harvested from
// command acks (0 when none seen yet) — feeds the snapshot's revision field.
func (r *officialRecovery) learnedRevision(sid string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sessionRevision[sid]
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
				// Must match the engine's queue_<commandId> naming (see the
				// twin site in official_track.go) — the phone sends this id
				// back on every queue mutation.
				"queueItemId":     "queue_" + cmdID,
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

// applyQueueOp mirrors a phone-issued queue mutation onto the synthesized
// queue (queuedSends). The command itself is forwarded to the engine verbatim
// — this only keeps the snapshot's queue view consistent with what the engine
// just did:
//   - deleteQueueItem: the engine removed it and no userInput row will ever
//     retire the chip, so drop it here too;
//   - sendQueuedNow: the engine dispatches it immediately — drop optimistically
//     (the resulting userInput row supersedes it through the normal rows flow);
//   - editQueueItem: retext the chip;
//   - reorderQueueItem: move the chip before beforeQueueItemId (null = tail).

func (r *officialRecovery) applyQueueOp(sid, typ string, pl map[string]any) {
	qid, _ := pl["queueItemId"].(string)
	cmdID := strings.TrimPrefix(qid, "queue_")
	if sid == "" || cmdID == "" {
		return
	}
	r.mu.Lock()
	items := r.queuedSends[sid]
	idx := -1
	for i, it := range items {
		if it["sourceCommandId"] == cmdID {
			idx = i
			break
		}
	}
	if idx < 0 {
		r.mu.Unlock()
		return
	}
	renumber := func() {
		for i, x := range items {
			if o, ok := x["order"].(map[string]any); ok {
				o["queuePosition"] = i
			}
		}
	}
	switch typ {
	case "editQueueItem":
		if nt, _ := pl["newText"].(string); nt != "" {
			items[idx]["text"] = nt
		}
	case "reorderQueueItem":
		before, _ := pl["beforeQueueItemId"].(string)
		bCmd := strings.TrimPrefix(before, "queue_")
		it := items[idx]
		items = append(items[:idx], items[idx+1:]...)
		pos := len(items)
		if bCmd != "" {
			for i, x := range items {
				if x["sourceCommandId"] == bCmd {
					pos = i
					break
				}
			}
		}
		items = append(items[:pos], append([]map[string]any{it}, items[pos:]...)...)
		renumber()
	default: // deleteQueueItem, sendQueuedNow
		items = append(items[:idx], items[idx+1:]...)
		renumber()
	}
	r.queuedSends[sid] = items
	r.mu.Unlock()
	fmt.Printf("zcode: recovery: queue op %s %s mirrored (%d left for %s)\n", typ, cmdID, len(items), sid)
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
