// Conversation commands (sendConversationCommandV4): createSession,
// sendText (queueing, historical-session rebuild), model/mode switches,
// deleteSession and AskUserQuestion resolveInteraction.

package main

import (
	"encoding/json"
	"fmt"
	"time"

	enginepkg "github.com/friddle/zcode-quick-web-forward/internal/engine"
	"github.com/friddle/zcode-quick-web-forward/internal/relay"
	"github.com/friddle/zcode-quick-web-forward/internal/zcode"
)

// bridgeSendCommand bridges a phone conversation command (createSession /
// sendText) to the real engine. Returns the ack to send back to the phone.
func bridgeSendCommand(c *relay.ChannelCall, engClient *enginepkg.Client, ps *phoneSessions, workspaces []string, engine *relay.BridgeEngine, send func(any)) map[string]any {
	ack := map[string]any{
		"commandId":          uuidNew(),
		"status":             "accepted",
		"revisionAtDecision": 0,
	}
	// Parse the phone envelope: {workspacePath, envelope:{commandId, sessionId, type, payload}}.
	var arg []byte
	switch a := c.Arg.(type) {
	case json.RawMessage:
		arg = a
	case map[string]any:
		arg, _ = json.Marshal(a)
	case []any:
		// phone wraps args as Array(1){object}
		arg, _ = json.Marshal(a)
	default:
		arg, _ = json.Marshal(c.Arg)
	}
	var req struct {
		Envelope struct {
			CommandID string `json:"commandId"`
			ClientID  string `json:"clientId"`
			SessionID string `json:"sessionId"`
			Type      string `json:"type"`
			Payload   struct {
				WorkspaceID string `json:"workspaceId"`
				FirstInput  struct {
					Text string `json:"text"`
				} `json:"firstInput"`
				Text          string `json:"text"`
				Provider      string `json:"provider"`
				Model         string `json:"model"`
				Thought       string `json:"thought"`
				Mode          string `json:"mode"`
				InteractionID string `json:"interactionId"`
				Answer        struct {
					OptionID string         `json:"optionId"`
					FreeText string         `json:"freeText"`
					Action   string         `json:"action"`
					Content  map[string]any `json:"content"`
				} `json:"answer"`
				Config struct {
					Provider string `json:"provider"`
					Model    string `json:"model"`
				} `json:"config"`
			} `json:"payload"`
		} `json:"envelope"`
	}
	if json.Unmarshal(arg, &req) != nil {
		fmt.Printf("zcode: bridgeSendCommand unmarshal failed arg=%s\n", arg)
		return ack
	}
	fmt.Printf("zcode: bridgeSendCommand raw=%s\n", arg)
	if req.Envelope.CommandID != "" {
		ack["commandId"] = req.Envelope.CommandID
	}
	if req.Envelope.ClientID != "" {
		ack["clientId"] = req.Envelope.ClientID
	}

	switch req.Envelope.Type {
	case "createSession":
		ws := req.Envelope.Payload.WorkspaceID
		if ws == "" && len(workspaces) > 0 {
			ws = workspaces[0]
		}
		// Ask the engine to create a real session. Prefer the config's default
		// model (model.main) over the phone's cached selection — the phone may
		// still carry a model whose key is out of balance, and the config is
		// the authoritative default.
		provider := req.Envelope.Payload.Config.Provider
		model := req.Envelope.Payload.Config.Model
		if defP, defM := zcode.DefaultModel(); defP != "" && defM != "" {
			provider, model = defP, defM
		}
		res, err := engClient.CreateSession(ws, ws, provider, model, 15*time.Second)
		if err != nil {
			fmt.Printf("zcode: engine createSession failed: %v\n", err)
			ack["status"] = "failed"
			ack["message"] = err.Error()
			return ack
		}
		sid, _ := res["sessionId"].(string)
		if sid == "" {
			// fall back to whatever the engine returned
			if s, ok := res["session"].(map[string]any); ok {
				sid, _ = s["sessionId"].(string)
			}
		}
		ps.setSession(sid, ws)
		ps.setModelConfig(provider, model, "")
		title := req.Envelope.Payload.FirstInput.Text
		if title == "" {
			title = req.Envelope.Payload.Text
		}
		typedTitle := title // "" for a bare draft — see below
		if title == "" {
			title = "新任务"
		}
		ps.runtimeTask(sid, ws, title, typedTitle == "")
		// Persist to the real task index so the task survives reconnects and
		// restarts — but only when the user actually typed something. The web
		// client fires a bare createSession (no firstInput) for every fresh
		// composer / page load; persisting those drafts littered the task list
		// with phantom 新任务 entries. Bare drafts are persisted on their
		// first sendText instead (see the sendText case).
		if typedTitle != "" {
			if err := zcode.UpsertTask(ws, ws, sid, title, "running"); err != nil {
				fmt.Printf("zcode: task persist failed: %v\n", err)
			} else {
				fmt.Printf("zcode: task persisted %s\n", sid)
			}
		}
		ack["result"] = map[string]any{
			"type":      "createSession",
			"sessionId": sid,
		}
		fmt.Printf("zcode: engine created session %s title=%q\n", sid, title)
		// Official semantics: a createSession carrying firstInput starts the
		// turn immediately — the text is the user's first message, not just a
		// title. Dropping it left the phone stuck on "sending".
		if typedTitle != "" && sid != "" {
			if !engClient.SendMessage(sid, typedTitle) {
				ack["status"] = "failed"
				ack["message"] = "engine stdin closed"
			} else {
				ps.setTurnRunning(sid, true)
				fmt.Printf("zcode: engine session/send %s (firstInput) text=%q\n", sid, typedTitle)
				ack["userTextSent"] = typedTitle
			}
		}
	case "sendText", "":
		sid := req.Envelope.SessionID
		if sid == "" {
			sid, _ = ps.get()
		}
		text := req.Envelope.Payload.Text
		if text == "" {
			text = req.Envelope.Payload.FirstInput.Text
		}
		if sid != "" && text != "" {
			// A pending AskUserQuestion blocks the engine mid-turn: a typed
			// composer message is the user's answer, not a new task.
			if pi := ps.oldestPendingInteractionFor(sid); pi != nil {
				resolveInteractionCommand(engClient, engine, send, ps, pi.InteractionID, "", text, "accept", nil)
				ack["status"] = "accepted"
				if cmdID := req.Envelope.CommandID; cmdID != "" {
					ack["result"] = map[string]any{"type": "inputAccepted", "delivery": "startNow", "inputId": cmdID}
				}
				return ack
			}
			// While a turn is still running the submission must QUEUE (the
			// desktop shows it as a waiting bubble and dispatches it when the
			// turn ends). Sending straight through would interleave into the
			// running turn.
			if ps.turnRunningFor(ps.engineFor(sid)) {
				q := queuedSend{
					text:            text,
					sourceCommandID: req.Envelope.CommandID,
					clientID:        req.Envelope.ClientID,
					admittedAt:      time.Now().UnixMilli(),
					sessionId:       sid,
				}
				if q.sourceCommandID == "" {
					q.sourceCommandID = uuidNew()
				}
				q.queueItemID = uuidNew()
				ps.enqueueSend(q)
				ack["status"] = "accepted"
				ack["queued"] = true
				// Official ack shape: inputAccepted with the queue delivery.
				ack["result"] = map[string]any{
					"type": "inputAccepted", "delivery": "queue", "inputId": q.sourceCommandID,
				}
				fmt.Printf("zcode: sendText queued session=%s text=%q\n", sid, text)
				go func(sid string) {
					time.Sleep(150 * time.Millisecond)
					ps.mu.Lock()
					convID, convSub, ws := ps.convListener, ps.convSubscription, ps.workspacePath
					ps.mu.Unlock()
					if convID == 0 {
						return
					}
					frame := conversationSnapshotFrame(ps, sid, ws, convSub, "recovery", ps.nextOrdinal(), ps.snapshotRows(), ps.collabMode, "running")
					frame["queue"] = map[string]any{"items": ps.queueItemsPayload(), "autoDrain": true}
					b, _ := json.Marshal(frame)
					engine.SendChannelEvent(convID, b, send)
					fmt.Printf("zcode: pushed queue snapshot session=%s pending=%d\n", sid, len(ps.queueItemsPayload()))
				}(sid)
				return ack
			}
			// The phone may send into a historical task whose engine session
			// died on a daemon restart. Resolve to the live engine session; if
			// none is active, rebuild one and continue from the saved
			// transcript so the command actually executes.
			engineSid := ps.engineFor(sid)
			if engineSid == sid {
				if _, err := engClient.ReadSession(sid, 3*time.Second); err != nil {
					engineSid = rebuildContinuedSession(engClient, ps, sid)
				}
			}
			if engineSid == "" {
				ack["status"] = "failed"
				ack["message"] = "无法恢复该历史任务的会话"
			} else {
				if !engClient.SendMessage(engineSid, text) {
					ack["status"] = "failed"
					ack["message"] = "engine stdin closed"
				}
				ps.setTurnRunning(engineSid, true)
				fmt.Printf("zcode: engine session/send %s (phone=%s) text=%q\n", engineSid, sid, text)
				ack["userTextSent"] = text
				if cmdID := req.Envelope.CommandID; cmdID != "" {
					ack["result"] = map[string]any{
						"type": "inputAccepted", "delivery": "startNow", "inputId": cmdID,
					}
				}
				// First real message in a bare draft: persist the task now.
				// (Drafts stay runtime-only until this point — see createSession.)
				if !zcode.TaskExists(sid) {
					ws, _ := taskMeta(ps, sid)
					if ws == "" {
						ps.mu.Lock()
						ws = ps.workspacePath
						ps.mu.Unlock()
					}
					if ws == "" && len(workspaces) > 0 {
						ws = workspaces[0]
					}
					if err := zcode.UpsertTask(ws, ws, sid, text, "running"); err != nil {
						fmt.Printf("zcode: task persist failed: %v\n", err)
					} else {
						fmt.Printf("zcode: task persisted %s (first send)\n", sid)
					}
					// Promote the runtime entry so the row shows in the list.
					ps.runtimeTask(sid, ws, text, false)
				} else {
					// A follow-up re-activates the task — flip it back to
					// running so the sidebar/waiting display reflects it.
					if err := zcode.SetTaskStatus(sid, "running"); err != nil {
						fmt.Printf("zcode: task status sync failed: %v\n", err)
					}
				}
			}
		}
	case "switchModelConfig":
		// Switch the current session's model. The payload is
		// {provider, model, thought} at the top level.
		sid := req.Envelope.SessionID
		if sid == "" {
			sid, _ = ps.get()
		}
		provider := req.Envelope.Payload.Provider
		model := req.Envelope.Payload.Model
		if sid != "" && provider != "" && model != "" {
			body := map[string]any{
				"sessionId":                  sid,
				"model":                      map[string]any{"providerId": provider, "modelId": model},
				"persistAsWorkspaceLastUsed": true,
			}
			if engClient.Write(map[string]any{"id": 0, "method": "session/setModel", "params": body}) {
				fmt.Printf("zcode: engine switchModelConfig session=%s provider=%s model=%s\n", sid, provider, model)
			}
			// Reasoning effort travels as its own engine call — the desktop
			// sends session/setThoughtLevel with the picker's value.
			if thought := req.Envelope.Payload.Thought; thought != "" {
				if engClient.Write(map[string]any{"id": 0, "method": "session/setThoughtLevel", "params": map[string]any{
					"sessionId": sid, "thoughtLevel": thought,
				}}) {
					fmt.Printf("zcode: engine setThoughtLevel session=%s level=%s\n", sid, thought)
				}
			}
			ps.setModelConfig(provider, model, req.Envelope.Payload.Thought)
			// Mirror the desktop: a model switch appends a timelineMarker row
			// (modelChange) and updates the projection's config block.
			go func(sessionID, fromP, fromM, toP, toM, toT string) {
				time.Sleep(300 * time.Millisecond)
				ps.mu.Lock()
				convID, convSub := ps.convListener, ps.convSubscription
				ps.mu.Unlock()
				if convID == 0 {
					return
				}
				now := time.Now().UnixMilli()
				rowID := ps.nextRowID()
				marker := map[string]any{
					"rowId": rowID, "turnId": "turn-" + sessionID,
					"entityId":  fmt.Sprintf("model-change:%d:%s/%s->%s/%s", now, fromP, fromM, toP, toM),
					"createdAt": now, "createdAtSeq": rowID,
					"kind": "timelineMarker", "lane": "lightBoundary",
					"marker": map[string]any{
						"type":         "modelChange",
						"fromProvider": fromP, "fromModel": fromM,
						"toProvider": toP, "toModel": toM, "toThought": toT,
					},
				}
				b, _ := json.Marshal(conversationDeltaFrame(sessionID, convSub, ps.nextOrdinal(), []any{
					map[string]any{"op": "row.appended", "row": marker},
					map[string]any{"op": "state.updated", "patch": map[string]any{"config": ps.modelCfg()}},
				}))
				engine.SendChannelEvent(convID, b, send)
				fmt.Printf("zcode: pushed model-change marker session=%s -> %s/%s thought=%s\n", sessionID, toP, toM, toT)
			}(sid, "", "", provider, model, req.Envelope.Payload.Thought)
		}
	case "switchCollaborationMode":
		// The phone's mode picker (变更前确认/自动编辑/计划模式/完全访问) sends
		// {mode: confirm|edit|plan|yolo}. Forward it to the engine's
		// session/setMode so it actually takes effect.
		sid := req.Envelope.SessionID
		if sid == "" {
			sid, _ = ps.get()
		}
		mode := req.Envelope.Payload.Mode
		// The engine's session/setMode enum is plan|build|edit|yolo|auto; the
		// phone's "confirm" (变更前确认) maps to "build". Unknown → build.
		switch mode {
		case "edit", "plan", "yolo":
		case "confirm":
			mode = "build"
		default:
			mode = "build"
		}
		if sid != "" {
			if engClient.SetMode(sid, mode) {
				fmt.Printf("zcode: engine setMode session=%s mode=%s\n", sid, mode)
			}
			// Remember the phone's selected mode so permission requests below
			// can decide whether to auto-approve or ask.
			ps.mu.Lock()
			ps.collabMode = mode
			ps.mu.Unlock()
			ack["modeChanged"] = mode
		}
	case "deleteSession":
		// The client deletes a task by sending this conversation command.
		// Close the engine session and drop the task from the index + runtime
		// list, then refresh the sidebar.
		sid := req.Envelope.SessionID
		if sid == "" {
			sid, _ = ps.get()
		}
		if sid != "" {
			engineSid := ps.engineFor(sid)
			engClient.Write(map[string]any{"id": 0, "method": "session/close", "params": map[string]any{"sessionId": engineSid}})
			deleted := true
			if err := zcode.SetTaskFlags(sid, &deleted, nil, nil); err != nil {
				fmt.Printf("zcode: deleteSession task flag failed: %v\n", err)
			}
			ps.removeRuntimeTask(sid)
			ack["status"] = "accepted"
			ack["result"] = map[string]any{"type": "deleteSession", "sessionId": sid}
			fmt.Printf("zcode: deleteSession session=%s (engine=%s closed)\n", sid, engineSid)
			go func() {
				time.Sleep(200 * time.Millisecond)
				ps.mu.Lock()
				controllerID := ps.listeners["controllerFrame"]
				indexID := ps.indexListener
				ps.mu.Unlock()
				if controllerID > 0 {
					b, _ := json.Marshal(tasksIndexFrame(ps))
					engine.SendChannelEvent(controllerID, b, send)
				}
				if indexID > 0 {
					b, _ := json.Marshal(sessionsIndexFrame(ps.convSub(), ps))
					engine.SendChannelEvent(indexID, b, send)
				}
			}()
		}
	case "resolveInteraction":
		// The phone answered an AskUserQuestion (pendingInteraction).
		answer := req.Envelope.Payload.Answer
		ack, done := resolveInteractionCommand(engClient, engine, send, ps, req.Envelope.Payload.InteractionID, answer.OptionID, answer.FreeText, answer.Action, answer.Content)
		if !done {
			return ack
		}
	default:
		fmt.Printf("zcode: engine command type %q unhandled (ignored)\n", req.Envelope.Type)
	}
	return ack
}

// answerContentFlattens the client's answer content. The official client sends
// {answers:{question:choice}, answer_0:choice, answer:choice} — normalize it
// to question→choice pairs the engine's AskUserQuestion expects.
func flattenAnswerContent(content map[string]any) map[string]string {
	flat := map[string]string{}
	if raw, ok := content["answers"].(map[string]any); ok {
		for q, a := range raw {
			if s, ok := a.(string); ok {
				flat[q] = s
			}
		}
	}
	for k, v := range content {
		if k == "answers" {
			continue
		}
		if s, ok := v.(string); ok && s != "" {
			flat[k] = s // e.g. answer_0 / answer — matched per question below
		}
	}
	return flat
}

// resolveInteractionCommand delivers the user's answer for a pending
// AskUserQuestion to the engine and refreshes the projection. done=false means
// the interaction no longer exists (already answered / stale).
func resolveInteractionCommand(engClient *enginepkg.Client, engine *relay.BridgeEngine, send func(any), ps *phoneSessions, interactionID, optionID, freeText, action string, content map[string]any) (map[string]any, bool) {
	ack := map[string]any{}
	pi := ps.getPendingInteraction(interactionID)
	if pi == nil {
		ack["status"] = "failed"
		ack["message"] = "该问题已失效（可能已回答或会话已重建）"
		return ack, false
	}
	// One answer value per question: the flattened content (question→choice)
	// wins, then the chosen option's label, then free text.
	flat := flattenAnswerContent(content)
	answers := map[string]any{}
	for i, q := range pi.Questions {
		qn, _ := q["question"].(string)
		if qn == "" {
			continue
		}
		if v, ok := flat[qn]; ok && v != "" {
			answers[qn] = v
			continue
		}
		if optionID != "" {
			opts, _ := q["options"].([]map[string]any)
			label := ""
			for _, o := range opts {
				if o["optionId"] == optionID {
					label, _ = o["label"].(string)
					break
				}
			}
			if label != "" {
				answers[qn] = label
				continue
			}
		}
		if freeText != "" {
			answers[qn] = freeText
			continue
		}
		// answer_0/answer style fallbacks from the official client.
		for _, k := range []string{fmt.Sprintf("answer_%d", i), "answer"} {
			if v, ok := flat[k]; ok && v != "" {
				answers[qn] = v
				break
			}
		}
	}
	// The engine parses the response with userInputResponseToBrokerResult
	// (JAo): {action:"accept", content:{answers:{question:choice}}} → the
	// tool input gets the answers merged in; any other action is a denial.
	var brokerResult map[string]any
	if action == "decline" || action == "cancel" || len(answers) == 0 {
		act := action
		if act == "" {
			act = "cancel"
		}
		brokerResult = map[string]any{"action": act}
	} else {
		content := map[string]any{"answers": answers}
		for _, a := range answers {
			content["answer"] = a // single-question convenience mirror
			break
		}
		brokerResult = map[string]any{"action": "accept", "content": content}
	}
	engClient.RespondToRequest(pi.EngineReqID, brokerResult)
	ps.removePendingInteraction(interactionID)
	fmt.Printf("zcode: interaction %s answered (action=%s answers=%d)\n", interactionID, brokerResult["action"], len(answers))

	// Refresh the projection: question row flips to answered, the
	// pendingInteractions list drops the entry.
	ps.mu.Lock()
	convID, convSub := ps.convListener, ps.convSubscription
	ps.mu.Unlock()
	if convID > 0 {
		now := time.Now().UnixMilli()
		answerText := ""
		for _, v := range answers {
			answerText, _ = v.(string)
		}
		deltas := []any{
			map[string]any{"op": "state.updated", "patch": map[string]any{"pendingInteractions": ps.pendingInteractionsPayload()}},
		}
		if pi.RowID > 0 {
			deltas = append([]any{map[string]any{"op": "row.upserted", "row": map[string]any{
				"rowId": pi.RowID, "entityId": pi.ToolCallID,
				"kind": "toolCall", "status": "success",
				"inputText": pi.Prompt,
				"output":    map[string]any{"text": answerText},
				"endedAt":   now,
			}}}, deltas...)
		}
		if b, err := json.Marshal(conversationDeltaFrame(pi.SessionID, convSub, ps.nextOrdinal(), deltas)); err == nil {
			engine.SendChannelEvent(convID, b, send)
		}
	}
	ack["status"] = "accepted"
	ack["result"] = map[string]any{"type": "resolveInteraction", "interactionId": interactionID}
	return ack, true
}
