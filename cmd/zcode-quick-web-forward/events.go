// Engine event handling: runtime preferences, permission auto-approval,
// AskUserQuestion surfacing, browser host calls, streaming chunks,
// turn.terminal finalization and queued-send auto-drain.

package main

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/friddle/zcode-quick-web-forward/internal/browser"
	enginepkg "github.com/friddle/zcode-quick-web-forward/internal/engine"
	"github.com/friddle/zcode-quick-web-forward/internal/relay"
	"github.com/friddle/zcode-quick-web-forward/internal/zcode"
)

// handleEngineEvent processes engine->client notifications: it answers
// session/requestRuntimePreferences and forwards conversation-relevant stream
// events to the phone as onDynamicConversationFrame frames.
func handleEngineEvent(engClient *enginepkg.Client, engine *relay.BridgeEngine, sender *relaySender, ps *phoneSessions, m json.RawMessage, br *browser.Browser) {
	var ev struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if json.Unmarshal(m, &ev) != nil || ev.Method == "" {
		return
	}
	switch ev.Method {
	case "session/requestRuntimePreferences":
		// The engine asks the client for runtime prefs; answer with the real
		// settings so it can materialize the app and accept messages.
		result := map[string]any{
			"nativeSearchEnhancementsEnabled": true,
			"memoryEnabled":                   false,
		}
		engClient.RespondToRequest(ev.ID, result)
		fmt.Println("zcode: engine prefs ok")
	case "interaction/requestPermission":
		// The engine requests permission for a tool (e.g. browser-use opening
		// a browser, or Edit/Write changing a file). The phone already asked
		// to run the task, so in edit/plan/yolo modes we auto-approve. In
		// confirm mode (变更前确认) file-modifying tools must NOT be silently
		// allowed — deny them so the engine reports the change needs approval.
		// The broker result schema S2 is
		// {decision, reason?, modifiedInput?, permissionUpdates?}.strict() —
		// any extra field (even resolvedAt) fails strict and the engine
		// silently retries the permission request forever.
		var pp struct {
			ToolName string `json:"toolName"`
			Action   string `json:"action"`
		}
		_ = json.Unmarshal(ev.Params, &pp)
		ps.mu.Lock()
		mode := ps.collabMode
		ps.mu.Unlock()
		tool := pp.ToolName
		if tool == "" {
			tool = pp.Action
		}
		if mode == "confirm" && (tool == "Edit" || tool == "Write" || tool == "edit_file" || tool == "write_file") {
			engClient.RespondToRequest(ev.ID, map[string]any{
				"decision": "deny",
				"reason":   "变更前确认模式: 文件修改需要用户批准",
			})
			fmt.Printf("zcode: denied %s (confirm mode)\n", tool)
			return
		}
		engClient.RespondToRequest(ev.ID, map[string]any{
			"decision": "allow",
			"reason":   "Auto-approved by zcode-quick-web-forward (phone requested this task)",
		})
		fmt.Printf("zcode: auto-approved permission request (mode=%s tool=%s)\n", mode, tool)
	case "interaction/requestUserInput":
		// AskUserQuestion: the engine is blocked on a user decision. Surface
		// it as a pendingInteraction (question card in the phone) plus a
		// pendingApproval toolCall row in the transcript; the phone answers
		// via the resolveInteraction command (see bridgeSendCommand).
		var rq struct {
			RequestID  string `json:"requestId"`
			SessionID  string `json:"sessionId"`
			ToolCallID string `json:"toolCallId"`
			TurnID     string `json:"turnId"`
			Prompt     string `json:"prompt"`
			Input      struct {
				Questions []struct {
					Question string `json:"question"`
					Header   string `json:"header"`
					Options  []struct {
						Label string `json:"label"`
						Value string `json:"value"`
					} `json:"options"`
				} `json:"questions"`
			} `json:"input"`
		}
		_ = json.Unmarshal(ev.Params, &rq)
		// The engine's verbatim input object — the answer must echo it back
		// with the answers merged in (broker "modify" → modifiedInput).
		var raw struct {
			Input map[string]any `json:"input"`
		}
		_ = json.Unmarshal(ev.Params, &raw)
		interactionID := rq.RequestID
		if interactionID == "" {
			interactionID = uuidNew()
		}
		pi := &pendingInteraction{
			InteractionID: interactionID,
			EngineReqID:   ev.ID,
			SessionID:     rq.SessionID,
			ToolCallID:    rq.ToolCallID,
			Prompt:        rq.Prompt,
			Input:         raw.Input,
		}
		for _, q := range rq.Input.Questions {
			qm := map[string]any{"question": q.Question}
			if q.Header != "" {
				qm["header"] = q.Header
			}
			opts := make([]map[string]any, 0, len(q.Options))
			for _, o := range q.Options {
				id := o.Value
				if id == "" {
					id = o.Label
				}
				opts = append(opts, map[string]any{"optionId": id, "label": o.Label})
			}
			qm["options"] = opts
			pi.Questions = append(pi.Questions, qm)
		}
		if pi.Prompt == "" && len(pi.Questions) > 0 {
			pi.Prompt = pi.Questions[0]["question"].(string)
		}
		// Plan approval (ExitPlanMode): the engine sends input {plan: "..."}
		// with no questions. Synthesize an approve/reject card so the phone
		// can answer it like any question.
		if len(pi.Questions) == 0 {
			if plan, ok := raw.Input["plan"].(string); ok && plan != "" {
				if len(plan) > 2000 {
					plan = plan[:2000] + "…"
				}
				pi.Prompt = plan
				pi.Questions = []map[string]any{{
					// header is REQUIRED by the client's question schema (El).
					"question": "是否批准该计划?", "header": "计划",
					"options": []map[string]any{
						{"optionId": "approve", "label": "批准"},
						{"optionId": "reject", "label": "拒绝"},
					},
				}}
				pi.IsPlanApproval = true
			}
		}
		if ps.getPendingInteraction(interactionID) != nil {
			return // engine retry of an interaction we already surfaced
		}
		// Retried requests carry a fresh requestId each time; supersede the
		// stale one so the user's answer reaches the engine's active waiter.
		if prev := ps.pendingInteractionForToolCall(rq.ToolCallID); prev != nil {
			engClient.RespondToRequest(prev.EngineReqID, map[string]any{"action": "cancel"})
			ps.removePendingInteraction(prev.InteractionID)
			fmt.Printf("zcode: superseded stale interaction %s (same tool call %s)\n", prev.InteractionID, rq.ToolCallID)
		}
		pi.RowID = ps.nextRowID()
		ps.addPendingInteraction(pi)

		ps.mu.Lock()
		convID, convSub := ps.convListener, ps.convSubscription
		ps.mu.Unlock()
		// Patch only: the transcript's own tool row (rendered from the engine
		// transcript) covers the visual, the pendingInteractions patch drives
		// the interactive question card. Bundling a synthetic row.appended
		// here made the client drop the whole frame.
		if b, err := json.Marshal(conversationDeltaFrame(ps, rq.SessionID, convSub, ps.nextOrdinal(), []any{
			map[string]any{"op": "state.updated", "patch": map[string]any{"pendingInteractions": ps.pendingInteractionsPayload()}},
		})); err == nil && convID > 0 {
			// Debug: dump the exact interaction entries so a client-side
			// schema rejection can be diffed against a working card.
			if dbg, derr := json.Marshal(ps.pendingInteractionsPayload()); derr == nil {
				fmt.Printf("zcode: pendingInteractions payload: %s\n", dbg)
			}
			engine.SendChannelEvent(convID, b, sender.send)
			// The client renders the interactive question card from the
			// snapshot's pendingInteractions, not from the delta (verified
			// empirically) — follow the patch with a fresh recovery snapshot.
			if sb, serr := json.Marshal(conversationSnapshotFrame(ps, rq.SessionID, ps.workspacePath, convSub, "recovery", ps.nextOrdinal(), ps.snapshotRows(), ps.collabMode, "running")); serr == nil {
				engine.SendChannelEvent(convID, sb, sender.send)
				fmt.Printf("zcode: pushed pendingInteractions patch conv=%d entries=%d (+snapshot)\n", convID, len(ps.pendingInteractionsPayload()))
			}
		} else {
			fmt.Printf("zcode: pendingInteractions push skipped conv=%d err=%v\n", convID, err)
		}
		fmt.Printf("zcode: pending interaction %s session=%s questions=%d (waiting for answer)\n", interactionID, rq.SessionID, len(pi.Questions))
	case "interaction/browserList":
		// Report the browser host so the engine's browser-use plugin has a
		// real browser to drive.
		var p struct {
			RequestID string `json:"requestId"`
		}
		_ = json.Unmarshal(ev.Params, &p)
		if br == nil {
			engClient.RespondToRequest(ev.ID, map[string]any{"browsers": []any{}})
			fmt.Println("zcode: browserList (no browser host)")
			return
		}
		insts := br.List()
		engClient.RespondToRequest(ev.ID, map[string]any{"browsers": insts})
		fmt.Printf("zcode: browserList -> %d browser\n", len(insts))
	case "interaction/browserExecute":
		// Execute a browser command on the browser host.
		var p struct {
			RequestID string         `json:"requestId"`
			BrowserID string         `json:"browserId"`
			Command   map[string]any `json:"command"`
		}
		if json.Unmarshal(ev.Params, &p) != nil {
			return
		}
		if br == nil {
			engClient.RespondToRequest(ev.ID, map[string]any{"ok": false, "error": map[string]any{"code": "backend_unavailable", "message": "no browser host"}, "elapsedMs": 0})
			return
		}
		result := br.Execute(p.Command)
		engClient.RespondToRequest(ev.ID, result)
		fmt.Printf("zcode: browserExecute %v -> ok=%v\n", p.Command["method"], result["ok"])
	case "state.updated":
		var p struct {
			SessionID string `json:"sessionId"`
			Patch     struct {
				Status string `json:"status"`
				Mode   struct {
					Current string `json:"current"`
				} `json:"mode"`
			} `json:"patch"`
			Revision int `json:"revision"`
		}
		_ = json.Unmarshal(ev.Params, &p)
		if p.SessionID == "" {
			return
		}
		phoneSid := ps.phoneFor(p.SessionID)
		ps.mu.Lock()
		convID := ps.convListener
		convSub := ps.convSubscription
		ps.mu.Unlock()
		// Engine-initiated mode changes (e.g. the agent leaving plan mode via
		// ExitPlanMode) must flip the phone's mode picker too: mirror the
		// engine's mode onto collabMode and push a config patch. The phone's
		// option values are build(变更前确认)/edit(自动编辑)/plan/yolo — identity
		// mapping; engine-only "auto" shows as edit.
		if m := p.Patch.Mode.Current; m != "" {
			phoneMode := m
			if m == "auto" {
				phoneMode = "edit"
			}
			ps.mu.Lock()
			ps.collabMode = phoneMode
			ps.mu.Unlock()
			fmt.Printf("zcode: engine mode change %s -> phone %s (session=%s)\n", m, phoneMode, p.SessionID)
			if convID > 0 {
				if b, err := json.Marshal(conversationDeltaFrame(ps, phoneSid, convSub, ps.nextOrdinal(), []any{
					map[string]any{"op": "state.updated", "patch": map[string]any{"config": ps.modelCfg()}},
				})); err == nil {
					engine.SendChannelEvent(convID, b, sender.send)
				}
			}
		}
		if p.Patch.Status != "" && convID > 0 {
			// Engine statuses (running/completed/idle/error…) map onto the
			// projection's phase enum before pushing the control patch.
			phase, _ := phaseForStatus(displayStatus(p.Patch.Status))
			b, _ := json.Marshal(stateUpdatedFrame(ps, phoneSid, phase, convSub, ps.nextOrdinal()))
			engine.SendChannelEvent(convID, b, sender.send)
		}
	case "session/event":
		// Rich event stream pushed after session/subscribe: model.streaming
		// carries REAL text/reasoning/tool-input deltas, tool.updated carries
		// real results — the content parity channel (telemetry only counts).
		var p struct {
			Session string `json:"sessionId"`
		}
		if json.Unmarshal(ev.Params, &p) != nil || p.Session == "" {
			return
		}
		rawParams := map[string]any{}
		_ = json.Unmarshal(ev.Params, &rawParams)
		if phoneSid := ps.phoneFor(p.Session); phoneSid != "" {
			handleSessionEvent(engine, sender.send, ps, p.Session, phoneSid, rawParams)
		}
	case "v4/telemetry/event":
		// Telemetry drives everything the phone sees while a turn RUNS: tool
		// lifecycle becomes live tool cards, stream.chunk counters become
		// ticking 思考中/生成中 rows, turn.terminal finalizes the transcript.
		var p struct {
			Kind    string `json:"kind"`
			Channel string `json:"channel"`
			Session string `json:"sessionId"`
			Chunk   string `json:"chunk"`
			Status  string `json:"status"`
		}
		rawParams := map[string]any{}
		if json.Unmarshal(ev.Params, &p) != nil {
			return
		}
		_ = json.Unmarshal(ev.Params, &rawParams)
		if p.Kind == "tool.lifecycle" && p.Session != "" {
			if phoneSid := ps.phoneFor(p.Session); phoneSid != "" {
				handleLiveToolEvent(engine, sender.send, ps, p.Session, phoneSid, rawParams)
			}
		}
		if p.Kind == "stream.chunk" && p.Session != "" {
			// The telemetry chunk carries only a length (no body) — push a
			// ticking placeholder row instead of an empty-text row per chunk.
			if phoneSid := ps.phoneFor(p.Session); phoneSid != "" {
				handleLiveChunkEvent(engine, sender.send, ps, p.Session, phoneSid, rawParams)
			}
		}
		if p.Kind == "turn.terminal" && p.Session != "" {
			// The turn is over: flush the last streamed content as complete
			// rows, then drop the synthetic live rows (the transcript snapshot
			// below replaces them with the real rows).
			phoneSid := ps.phoneFor(p.Session)
			ps.mu.Lock()
			var deltas []any
			if lt := ps.live[p.Session]; lt != nil {
				deltas = lt.flushLiveTurn(ps.currentTurnIDLocked(phoneSid))
			}
			ps.endLiveTurn(p.Session)
			ps.mu.Unlock()
			pushLiveDeltas(engine, sender.send, ps, phoneSid, deltas)
			// The engine session may be a rebuilt continuation of a phone task;
			// update the phone-visible task and push under its id.
			go func(sid string) {
				ws, title := taskMeta(ps, sid)
				// Normalize the engine's terminal status to the display
				// vocabulary — "success" isn't recognized downstream and made
				// finished tasks show as 运行中 forever.
				st := "completed"
				switch p.Status {
				case "failed", "error":
					st = "failed"
				case "interrupted", "cancelled":
					st = "interrupted"
				}
				if ws != "" {
					if err := zcode.UpsertTask(ws, ws, sid, title, st); err != nil {
						fmt.Printf("zcode: task finalize failed: %v\n", err)
					}
				}
				// Landing list (项目 tabs) must reflect the new status.
				pushWorkspaceList(sender.send, ps)
			}(phoneSid)
			// The turn is over: queued submissions (sent while this turn was
			// running) may now dispatch.
			ps.setTurnRunning(p.Session, false)
			// Mirror the desktop's completed-state patch: control back to idle,
			// activeWorks cleared, follow-ups route startNow again. The
			// controller tasks-index is refreshed too so the sidebar's live
			// status leaves "running".
			go func(psid, csub string) {
				// One lock pass — listener() would re-lock the non-reentrant
				// mutex and deadlock this goroutine (wedging the bridge).
				ps.mu.Lock()
				convID := ps.convListener
				controllerID := ps.listeners["controllerFrame"]
				ps.mu.Unlock()
				if convID > 0 && psid != "" {
					b, _ := json.Marshal(stateUpdatedFrame(ps, psid, "completedSuccess", csub, ps.nextOrdinal()))
					engine.SendChannelEvent(convID, b, sender.send)
				}
				if controllerID > 0 {
					b, _ := json.Marshal(tasksIndexFrame(ps))
					engine.SendChannelEvent(controllerID, b, sender.send)
				}
			}(phoneSid, ps.convSub())
			// A turn finished: pull the full transcript (assistant text, tool
			// outputs like ls results) and push it as conversation rows so the
			// phone actually sees the reply. Run async — this goroutine IS the
			// stdout reader, and ReadSession must not block it (the reply
			// arrives on the same stream after this event).
			go syncConversation(engClient, engine, sender, ps, p.Session, phoneSid)
			// Auto-drain: dispatch the next queued submission once the final
			// transcript push has landed (syncConversation reads the engine
			// first, so give it a head start).
			go func(engSid, psid string) {
				time.Sleep(1800 * time.Millisecond)
				if ps.turnRunningFor(engSid) || engClient == nil {
					return
				}
				// Serial mode first: a queued NEW task may be waiting; only
				// drain the same-conversation queue when none is.
				if ps.queuedGlobalCount() > 0 {
					dispatchGlobalQueue(engClient, engine, sender.send, ps)
					return
				}
				q, ok := ps.popQueuedSend(psid)
				if !ok {
					return
				}
				if !engClient.SendMessage(engSid, q.text) {
					ps.enqueueSend(q) // engine gone — put it back
					return
				}
				ps.setTurnRunning(engSid, true)
				engClient.SubscribeSession(engSid)
				if zcode.TaskExists(psid) {
					_ = zcode.SetTaskStatus(psid, "running")
				}
				time.Sleep(200 * time.Millisecond)
				ps.mu.Lock()
				convID, convSub, ws := ps.convListener, ps.convSubscription, ps.workspacePath
				indexID := ps.indexListener
				ps.mu.Unlock()
				now := time.Now().UnixMilli()
				turnID := ps.beginTurn(psid)
				hdr := map[string]any{
					"rowId":        ps.nextRowID(),
					"turnId":       turnID,
					"createdAt":    now,
					"createdAtSeq": now,
					"kind":         "turnHeader",
					"origin":       "userInput",
					"state":        "running",
					"startedAt":    now,
				}
				row := map[string]any{
					"rowId":        ps.nextRowID(),
					"turnId":       turnID,
					"createdAt":    now,
					"createdAtSeq": now,
					"kind":         "userInput",
					"text":         q.text,
					"origin":       "realUser",
				}
				rows := append(ps.snapshotRows(), liveTailBoundary(ps.nextRowID(), turnID), hdr, row)
				ps.rememberRows(rows)
				if convID > 0 {
					frame := conversationSnapshotFrame(ps, psid, ws, convSub, "recovery", ps.nextOrdinal(), rows, ps.collabMode, "running")
					frame["queue"] = map[string]any{"items": ps.queueItemsPayload(), "autoDrain": true}
					b, _ := json.Marshal(frame)
					engine.SendChannelEvent(convID, b, sender.send)
					fmt.Printf("zcode: drained queued send session=%s text=%q\n", psid, q.text)
				}
				if indexID > 0 {
					ib, _ := json.Marshal(sessionsIndexFrame(ps))
					engine.SendChannelEvent(indexID, ib, sender.send)
				}
			}(p.Session, phoneSid)
			// Also give the serial queue a (later) chance in case the
			// per-conversation drain above didn't pick anything up.
			go func() {
				time.Sleep(4 * time.Second)
				if !ps.turnRunningFor(p.Session) {
					dispatchGlobalQueue(engClient, engine, sender.send, ps)
				}
			}()
		}
	}
}
