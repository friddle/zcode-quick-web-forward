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
		if ps.getPendingInteraction(interactionID) != nil {
			return // engine retry of an interaction we already surfaced
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
		if b, err := json.Marshal(conversationDeltaFrame(rq.SessionID, convSub, ps.nextOrdinal(), []any{
			map[string]any{"op": "state.updated", "patch": map[string]any{"pendingInteractions": ps.pendingInteractionsPayload()}},
		})); err == nil && convID > 0 {
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
			} `json:"patch"`
			Revision int `json:"revision"`
		}
		if json.Unmarshal(ev.Params, &p) == nil && p.SessionID != "" && p.Patch.Status != "" {
			ps.mu.Lock()
			convID := ps.convListener
			convSub := ps.convSubscription
			ps.mu.Unlock()
			phoneSid := ps.phoneFor(p.SessionID)
			if convID > 0 {
				// Engine statuses (running/completed/idle/error…) map onto the
				// projection's phase enum before pushing the control patch.
				phase, _ := phaseForStatus(displayStatus(p.Patch.Status))
				b, _ := json.Marshal(stateUpdatedFrame(phoneSid, phase, convSub, ps.nextOrdinal()))
				engine.SendChannelEvent(convID, b, sender.send)
			}
		}
	case "v4/telemetry/event":
		// stream.chunk carries the assistant's streaming text.
		var p struct {
			Kind    string `json:"kind"`
			Channel string `json:"channel"`
			Session string `json:"sessionId"`
			Chunk   string `json:"chunk"`
			Status  string `json:"status"`
		}
		if json.Unmarshal(ev.Params, &p) != nil {
			return
		}
		if p.Kind == "stream.chunk" && p.Channel == "text" && p.Session != "" {
			ps.mu.Lock()
			convID := ps.convListener
			convSub := ps.convSubscription
			ps.mu.Unlock()
			phoneSid := ps.phoneFor(p.Session)
			if convID > 0 {
				b, _ := json.Marshal(conversationChunkFrame(phoneSid, p.Chunk, convSub, ps.nextOrdinal()))
				engine.SendChannelEvent(convID, b, sender.send)
			}
		}
		if p.Kind == "turn.terminal" && p.Session != "" {
			// The engine session may be a rebuilt continuation of a phone task;
			// update the phone-visible task and push under its id.
			phoneSid := ps.phoneFor(p.Session)
			st := p.Status
			if st != "success" && st != "interrupted" && st != "failed" {
				st = "completed"
			}
			go func(sid string) {
				ws, title := taskMeta(ps, sid)
				if ws != "" {
					if err := zcode.UpsertTask(ws, ws, sid, title, st); err != nil {
						fmt.Printf("zcode: task finalize failed: %v\n", err)
					}
				}
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
					b, _ := json.Marshal(stateUpdatedFrame(psid, "completedSuccess", csub, ps.nextOrdinal()))
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
				q, ok := ps.popQueuedSend(psid)
				if !ok {
					return
				}
				if !engClient.SendMessage(engSid, q.text) {
					ps.enqueueSend(q) // engine gone — put it back
					return
				}
				ps.setTurnRunning(engSid, true)
				if zcode.TaskExists(psid) {
					_ = zcode.SetTaskStatus(psid, "running")
				}
				time.Sleep(200 * time.Millisecond)
				ps.mu.Lock()
				convID, convSub, ws := ps.convListener, ps.convSubscription, ps.workspacePath
				indexID := ps.indexListener
				ps.mu.Unlock()
				now := time.Now().UnixMilli()
				turnID := "turn-" + psid
				hdr := map[string]any{
					"rowId":        1,
					"turnId":       turnID,
					"createdAt":    now,
					"createdAtSeq": 1,
					"kind":         "turnHeader",
					"origin":       "userInput",
					"state":        "running",
					"startedAt":    now,
				}
				row := map[string]any{
					"rowId":        2,
					"turnId":       turnID,
					"createdAt":    now,
					"createdAtSeq": 2,
					"kind":         "userInput",
					"text":         q.text,
					"origin":       "realUser",
				}
				rows := append(ps.snapshotRows(), []any{hdr, row}...)
				ps.rememberRows(rows)
				if convID > 0 {
					frame := conversationSnapshotFrame(ps, psid, ws, convSub, "recovery", ps.nextOrdinal(), rows, ps.collabMode, "running")
					frame["queue"] = map[string]any{"items": ps.queueItemsPayload(), "autoDrain": true}
					b, _ := json.Marshal(frame)
					engine.SendChannelEvent(convID, b, sender.send)
					fmt.Printf("zcode: drained queued send session=%s text=%q\n", psid, q.text)
				}
				if indexID > 0 {
					ib, _ := json.Marshal(sessionsIndexFrame(convSub, ps))
					engine.SendChannelEvent(indexID, ib, sender.send)
				}
			}(p.Session, phoneSid)
		}
	}
}
