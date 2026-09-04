// Conversation transcript sync: subscription/snapshot pushes, engine
// session/read pulls, transcript persistence, and the official row model
// (turnHeader / userInput / assistantText / reasoning / toolCall).

package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	enginepkg "github.com/friddle/zcode-quick-web-forward/internal/engine"
	"github.com/friddle/zcode-quick-web-forward/internal/relay"
	"github.com/friddle/zcode-quick-web-forward/internal/zcode"
)

// pushSubscriptionFrames pushes the sessions-index + conversation snapshot so
// the phone's subscription goes live (otherwise it resync-loops).
func pushSubscriptionFrames(engine *relay.BridgeEngine, send func(any), ps *phoneSessions, c *relay.ChannelCall, engClient *enginepkg.Client) {
	time.Sleep(300 * time.Millisecond)
	ps.mu.Lock()
	indexID, runtimeID, convSub := ps.indexListener, ps.runtimeListener, ps.convSubscription
	ps.mu.Unlock()
	if indexID > 0 {
		b, _ := json.Marshal(sessionsIndexFrame(convSub, ps))
		engine.SendChannelEvent(indexID, b, send)
		fmt.Println("zcode: pushed sessions-index snapshot")
	}
	if runtimeID > 0 {
		b, _ := json.Marshal(map[string]any{"workspaceKey": "local", "state": "available"})
		engine.SendChannelEvent(runtimeID, b, send)
	}
	// If this was a resync, the phone waits for a recovery-delivery frame with
	// the full transcript so it renders the conversation history. When the
	// phone opens a specific session (old task from the list) we also push its
	// history so the conversation is restored, not left empty.
	if (c.Name == "resyncConversationV4" || c.Name == "resyncSessionsIndexV4" ||
		c.Name == "subscribeConversationV4") && (c.Name != "subscribeSessionsIndexV4") {
		sid, _ := ps.get()
		if sid != "" {
			rows := []any{}
			if engClient != nil {
				if tx, err := engClient.ReadSession(sid, 10*time.Second); err == nil {
					rows = messageRows(tx, sid, ps.nextOrdinal)
					fmt.Printf("zcode: recovery read session=%s rows=%d\n", sid, len(rows))
				} else if stored := zcode.LoadSessionTranscript(sid); stored != nil {
					// Engine session gone (daemon restarted): restore from the
					// transcript snapshot we saved when the turn completed.
					rows = messageRows(stored, sid, ps.nextOrdinal)
					fmt.Printf("zcode: recovery from transcript session=%s rows=%d (engine: %v)\n", sid, len(rows), err)
				} else {
					fmt.Printf("zcode: recovery read failed: %v (no transcript)\n", err)
				}
			}
			// workspacePath: prefer the one persisted for this task in sqlite.
			ws := ps.workspacePath
			if tasks, err := zcode.ListTasks("", ""); err == nil {
				for _, t := range tasks {
					if t.TaskID == sid && t.WorkspacePath != "" {
						ws = t.WorkspacePath
						break
					}
				}
			}
			b, _ := json.Marshal(conversationSnapshotFrame(ps, sid, ws, convSub, "recovery", ps.nextOrdinal(), rows, ps.collabMode, phaseForSession(ps, sid)))
			engine.SendChannelEvent(ps.convListener, b, send)
			fmt.Printf("zcode: pushed recovery snapshot session=%s rows=%d\n", sid, len(rows))
		}
	}
}

// pushConversationFrame pushes an initial conversation snapshot for the opened
// session so the phone renders the (empty) conversation instead of waiting.
func pushConversationFrame(engine *relay.BridgeEngine, send func(any), ps *phoneSessions, ack map[string]any) {
	time.Sleep(500 * time.Millisecond)
	sess, ok := ack["result"].(map[string]any)
	if !ok {
		return
	}
	sessionID, _ := sess["sessionId"].(string)
	ps.mu.Lock()
	convID, ws, convSub := ps.convListener, ps.workspacePath, ps.convSubscription
	ps.mu.Unlock()
	if convID == 0 {
		return
	}
	frame := conversationSnapshotFrame(ps, sessionID, ws, convSub, "initial", ps.nextOrdinal(), nil, ps.collabMode, phaseForSession(ps, sessionID))
	b, _ := json.Marshal(frame)
	engine.SendChannelEvent(convID, b, send)
	fmt.Printf("zcode: pushed conversation snapshot session=%s\n", sessionID)
}

// syncConversation pulls the engine session transcript and pushes each message
// as a conversation delta so the phone renders the full reply (assistant text +
// tool outputs). Called when a turn.terminal event fires. engineSid is the live
// engine session (may be a rebuilt continuation); phoneSid is the task id the
// phone displays (may differ when resuming a historical task).
func syncConversation(engClient *enginepkg.Client, engine *relay.BridgeEngine, sender *relaySender, ps *phoneSessions, engineSid, phoneSid string) {
	fmt.Printf("zcode: syncConversation enter engine=%s phone=%s\n", engineSid, phoneSid)
	if phoneSid == "" {
		phoneSid = engineSid
	}
	ps.mu.Lock()
	convID := ps.convListener
	convSub := ps.convSubscription
	ps.mu.Unlock()
	if convID == 0 {
		return
	}
	tx, err := engClient.ReadSession(engineSid, 10*time.Second)
	if err != nil {
		fmt.Printf("zcode: syncConversation read failed: %v\n", err)
		return
	}
	// The session/read reply carries the session's real model settings,
	// reasoning-effort options and context usage — feed the projection with
	// them instead of static defaults.
	if s, ok := tx["session"].(map[string]any); ok {
		if m, ok := s["model"].(map[string]any); ok {
			pi, _ := m["providerId"].(string)
			mi, _ := m["modelId"].(string)
			ps.setModelConfig(pi, mi, "")
		}
		if t, _ := s["title"].(string); t != "" {
			if ws, _ := s["workspace"].(map[string]any); ws != nil {
				if wp, _ := ws["workspacePath"].(string); wp != "" {
					if err := zcode.UpsertTask(wp, wp, phoneSid, t, "completed"); err != nil {
						fmt.Printf("zcode: task title sync failed: %v\n", err)
					} else {
						fmt.Printf("zcode: task title synced %s title=%q\n", phoneSid, t)
					}
				}
			}
		}
	}
	if st, ok := tx["settings"].(map[string]any); ok {
		if tl, ok := st["thoughtLevel"].(map[string]any); ok {
			cur, _ := tl["current"].(string)
			ps.setModelConfig("", "", cur)
			if opts, ok := tl["available"].([]any); ok && len(opts) > 0 {
				levels := make([]string, 0, len(opts))
				for _, o := range opts {
					if om, _ := o.(map[string]any); om != nil {
						if v, _ := om["value"].(string); v != "" {
							levels = append(levels, v)
						}
					}
				}
				ps.setThoughtLevels(levels)
			}
		}
	}
	if rt, ok := tx["runtime"].(map[string]any); ok {
		if cu, ok := rt["contextUsage"].(map[string]any); ok {
			used, _ := cu["used"].(float64)
			size, _ := cu["size"].(float64)
			if used > 0 || size > 0 {
				ps.setContextUsage(int64(used), int64(size))
			}
		}
	}
	// Snapshot the transcript to disk so the phone can reopen this task's
	// history even after the engine restarts (its sessions are in-memory).
	if saved, _ := json.Marshal(tx); len(saved) > 0 {
		if err := zcode.SaveSessionTranscript(phoneSid, tx); err != nil {
			fmt.Printf("zcode: transcript save failed: %v\n", err)
		} else {
			fmt.Printf("zcode: transcript saved session=%s bytes=%d\n", phoneSid, len(saved))
		}
	}
	// Persist the engine-generated session title (engine titles the task, e.g.
	// "List files in /home/friddle") so the phone list shows real names.
	// (Handled above while reading tx["session"] for the model settings.)
	rows := messageRows(tx, phoneSid, ps.nextOrdinal)
	if len(rows) == 0 {
		fmt.Printf("zcode: syncConversation empty rows session=%s\n", phoneSid)
		return
	}
	// The turn just ended: the projection must leave "running" — the live
	// session stays open (follow-ups), so it completes, not drafts.
	phase := "completedSuccess"
	if ps.turnRunningFor(ps.engineFor(phoneSid)) {
		phase = "running"
	}
	b, _ := json.Marshal(conversationSnapshotFrame(ps, phoneSid, ps.workspacePath, convSub, "recovery", ps.nextOrdinal(), rows, ps.collabMode, phase))
	ps.rememberRows(rows)
	engine.SendChannelEvent(convID, b, sender.send)
	fmt.Printf("zcode: synced conversation engine=%s phone=%s rows=%d\n", engineSid, phoneSid, len(rows))
}

// messageRows converts a session/read transcript into conversation snapshot
// rows using the phone's official row model (turnHeader / userInput /
// assistantText / reasoning / toolCall — see the client's row zod union), so
// the UI renders thinking, tool cards and answers like the desktop app.
func messageRows(tx map[string]any, sessionID string, ordinal func() int) []any {
	var msgs []any
	if m, ok := tx["messages"].([]any); ok {
		msgs = m
	} else if m, ok := tx["rows"].([]any); ok {
		msgs = m
	}
	out := make([]any, 0, len(msgs)*2)
	rowID := 0
	turn := 0
	now := time.Now().UnixMilli()
	newRow := func(turnID string, ts int64) map[string]any {
		rowID++
		if ts <= 0 {
			ts = now
		}
		return map[string]any{
			"rowId":        rowID,
			"turnId":       turnID,
			"createdAt":    ts,
			"createdAtSeq": rowID,
		}
	}
	// legacy flat rows (kind userText/assistantText + text, no parts)
	for _, raw := range msgs {
		m, _ := raw.(map[string]any)
		if m == nil {
			continue
		}
		if _, hasParts := m["parts"]; hasParts {
			continue
		}
		kind, _ := m["kind"].(string)
		content, _ := m["content"].(string)
		text, _ := m["text"].(string)
		if content == "" {
			content = text
		}
		if content == "" {
			continue
		}
		rowKind := "assistantText"
		if kind == "userText" || kind == "userInput" {
			rowKind = "userInput"
		}
		rowID++
		out = append(out, map[string]any{
			"rowId":               rowID,
			"turnId":              "turn-" + sessionID,
			"createdAt":           now,
			"createdAtSeq":        rowID,
			"kind":                rowKind,
			"assistantResponseId": "ar-" + sessionID,
			"text":                content,
			"state":               "complete",
		})
	}
	turnID := func() string {
		if turn < 1 {
			turn = 1
		}
		return fmt.Sprintf("turn-%s-%d", shortSessionID(sessionID), turn)
	}
	for _, raw := range msgs {
		m, _ := raw.(map[string]any)
		if m == nil {
			continue
		}
		parts, hasParts := m["parts"].([]any)
		if !hasParts {
			continue // legacy flat row — handled above
		}
		role, _ := m["role"].(string)
		info, _ := m["info"].(map[string]any)
		ts := int64(0)
		endedAt := int64(0)
		if tm, ok := info["time"].(map[string]any); ok {
			if c, ok := tm["created"].(float64); ok {
				ts = int64(c)
			}
			if c, ok := tm["completed"].(float64); ok {
				endedAt = int64(c)
				if ts == 0 {
					ts = endedAt
				}
			}
		}
		resp, _ := info["messageId"].(string)
		if resp == "" {
			resp = "ar-" + sessionID
		}
		if role == "user" {
			turn++
			text := collectEngineText(parts)
			// Stable per-turn command id — the official rows always carry the
			// originating sendConversationCommandV4 commandId chain.
			cmdID := fmt.Sprintf("cmd-%s-%d", shortSessionID(sessionID), turn)
			ended := endedAt
			if ended == 0 {
				ended = ts
			}
			h := newRow(turnID(), ts)
			h["kind"] = "turnHeader"
			h["origin"] = "userInput"
			h["executionKind"] = "agent"
			h["state"] = "completedSuccess"
			h["startedAt"] = ts
			h["endedAt"] = ended
			if ended > ts {
				h["activeMs"] = ended - ts
			}
			h["historyRoundCount"] = turn
			h["sourceCommandId"] = cmdID
			out = append(out, h)
			if text != "" {
				r := newRow(turnID(), ts)
				r["kind"] = "userInput"
				r["text"] = text
				r["origin"] = "realUser"
				r["sourceCommandId"] = cmdID
				r["rootSourceCommandId"] = cmdID
				out = append(out, r)
			}
			continue
		}
		// assistant message: map part runs to reasoning / assistantText /
		// toolCall rows, preserving order.
		var textRun, reasonRun []string
		flushText := func() {
			if len(textRun) == 0 {
				return
			}
			r := newRow(turnID(), ts)
			r["kind"] = "assistantText"
			r["assistantResponseId"] = resp
			r["text"] = strings.Join(textRun, "\n")
			r["state"] = "complete"
			out = append(out, r)
			textRun = nil
		}
		flushReasoning := func() {
			if len(reasonRun) == 0 {
				return
			}
			r := newRow(turnID(), ts)
			r["kind"] = "reasoning"
			r["assistantResponseId"] = resp
			r["text"] = strings.Join(reasonRun, "\n")
			r["state"] = "complete"
			out = append(out, r)
			reasonRun = nil
		}
		for _, p := range parts {
			pm, _ := p.(map[string]any)
			if pm == nil {
				continue
			}
			switch pm["type"] {
			case "reasoning":
				flushText()
				txt, _ := pm["text"].(string)
				if txt != "" {
					reasonRun = append(reasonRun, txt)
				}
			case "text":
				flushReasoning()
				txt, _ := pm["text"].(string)
				if txt != "" {
					textRun = append(textRun, txt)
				}
			case "tool":
				flushText()
				flushReasoning()
				out = append(out, toolCallRow(pm, newRow(turnID(), ts), resp))
			}
		}
		flushText()
		flushReasoning()
	}
	return out
}

// toolCallRow maps an engine tool part ({callId, state:{title, input, output,
// startedAt, completedAt, error}}) to the phone's toolCall row.
func toolCallRow(part map[string]any, base map[string]any, assistantResponseID string) map[string]any {
	st, _ := part["state"].(map[string]any)
	toolName, _ := st["title"].(string)
	if toolName == "" {
		if tn, ok := part["toolName"].(string); ok {
			toolName = tn
		}
	}
	if toolName == "" {
		toolName = "Tool"
	}
	callID, _ := part["callId"].(string)
	if callID == "" {
		pid, _ := part["partId"].(string)
		callID = pid
	}
	if callID == "" {
		callID = fmt.Sprintf("call-%s-%d", assistantResponseID, base["rowId"])
	}
	input, _ := st["input"].(map[string]any)
	inputText := ""
	for _, k := range []string{"command", "query", "prompt", "file_path", "path"} {
		if s, ok := input[k].(string); ok && s != "" {
			inputText = s
			break
		}
	}
	if inputText == "" && input != nil {
		if b, err := json.Marshal(input); err == nil && len(b) <= 512 {
			inputText = string(b)
		}
	}
	outputText, _ := st["output"].(string)
	status := "success"
	if e, ok := st["error"].(string); ok && e != "" {
		status = "error"
	}
	base["kind"] = "toolCall"
	base["assistantResponseId"] = assistantResponseID
	base["toolCallId"] = callID
	base["toolName"] = toolName
	base["status"] = status
	base["inputText"] = inputText
	if input != nil {
		base["input"] = input
	}
	output := map[string]any{"text": outputText}
	base["output"] = output
	if s, ok := st["startedAt"].(float64); ok {
		base["startedAt"] = int64(s)
	}
	if c, ok := st["completedAt"].(float64); ok {
		base["endedAt"] = int64(c)
	}
	return base
}

// collectEngineText joins the text parts of an engine message.
func collectEngineText(parts []any) string {
	var sb strings.Builder
	for _, p := range parts {
		pm, _ := p.(map[string]any)
		if pm == nil || pm["type"] != "text" {
			continue
		}
		txt, _ := pm["text"].(string)
		if txt == "" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(txt)
	}
	return sb.String()
}

func shortSessionID(id string) string {
	if i := strings.Index(id, "-"); i > 0 {
		return id[:i]
	}
	return id
}

// sessionsIndexFrame builds a sessions-index snapshot wire frame.
// phaseForStatus maps a task's display status to the phone's session phase
// enum (draft|prewarming|running|completedSuccess|completedInterrupted|error)
// and whether the session has ended.
func phaseForStatus(display string) (string, bool) {
	switch display {
	case "running", "in-progress", "active":
		return "running", false
	case "error", "failed":
		return "error", true
	case "completed", "completedInterrupted":
		return "completedInterrupted", true
	case "idle", "cancelled", "paused":
		return "draft", true
	default: // completed / completedSuccess and unknown
		return "completedSuccess", true
	}
}
