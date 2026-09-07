// Live turn streaming: the engine never streams transcript bodies (see
// docs/web-remote-protocol.md) but its v4/telemetry notifications DO carry
// tool lifecycle (scheduled/started/completed per toolCallId) and
// stream.chunk counters on the "thought" and "text" channels. Forwarding
// those as conversation row deltas gives the phone live tool cards and a
// ticking 思考中/生成中 bubble while the turn runs, instead of a silent
// spinner until turn.terminal.

package main

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/friddle/zcode-quick-web-forward/internal/relay"
)

// liveTurn tracks the synthetic rows pushed for the in-flight engine turn so
// completion events can upsert the same rowId instead of duplicating cards.
type liveTurn struct {
	turnID      string
	toolRows    map[string]toolLive
	reasonRowID int
	reasonChars int
	reasonAt    int64
	textRowID   int
	textChars   int
	textAt      int64
}

type toolLive struct {
	rowID     int
	toolName  string
	startedAt int64
}

// liveFlushInterval throttles counter-row upserts: stream.chunk fires several
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
	lt := &liveTurn{turnID: turnID, toolRows: map[string]toolLive{}}
	p.live[engSid] = lt
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
func liveToolRow(rowID int, turnID, toolCallID, toolName, status string, startedAt, endedAt int64) map[string]any {
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
		"inputText":           "",
		"output":              map[string]any{"text": ""},
		"startedAt":           startedAt,
	}
	if endedAt > 0 {
		row["endedAt"] = endedAt
	}
	return row
}

// liveCounterRow renders the ticking reasoning/assistantText placeholder.
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

// pushLiveDeltas marshals and sends one conversation delta frame.
func pushLiveDeltas(engine *relay.BridgeEngine, send func(v any), ps *phoneSessions, phoneSid string, deltas []any) {
	if len(deltas) == 0 {
		return
	}
	ps.mu.Lock()
	convID := ps.convListener
	convSub := ps.convSubscription
	ps.mu.Unlock()
	if convID <= 0 {
		return
	}
	b, err := json.Marshal(conversationDeltaFrame(phoneSid, convSub, ps.nextOrdinal(), deltas))
	if err != nil {
		return
	}
	engine.SendChannelEvent(convID, b, send)
}

// handleLiveToolEvent maps one tool.lifecycle telemetry event to live rows.
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
	var deltas []any
	switch phase {
	case "scheduled", "started":
		if _, seen := lt.toolRows[toolCallID]; seen {
			ps.mu.Unlock()
			return // started re-broadcast after scheduled — one card only
		}
		rowID := ps.nextRowID()
		lt.toolRows[toolCallID] = toolLive{rowID: rowID, toolName: toolName, startedAt: now}
		deltas = []any{map[string]any{"op": "row.appended", "row": liveToolRow(rowID, lt.turnID, toolCallID, toolName, "running", now, 0)}}
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
		delete(lt.toolRows, toolCallID)
		deltas = []any{map[string]any{"op": "row.upserted", "row": liveToolRow(meta.rowID, lt.turnID, toolCallID, meta.toolName, status, meta.startedAt, ended)}}
	default:
		ps.mu.Unlock()
		return // progress carries no new visible state
	}
	ps.mu.Unlock()

	pushLiveDeltas(engine, send, ps, phoneSid, deltas)
	fmt.Printf("zcode: live tool %s %s (%s) -> %s\n", toolName, phase, toolCallID, statusOf(deltas))
}

func statusOf(deltas []any) string {
	d0, _ := deltas[0].(map[string]any)
	if row, ok := d0["row"].(map[string]any); ok {
		if s, ok := row["status"].(string); ok {
			return s
		}
	}
	return "?"
}

// handleLiveChunkEvent maps stream.chunk thought/text counters to ticking
// reasoning/assistantText placeholder rows.
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
	var rowID int
	var kind, label string
	var chars *int
	var lastAt *int64
	if channel == "thought" {
		if lt.reasonRowID == 0 {
			lt.reasonRowID = ps.nextRowID()
			lt.reasonAt = now
			rid := lt.reasonRowID
			turn := lt.turnID
			ps.mu.Unlock()
			pushLiveDeltas(engine, send, ps, phoneSid, []any{map[string]any{
				"op": "row.appended", "row": liveCounterRow(rid, turn, "reasoning", "思考中…"),
			}})
			return
		}
		lt.reasonChars += int(n)
		rowID, kind, label, chars, lastAt = lt.reasonRowID, "reasoning", "思考中… %d 字", &lt.reasonChars, &lt.reasonAt
	} else {
		if lt.textRowID == 0 {
			lt.textRowID = ps.nextRowID()
			lt.textAt = now
			rid := lt.textRowID
			turn := lt.turnID
			ps.mu.Unlock()
			pushLiveDeltas(engine, send, ps, phoneSid, []any{map[string]any{
				"op": "row.appended", "row": liveCounterRow(rid, turn, "assistantText", "生成中…"),
			}})
			return
		}
		lt.textChars += int(n)
		rowID, kind, label, chars, lastAt = lt.textRowID, "assistantText", "生成中… %d 字", &lt.textChars, &lt.textAt
	}
	if now-*lastAt < liveFlushInterval.Milliseconds() {
		ps.mu.Unlock()
		return
	}
	*lastAt = now
	turn := lt.turnID
	text := fmt.Sprintf(label, *chars)
	ps.mu.Unlock()

	pushLiveDeltas(engine, send, ps, phoneSid, []any{map[string]any{
		"op": "row.upserted", "row": liveCounterRow(rowID, turn, kind, text),
	}})
}
