// Live turn streaming: the engine never streams transcript bodies (see
// docs/web-remote-protocol.md) but two channels carry live progress:
//
//  1. v4/telemetry tool.lifecycle + stream.chunk counters — always on, but
//     content-free (tool names, chunk LENGTHS only).
//  2. session/event notifications after a one-time session/subscribe — REAL
//     content: model.streaming text_delta/reasoning_delta (actual thinking and
//     answer text), tool_input_start/delta/end (the actual command being
//     written), tool.updated result (actual tool output).
//
// Both map onto the same synthetic live rows (per turn: a tail boundary marker,
// reasoning rows, assistantText rows, toolCall cards) so the phone renders the
// turn live instead of a silent spinner until turn.terminal.

package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/friddle/zcode-quick-web-forward/internal/relay"
)

// liveTurn tracks the synthetic rows pushed for the in-flight engine turn so
// completion events can upsert the same rowId instead of duplicating cards.
type liveTurn struct {
	turnID        string
	tailPushed    bool
	boundaryRowID int
	toolRows      map[string]toolLive

	// real streamed content (session/event model.streaming deltas)
	textMsgID, reasonMsgID string // assistantMessageId owning the live row
	textBuf, reasonBuf     string
	textRowID, reasonRowID int
	textAt, reasonAt       int64
	textReal, reasonReal   bool // real content arrived — counter fallback backs off
	textChars, reasonChars int  // telemetry counter fallback
	toolInputs             map[string]*strings.Builder
	toolInputAt            map[string]int64
}

type toolLive struct {
	rowID     int
	toolName  string
	startedAt int64
	hasCard   bool
}

// liveFlushInterval throttles streaming-text upserts: deltas can fire many
// times a second, the phone only needs a periodic tick.
const liveFlushInterval = 700 * time.Millisecond

// liveTurnFor returns (creating if needed) the live-turn state for an engine
// session. A new turnId supersedes the previous turn's state. Caller holds
// ps.mu.
func (p *phoneSessions) liveTurnFor(engSid, turnID string) *liveTurn {
	if turnID == "" {
		turnID = "turn-live"
	}
	if p.live == nil {
		p.live = map[string]*liveTurn{}
	}
	if lt := p.live[engSid]; lt != nil {
		if lt.turnID == turnID {
			return lt
		}
	}
	lt := &liveTurn{
		turnID:        turnID,
		toolRows:      map[string]toolLive{},
		toolInputs:    map[string]*strings.Builder{},
		toolInputAt:   map[string]int64{},
		boundaryRowID: p.nextRowID(),
		textRowID:     p.nextRowID(),
		reasonRowID:   p.nextRowID(),
	}
	return lt
}

// endLiveTurn drops live-turn state (called on turn.terminal; the post-turn
// snapshot replaces the synthetic rows with the real transcript). Caller
// holds ps.mu.
func (p *phoneSessions) endLiveTurn(engSid string) {
	delete(p.live, engSid)
}

// liveToolRow renders a toolCall row in the exact shape the transcript mapper
// (toolCallRow) emits — the phone's zod union accepts it there, so it accepts
// it here. status must be one of the client's wire enum values:
// inputStreaming/pendingApproval/running/success/error/cancelled.
func liveToolRow(rowID int, turnID, toolCallID, toolName, status, inputText string, startedAt, endedAt int64, outputText string) map[string]any {
	row := map[string]any{
		"rowId":               rowID,
		"turnId":              turnID,
		"entityId":            toolCallID,
		"productTurnId":       turnID,
		"visibility":          "visible",
		"createdAt":           startedAt,
		"createdAtSeq":        rowID,
		"kind":                "toolCall",
		"assistantResponseId": turnID,
		"toolCallId":          toolCallID,
		"toolName":            toolName,
		"status":              status,
		"inputText":           inputText,
		"output":              map[string]any{"text": outputText},
		"startedAt":           startedAt,
	}
	if endedAt > 0 {
		row["endedAt"] = endedAt
	}
	return row
}

// liveCounterRow renders a streaming reasoning/assistantText row. With real
// content it carries the accumulated text; as a telemetry fallback it shows
// the ticking counter.
func liveCounterRow(rowID int, turnID, kind, text string) map[string]any {
	return map[string]any{
		"rowId":               rowID,
		"turnId":              turnID,
		"visibility":          "visible",
		"createdAt":           time.Now().UnixMilli(),
		"createdAtSeq":        rowID,
		"kind":                kind,
		"assistantResponseId": turnID,
		"text":                text,
		"state":               "streaming",
	}
}

// liveTailBoundary mints the invisible timelineMarker row that routes all
// following live rows into the client's streaming tail: the timeline only
// shows rows AFTER the last turnTailBoundary marker in the open tail section
// (rows before it fold into the collapsed history). checkpointRestored hits
// the marker renderer's default case, which renders nothing.
func liveTailBoundary(rowID int, turnID string) map[string]any {
	return map[string]any{
		"rowId":        rowID,
		"turnId":       turnID,
		"visibility":   "visible",
		"createdAt":    time.Now().UnixMilli(),
		"createdAtSeq": rowID,
		"kind":         "timelineMarker",
		"lane":         "turnTailBoundary",
		"marker":       map[string]any{"type": "checkpointRestored", "checkpointId": "live"},
	}
}

// liveTailOps prefixes the first live row of a turn with the boundary marker.
// Caller holds ps.mu.
func (lt *liveTurn) liveTailOps(row map[string]any) []any {
	if lt.tailPushed {
		return []any{map[string]any{"op": "row.appended", "row": row}}
	}
	lt.tailPushed = true
	return []any{
		map[string]any{"op": "row.appended", "row": liveTailBoundary(lt.boundaryRowID, row["turnId"].(string))},
		map[string]any{"op": "row.appended", "row": row},
	}
}

// pushLiveDeltas marshals, sends and remembers one conversation delta frame.
// Appended rows extend the remembered set; upserted rows patch it in place, so
// recovery snapshots replay the live turn to mid-turn subscribers.
func pushLiveDeltas(engine *relay.BridgeEngine, send func(v any), ps *phoneSessions, phoneSid string, deltas []any) {
	if len(deltas) == 0 {
		return
	}
	ps.mu.Lock()
	convID := ps.convListener
	convSub := ps.convSubscription
	ps.mu.Unlock()
	appended := []any{}
	for _, d := range deltas {
		dm, _ := d.(map[string]any)
		if dm == nil {
			continue
		}
		if op, _ := dm["op"].(string); op == "row.appended" {
			if row, ok := dm["row"].(map[string]any); ok {
				appended = append(appended, row)
			}
		}
	}
	if convID > 0 {
		if b, err := json.Marshal(conversationDeltaFrame(ps, phoneSid, convSub, ps.nextOrdinal(), deltas)); err == nil {
			engine.SendChannelEvent(convID, b, send)
		}
	}
	if len(appended) > 0 {
		ps.appendRemembered(appended)
	}
	for _, d := range deltas {
		if dm, ok := d.(map[string]any); ok {
			if op, _ := dm["op"].(string); op == "row.upserted" {
				if row, ok := dm["row"].(map[string]any); ok {
					ps.rememberUpsert(row)
				}
			}
		}
	}
}

// liveRowID is the shared row source for live rows (must not lock ps.mu —
// callers already hold it; rowIDSeqMu is a dedicated mutex).
func liveRowID(ps *phoneSessions) int { return ps.nextRowID() }

// telemetry fallback: map one tool.lifecycle event to live cards.
func handleLiveToolEvent(engine *relay.BridgeEngine, send func(v any), ps *phoneSessions, engSid, phoneSid string, params map[string]any) {
	phase, _ := params["phase"].(string)
	toolCallID, _ := params["toolCallId"].(string)
	if toolCallID == "" {
		return
	}
	toolName, _ := params["toolName"].(string)
	if toolName == "" {
		toolName = "Tool"
	}
	turnID, _ := params["turnId"].(string)
	now := time.Now().UnixMilli()

	ps.mu.Lock()
	lt := ps.liveTurnFor(engSid, turnID)
	rowTurn := ps.currentTurnIDLocked(phoneSid)
	var deltas []any
	switch phase {
	case "scheduled", "started":
		if meta, seen := lt.toolRows[toolCallID]; seen {
			// session/event may have created the card first (input streaming) —
			// fill in the real tool name if we only had a placeholder.
			if meta.toolName != toolName {
				meta.toolName = toolName
				lt.toolRows[toolCallID] = meta
				in := ""
				if sb := lt.toolInputs[toolCallID]; sb != nil {
					in = sb.String()
				}
				deltas = []any{map[string]any{"op": "row.upserted", "row": liveToolRow(meta.rowID, rowTurn, toolCallID, toolName, "running", in, meta.startedAt, 0, "")}}
			}
			ps.mu.Unlock()
			pushLiveDeltas(engine, send, ps, phoneSid, deltas)
			return
		}
		rowID := liveRowID(ps)
		lt.toolRows[toolCallID] = toolLive{rowID: rowID, toolName: toolName, startedAt: now, hasCard: true}
		deltas = lt.liveTailOps(liveToolRow(rowID, rowTurn, toolCallID, toolName, "running", "", now, 0, ""))
	case "completed", "failed":
		meta, ok := lt.toolRows[toolCallID]
		if !ok {
			ps.mu.Unlock()
			return // completed for a tool we never saw start (pre-attach)
		}
		status := "success"
		if phase == "failed" {
			status = "error"
		}
		if ec, _ := params["errorCode"].(string); ec != "" {
			status = "error"
		}
		ended := now
		if d, _ := params["durationMs"].(float64); d > 0 {
			ended = meta.startedAt + int64(d)
		}
		in := ""
		if sb := lt.toolInputs[toolCallID]; sb != nil {
			in = sb.String()
		}
		delete(lt.toolRows, toolCallID)
		deltas = []any{map[string]any{"op": "row.upserted", "row": liveToolRow(meta.rowID, rowTurn, toolCallID, meta.toolName, status, in, meta.startedAt, ended, "")}}
	default:
		ps.mu.Unlock()
		return // progress carries no new visible state
	}
	ps.mu.Unlock()

	pushLiveDeltas(engine, send, ps, phoneSid, deltas)
	fmt.Printf("zcode: live tool %s %s (%s)\n", toolName, phase, toolCallID)
}

// handleLiveChunkEvent is the telemetry FALLBACK for streaming indicators —
// it only knows chunk LENGTHS. Once real session/event content has arrived
// for a row the counter backs off (never overwrites real text).
func handleLiveChunkEvent(engine *relay.BridgeEngine, send func(v any), ps *phoneSessions, engSid, phoneSid string, params map[string]any) {
	channel, _ := params["channel"].(string)
	if channel != "thought" && channel != "text" {
		return
	}
	n, _ := params["chunkLength"].(float64)
	turnID, _ := params["turnId"].(string)
	now := time.Now().UnixMilli()

	ps.mu.Lock()
	lt := ps.liveTurnFor(engSid, turnID)
	rowTurn := ps.currentTurnIDLocked(phoneSid)
	var rowID int
	var kind, text string
	var lastAt *int64
	if channel == "thought" {
		if lt.reasonReal {
			ps.mu.Unlock()
			return
		}
		lt.reasonChars += int(n)
		rowID, kind = lt.reasonRowID, "reasoning"
		text = fmt.Sprintf("思考中… %d 字", lt.reasonChars)
		lastAt = &lt.reasonAt
	} else {
		if lt.textReal {
			ps.mu.Unlock()
			return
		}
		lt.textChars += int(n)
		rowID, kind = lt.textRowID, "assistantText"
		text = fmt.Sprintf("生成中… %d 字", lt.textChars)
		lastAt = &lt.textAt
	}
	if now-*lastAt < liveFlushInterval.Milliseconds() {
		ps.mu.Unlock()
		return
	}
	*lastAt = now
	deltas := lt.liveTailOps(liveCounterRow(rowID, rowTurn, kind, text))
	ps.mu.Unlock()

	pushLiveDeltas(engine, send, ps, phoneSid, deltas)
}

// handleSessionEvent maps the engine's rich session/event notifications
// (after session/subscribe) onto the live rows with REAL content.
func handleSessionEvent(engine *relay.BridgeEngine, send func(v any), ps *phoneSessions, engSid, phoneSid string, params map[string]any) {
	evType, _ := params["type"].(string)
	payload, _ := params["payload"].(map[string]any)
	if payload == nil {
		return
	}
	turnID, _ := params["turnId"].(string)
	now := time.Now().UnixMilli()

	switch evType {
	case "model.streaming":
		kind, _ := payload["kind"].(string)
		delta, _ := payload["delta"].(string)
		msgID, _ := payload["assistantMessageId"].(string)
		toolCallID, _ := payload["toolCallId"].(string)
		switch kind {
		case "text_delta", "reasoning_delta":
			if delta == "" {
				return
			}
			ps.mu.Lock()
			lt := ps.liveTurnFor(engSid, turnID)
			rowTurn := ps.currentTurnIDLocked(phoneSid)
			var deltas []any
			if kind == "text_delta" {
				if lt.textMsgID != msgID {
					// new assistant message: seal the previous text row
					if lt.textRowID != 0 && lt.textBuf != "" {
						deltas = append(deltas, map[string]any{"op": "row.upserted", "row": liveCounterRow(lt.textRowID, rowTurn, "assistantText", lt.textBuf)})
					}
					lt.textMsgID, lt.textBuf, lt.textRowID = msgID, "", liveRowID(ps)
					lt.textReal = true
				}
				lt.textReal = true
				lt.textBuf += delta
				if now-lt.textAt >= liveFlushInterval.Milliseconds() {
					lt.textAt = now
					deltas = append(deltas, lt.liveTailOps(liveCounterRow(lt.textRowID, rowTurn, "assistantText", lt.textBuf))...)
				}
			} else {
				if lt.reasonMsgID != msgID {
					if lt.reasonRowID != 0 && lt.reasonBuf != "" {
						deltas = append(deltas, map[string]any{"op": "row.upserted", "row": liveCounterRow(lt.reasonRowID, rowTurn, "reasoning", lt.reasonBuf)})
					}
					lt.reasonMsgID, lt.reasonBuf, lt.reasonRowID = msgID, "", liveRowID(ps)
					lt.reasonReal = true
				}
				lt.reasonReal = true
				lt.reasonBuf += delta
				if now-lt.reasonAt >= liveFlushInterval.Milliseconds() {
					lt.reasonAt = now
					deltas = append(deltas, lt.liveTailOps(liveCounterRow(lt.reasonRowID, rowTurn, "reasoning", lt.reasonBuf))...)
				}
			}
			ps.mu.Unlock()
			pushLiveDeltas(engine, send, ps, phoneSid, deltas)
		case "tool_input_start", "tool_input_delta", "tool_input_end", "tool_call":
			if toolCallID == "" {
				return
			}
			ps.mu.Lock()
			lt := ps.liveTurnFor(engSid, turnID)
			rowTurn := ps.currentTurnIDLocked(phoneSid)
			sb := lt.toolInputs[toolCallID]
			if sb == nil {
				sb = &strings.Builder{}
				lt.toolInputs[toolCallID] = sb
			}
			if kind == "tool_input_start" {
				sb.Reset()
			}
			sb.WriteString(delta)
			in := sb.String()
			if kind == "tool_call" {
				// complete input object — prefer the raw string form
				if raw, ok := payload["input"].(string); ok && raw != "" {
					in = raw
				}
			}
			var deltas []any
			if meta, ok := lt.toolRows[toolCallID]; ok {
				if kind == "tool_input_end" || kind == "tool_call" {
					in = prettyToolInput(in)
				}
				deltas = []any{map[string]any{"op": "row.upserted", "row": liveToolRow(meta.rowID, rowTurn, toolCallID, meta.toolName, "running", in, meta.startedAt, 0, "")}}
			} else {
				// input streaming before any lifecycle card: create a card now
				rowID := liveRowID(ps)
				name := "Tool"
				if kind == "tool_call" {
					name = prettyToolName(payload)
				}
				lt.toolRows[toolCallID] = toolLive{rowID: rowID, toolName: name, startedAt: now, hasCard: true}
				deltas = lt.liveTailOps(liveToolRow(rowID, rowTurn, toolCallID, name, "running", in, now, 0, ""))
			}
			ps.mu.Unlock()
			pushLiveDeltas(engine, send, ps, phoneSid, deltas)
		}
	case "tool.updated":
		pkind, _ := payload["kind"].(string)
		toolCallID, _ := payload["toolCallId"].(string)
		if toolCallID == "" {
			return
		}
		toolName, _ := payload["toolName"].(string)
		switch pkind {
		case "scheduled", "started":
			if toolName == "" {
				toolName = "Tool"
			}
			ps.mu.Lock()
			lt := ps.liveTurnFor(engSid, turnID)
			rowTurn := ps.currentTurnIDLocked(phoneSid)
			var deltas []any
			if meta, ok := lt.toolRows[toolCallID]; ok {
				if meta.toolName != toolName && meta.toolName == "Tool" {
					meta.toolName = toolName
					lt.toolRows[toolCallID] = meta
					in := ""
					if sb := lt.toolInputs[toolCallID]; sb != nil {
						in = sb.String()
					}
					deltas = []any{map[string]any{"op": "row.upserted", "row": liveToolRow(meta.rowID, rowTurn, toolCallID, toolName, "running", in, meta.startedAt, 0, "")}}
				}
			} else {
				rowID := liveRowID(ps)
				lt.toolRows[toolCallID] = toolLive{rowID: rowID, toolName: toolName, startedAt: now, hasCard: true}
				deltas = lt.liveTailOps(liveToolRow(rowID, rowTurn, toolCallID, toolName, "running", "", now, 0, ""))
			}
			ps.mu.Unlock()
			pushLiveDeltas(engine, send, ps, phoneSid, deltas)
		case "result", "error":
			ps.mu.Lock()
			lt := ps.liveTurnFor(engSid, turnID)
			rowTurn := ps.currentTurnIDLocked(phoneSid)
			meta, ok := lt.toolRows[toolCallID]
			if !ok {
				ps.mu.Unlock()
				return
			}
			status := "success"
			if pkind == "error" {
				status = "error"
			}
			in := ""
			if sb := lt.toolInputs[toolCallID]; sb != nil {
				in = sb.String()
			}
			out := toolOutputText(payload["output"])
			delete(lt.toolRows, toolCallID)
			deltas := []any{map[string]any{"op": "row.upserted", "row": liveToolRow(meta.rowID, rowTurn, toolCallID, meta.toolName, status, in, meta.startedAt, now, out)}}
			ps.mu.Unlock()
			pushLiveDeltas(engine, send, ps, phoneSid, deltas)
		}
	}
}

// prettyToolInput turns a raw streamed tool-input JSON string into the display
// line the desktop shows (command / file_path / prompt …). Falls back to the
// raw string, trimmed.
func prettyToolInput(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if !strings.HasPrefix(trimmed, "{") {
		return trimmed
	}
	var m map[string]any
	if json.Unmarshal([]byte(trimmed), &m) != nil {
		return trimmed
	}
	for _, k := range []string{"command", "file_path", "path", "query", "prompt", "pattern", "url"} {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return trimmed
}

// prettyToolName extracts a display name from a complete tool_call payload.
func prettyToolName(payload map[string]any) string {
	if n, ok := payload["toolName"].(string); ok && n != "" {
		return n
	}
	if n, ok := payload["name"].(string); ok && n != "" {
		return n
	}
	return "Tool"
}

// toolOutputText normalizes a tool result output field into display text.
func toolOutputText(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case map[string]any:
		if s, ok := t["text"].(string); ok {
			return s
		}
		if s, ok := t["stdout"].(string); ok {
			return s
		}
		b, err := json.Marshal(t)
		if err == nil && len(b) <= 4000 {
			return string(b)
		}
	default:
		b, err := json.Marshal(v)
		if err == nil && len(b) <= 4000 {
			return string(b)
		}
	}
	return ""
}
