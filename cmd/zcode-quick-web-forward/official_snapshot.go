package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/friddle/zcode-quick-web-forward/internal/relay"
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
	tokens        int   // projection.totalTokenCount — climbs while the engine streams
	revision      int64 // snapshot revision — bumps on every conversation mutation
	interactions  int   // pendingInteractions — permission/interaction waits
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
		PendingInteractions []any `json:"pendingInteractions"`
		Revision            int64 `json:"revision"`
		SlashCommands       []any `json:"slashCommands"`
	}
	if json.Unmarshal(raw, &snap) != nil {
		return f
	}
	f.title = snap.Session.Title
	f.status = snap.Session.Status
	f.mode = snap.Session.Mode
	f.goal = snap.Session.Target
	f.interactions = len(snap.PendingInteractions)
	f.revision = snap.Revision
	f.tokens = snap.Projection.TotalTokenCount
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
	if m == nil {
		// Usage persistence: the readSession stash is a small LRU and the
		// host only polls it while the conversation is open — without a
		// last-known cache the context meter flickered between shown and
		// hidden. Values are the engine's own last report, never invented.
		if c, ok := r.ctxBySess[sid]; ok {
			if f.ctxUsed <= 0 {
				f.ctxUsed = c[0]
			}
			if f.ctxWindow <= 0 {
				f.ctxWindow = c[1]
			}
		}
	}
	r.mu.Unlock()
	if f.ctxUsed > 0 && f.ctxWindow > 0 {
		r.mu.Lock()
		r.ctxBySess[sid] = [2]int{f.ctxUsed, f.ctxWindow}
		r.mu.Unlock()
	}
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
	// firstRowID gives the page a cursor for rowsRange(beforeRowId) history
	// paging. totalCount deliberately stays the CAPPED window length: the
	// page's turn navigator sees totalCount > window and starts a FULL
	// history hydration loop (600KB+ rowsRange pulls) that a phone browser
	// cannot survive — it presented as the page endlessly reloading.
	firstRowID := any(nil)
	if len(window) > 0 {
		if m, ok := window[0].(map[string]any); ok {
			firstRowID = m["rowId"]
		}
	}
	// Cap the recovery window: long transcripts fragment into many rpc-frames
	// and the client fails reassembly (endless recover loop). The visible
	// tail is what matters.
	const maxSnapshotRows = 24
	if len(window) > maxSnapshotRows {
		window = window[len(window)-maxSnapshotRows:]
	}
	// Byte budget on top of the row count: one huge analysis turn makes
	// single rows weigh many KB, so even 24 rows can reach ~100KB — the phone
	// chokes reassembling/rendering it and a refresh looks frozen. Keep the
	// newest rows that fit the budget.
	const snapshotByteBudget = 48 * 1024
	if w, big := shrinkWindowToBudget(window, snapshotByteBudget); big {
		window = w
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

// recordTurnRunning stores the latest engine-observed turn state and books
// transitions: running→ended stamps completedAt (the phone task list reads it
// for the 结束蓝点), and either direction schedules a fresh workspace/task
// list push so the 运行 badge appears/disappears live. Caller must hold r.mu.
func (r *officialRecovery) recordTurnRunning(sid string, running bool) {
	if r.turnRunning == nil {
		r.turnRunning = map[string]bool{}
	}
	prev := r.turnRunning[sid]
	r.turnRunning[sid] = running
	if prev != running {
		// The resurrect gate: only a turn that produced NO observable output
		// since its input was staged counts as silently dead. A normal
		// completion (tokens moved, frames flowed) must NOT resurrect — the
		// ungated version re-injected every successfully finished task once,
		// duplicating it.
		progressAt := r.turnProgressAt[sid]
		stagedAt := r.stagedRetryAt[sid]
		hadOutput := stagedAt > 0 && progressAt >= stagedAt
		r.noteTurnProgressLocked(sid, time.Now().UnixMilli())
		// A turn ended while a daemon-injected input was still staged and
		// unconsumed: the turn died silently (assistant row unfinished) —
		// resurrect it. Async: this runs under r.mu.
		if !running && !hadOutput && r.stagedRetry[sid] != "" && !r.stagedRetryTaken[sid] {
			sidCopy := sid
			go func() {
				time.Sleep(time.Second)
				officialState.mu.Lock()
				b := officialState.active
				officialState.mu.Unlock()
				if b != nil {
					maybeRetryStagedInput(b, sidCopy, "turn ended with staged input")
				}
			}()
		}
	}
	if prev == running {
		return
	}
	if !running {
		if r.completedAt == nil {
			r.completedAt = map[string]int64{}
		}
		r.completedAt[sid] = time.Now().UnixMilli()
		// Fast queue drain: give the engine ~4s to auto-dispatch messages
		// queued behind this turn, then cover the case where that drain
		// never happens (headless wedge — the 60s patrol alone made
		// follow-up messages feel lost).
		time.AfterFunc(4*time.Second, func() { r.dispatchQueuedAfterTurn(sid) })
	}
	if f := taskStatusNudge; f != nil {
		time.AfterFunc(300*time.Millisecond, f)
	}
}

func (r *officialRecovery) observeTurnState(b *officialHostBridge, sid string, rowsRes map[string]any) {
	running := false
	for _, row := range fullRowsOf(rowsRes) {
		if m, ok := row.(map[string]any); ok && m["kind"] == "turnHeader" {
			running = m["state"] == "running"
		}
	}
	r.mu.Lock()
	r.recordTurnRunning(sid, running)
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
			// BUG-002: sessions holding undelivered queue-mirror items need
			// rescue patrol even when NOT marked running — after an abnormal
			// turn end the engine can wedge with queued messages and an idle
			// projection, which the running-only sweep never saw.
			cand := make(map[string]bool, len(sids))
			for _, sid := range sids {
				cand[sid] = true
			}
			for sid := range r.queuedSends {
				if !cand[sid] {
					sids = append(sids, sid)
					cand[sid] = true
				}
			}
			r.mu.Unlock()
			for _, sid := range sids {
				// Stuck-queue rescue first: a turn the host still considers
				// running while our queue mirror sits undelivered means the
				// message silently never dispatches (BUG-002). Sessions not
				// running get the resubmit-only variant (inside).
				rescueStuckQueues(b, sid, now)
				running := func() bool {
					r.mu.Lock()
					defer r.mu.Unlock()
					return r.turnRunning[sid]
				}()
				if running {
					requestRecoverySnapshot(b, sid)
				}
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

// markRowsSeen records when a session's transcript rows were last observed.
// The queue rescues only act on decisions this fresh — an older view cannot
// tell "engine already took the message" from "message still undelivered".
func (r *officialRecovery) markRowsSeen(sid string) {
	r.mu.Lock()
	if r.lastRowsAt == nil {
		r.lastRowsAt = map[string]int64{}
	}
	r.lastRowsAt[sid] = time.Now().UnixMilli()
	r.mu.Unlock()
}

// realFramesFresh reports whether the host's own subscription delivered a
// live conversation frame for sid recently. While it does, the synthesizer
// must NOT emit: a synthesized snapshot carries its own seq counter, and
// interleaved with the host's real stream those frames look like sequence
// gaps — the page resynced/refetched in a loop (constant layout resets).
func (r *officialRecovery) realFramesFresh(sid string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	at, ok := r.realFramesAt[sid]
	return ok && time.Now().UnixMilli()-at < 10_000
}

// hasEmittedBase reports whether a full snapshot (and therefore a delta diff
// base) exists for sid — polling call sites use it to shrink their payloads.
func (r *officialRecovery) hasEmittedBase(sid string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastEmittedState[sid] != nil
}

// queuedTextFor returns the mirrored text behind a queue_<commandId> item,
// or "" when the mirror has no such item (the engine's own queue is then the
// only copy — leave the forwarded command untouched).
func (r *officialRecovery) queuedTextFor(sid, cmdID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, it := range r.queuedSends[sid] {
		if it["sourceCommandId"] == cmdID {
			t, _ := it["text"].(string)
			return t
		}
	}
	return ""
}

// selfInjectedCall reports whether a sendConversationCommandV4 call was
// minted by this daemon (rescue/queue dispatch) rather than by the phone —
// its sendText must skip the optimistic/queue bookkeeping.
func (r *officialRecovery) selfInjectedCall(id int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.selfInjected[id]
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
				"queueItemId": "queue_" + cmdID,
				"clientId":    clientID,
				"kind":        "sendText",
				"text":        text,
				"attachments": []any{},
				"delivery":    map[string]any{"requested": "auto", "admitted": "queue"},
				"order":       map[string]any{"admissionSeq": r.admSeq, "queuePosition": len(r.queuedSends[sid])},
				"steer":       map[string]any{"state": "notRequested"},
				"dispatch":    map[string]any{"state": "queued"},
				"admittedAt":  time.Now().UnixMilli(),
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
	r.recordTurnRunning(sid, running)
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

// applyQueueOp mirrors a phone-issued queue mutation onto the synthesized
// queue mirror. Returns false when the item is no longer in the mirror —
// i.e. its message already dispatched (executing or done), where a 撤回
// needs a stop instead of a queue deletion.
func (r *officialRecovery) applyQueueOp(sid, typ string, pl map[string]any) bool {
	qid, _ := pl["queueItemId"].(string)
	cmdID := strings.TrimPrefix(qid, "queue_")
	if sid == "" || cmdID == "" {
		return false
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
		return false
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
	// The mirror changed while the engine rows may not have — the
	// rows-unchanged dedupe would swallow the next snapshot and the chip
	// would stick on screen until a manual refresh. Drop the dedup key so
	// the follow-up snapshot is always emitted.
	delete(r.lastRowsJSON, sid)
	r.mu.Unlock()
	fmt.Printf("zcode: recovery: queue op %s %s mirrored (%d left for %s)\n", typ, cmdID, len(items), sid)
	return true
}

// shrinkWindowToBudget trims a rows window (from the HEAD — the newest rows
// at the tail are kept) until its JSON weight fits budget. Returns the
// original window when it already fits.
func shrinkWindowToBudget(window []any, budget int) ([]any, bool) {
	total := 0
	sizes := make([]int, len(window))
	for i, row := range window {
		b, err := json.Marshal(row)
		if err != nil {
			sizes[i] = 512
		} else {
			sizes[i] = len(b)
		}
		total += sizes[i]
	}
	if total <= budget {
		return window, false
	}
	keep := total
	start := 0
	for start < len(window)-1 && keep > budget {
		keep -= sizes[start]
		start++
	}
	return window[start:], true
}

// patchableStateKeys are the snapshot's non-rows parts the page accepts in a
// state.updated delta patch (the official v4 deltas schema). rows and
// slashCommands are deliberately absent: rows move via row.* ops, and a
// changed slashCommands list falls back to a full snapshot.
var patchableStateKeys = []string{
	"revision", "control", "availability", "inputRouting", "meta", "config",
	"modelTransition", "usage", "queue", "pendingInteractions",
	"pendingCommands", "backgroundWorks", "subagents", "goal", "plan",
	"workspaceHookAdmission",
}

// rowIdentity returns the stable row id the page's delta ops address.
func rowIdentity(row map[string]any) (string, bool) {
	switch v := row["rowId"].(type) {
	case string:
		return v, v != ""
	case float64:
		return fmt.Sprintf("%v", v), true
	case int64:
		return fmt.Sprintf("%v", v), true
	}
	return "", false
}

// deltaOpsFor diffs snap against the last emitted snapshot for sid and
// returns official v4 delta ops (row.appended / row.upserted /
// state.updated). ok=false when a full snapshot must be emitted instead —
// no base yet, a structural change, or any force-full condition. While
// deltas flow the page patches in place; full-window snapshots reset the
// whole layout, which during streaming read as a constant screen refresh.
//
// Window slides (head trim) are tolerated: a contiguous missing prefix of
// the previous window is treated as trimmed history, not removals — the
// page keeps its own untrimmed view, exactly like the desktop host stream.
func (r *officialRecovery) deltaOpsFor(sid string, snap map[string]any, forceFull bool) ([]map[string]any, bool) {
	r.mu.Lock()
	prevRows := r.lastEmittedRows[sid]
	prevState := r.lastEmittedState[sid]
	r.mu.Unlock()
	if forceFull || prevState == nil {
		return nil, false
	}
	rowsMap, _ := snap["rows"].(map[string]any)
	newRowsAny, _ := rowsMap["window"].([]any)

	prevByID := make(map[string]map[string]any, len(prevRows))
	prevOrder := make([]string, 0, len(prevRows))
	for _, row := range prevRows {
		id, ok := rowIdentity(row)
		if !ok {
			return nil, false // unidentified row — full snapshot is the safe path
		}
		prevByID[id] = row
		prevOrder = append(prevOrder, id)
	}
	ops := make([]map[string]any, 0, 8)
	seen := make(map[string]bool, len(newRowsAny))
	for _, ra := range newRowsAny {
		row, ok := ra.(map[string]any)
		if !ok {
			continue
		}
		id, ok := rowIdentity(row)
		if !ok {
			return nil, false
		}
		seen[id] = true
		if prev, existed := prevByID[id]; !existed {
			ops = append(ops, map[string]any{"op": "row.appended", "row": row})
		} else {
			ab, _ := json.Marshal(prev)
			bb, _ := json.Marshal(row)
			if !bytes.Equal(ab, bb) {
				ops = append(ops, map[string]any{"op": "row.upserted", "row": row})
			}
		}
	}
	// Removals: only a contiguous missing PREFIX is a window slide; anything
	// else (middle/tail vanished) is structural — full snapshot.
	prefix := true
	for _, id := range prevOrder {
		if seen[id] {
			prefix = false
			continue
		}
		if !prefix {
			return nil, false
		}
	}
	patch := map[string]any{}
	for _, k := range patchableStateKeys {
		nv, nb := snap[k]
		pv, pb := prevState[k]
		if !nb && !pb {
			continue
		}
		ab, _ := json.Marshal(pv)
		bb, _ := json.Marshal(nv)
		if !bytes.Equal(ab, bb) && nb {
			patch[k] = nv
		}
	}
	if len(patch) > 0 {
		ops = append(ops, map[string]any{"op": "state.updated", "patch": patch})
	}
	return ops, true
}

// recordEmittedBase stores snap as the delta diff base for sid. Call ONLY
// after a frame actually went out to a live listener — recording on a
// dropped emission made every later fetch read as "unchanged" and the page
// never received anything.
func (r *officialRecovery) recordEmittedBase(sid string, snap map[string]any) {
	rowsMap, _ := snap["rows"].(map[string]any)
	window, _ := rowsMap["window"].([]any)
	rows := make([]map[string]any, 0, len(window))
	for _, ra := range window {
		if row, ok := ra.(map[string]any); ok {
			rows = append(rows, row)
		}
	}
	state := make(map[string]any, len(patchableStateKeys))
	for _, k := range patchableStateKeys {
		if v, ok := snap[k]; ok {
			state[k] = v
		}
	}
	r.mu.Lock()
	r.lastEmittedRows[sid] = rows
	r.lastEmittedState[sid] = state
	r.mu.Unlock()
}

// emitRecoveryDeltas wraps diff ops into the conversation topic frame the
// page's store patches in place (payload kind "deltas"). Same wire envelope
// as snapshots; deliveryKind "online" is the live-stream kind — a recovery
// delivery must carry a full snapshot instead. Returns false when nothing
// was emitted (no live listener) so the caller skips the base recording.
func emitRecoveryDeltas(b *officialHostBridge, sid string, snap map[string]any, ops []map[string]any) bool {
	r := b.rec
	listen := r.listener()
	if listen == 0 || len(ops) == 0 {
		return false
	}
	sub := r.subFor(sid)
	if sub == "" {
		sub = "sub-synth-" + uuidNew()
		r.setSub(sid, sub)
	}
	delete(snap, "protocol")
	epoch := r.epochFor(sid)
	seq := r.nextSnapSeq(sid)
	// The page's store requires deltas to CHAIN exactly: fromSeq must equal
	// the currently applied snapshot.seq (the seq the previous frame left
	// behind) and toSeq becomes the new local seq. Off by one here and every
	// delta reads as a sequence gap — the page resynced every few seconds,
	// each resync re-applied a full snapshot, and the whole view kept
	// resetting (the "history jumps around" bug).
	inner := map[string]any{
		"topic":          "conversation/" + sid,
		"subscriptionId": sub,
		"fromSeq":        seq - 1,
		"toSeq":          seq,
		"sentAt":         time.Now().UnixMilli(),
		"payload":        map[string]any{"kind": "deltas", "deltas": ops},
	}
	if epoch != "" { // an empty logEpoch fails the frame schema's min(1)
		inner["logEpoch"] = epoch
	}
	r.mu.Lock()
	r.wireOrdinal++
	ordinal := r.wireOrdinal
	r.mu.Unlock()
	frame := map[string]any{
		"wireVersion":         3,
		"kind":                "complete",
		"deliveryKind":        "online",
		"logicalFrameId":      uuidNew(),
		"logicalFrameOrdinal": ordinal,
		"topic":               "conversation/" + sid,
		"subscriptionId":      sub,
		"frame":               inner,
	}
	pb, err := json.Marshal(frame)
	if err != nil {
		return false
	}
	out := relay.EventFireBytes(listen, pb)
	fmt.Printf("zcode: recovery: synthesized deltas frame %d bytes (%d ops) for %s (listen %d)\n", len(out), len(ops), sid, listen)
	if b.engine != nil && b.engine.HasIdentity() {
		b.engine.SendRawChannelBytes(out, senderSend())
		return true
	}
	return false
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
	// The strict frame schema (recovery deliveries) requires a non-empty
	// frame-level logEpoch that MATCHES snapshot.logEpoch. Omitting it made
	// every recovery frame fail validation silently — the page resynced
	// forever and updates only appeared after a manual reload.
	if epoch != "" {
		inner["logEpoch"] = epoch
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
	// A full snapshot REPLACES the page's base — it is also the new diff
	// base for subsequent deltas. (listen==0 already returned above, so a
	// recorded base always corresponds to a delivered frame.)
	r.recordEmittedBase(sid, snap)
	if b.engine != nil && b.engine.HasIdentity() {
		b.engine.SendRawChannelBytes(out, senderSend())
	}
}
