// messages→rows back-fill for unregistered workspaces. The host's conversation
// cache only accumulates streamed assistant rows for the workspace seeded at
// init-local; sessions created under other pinned workspaces (opened via the
// phone's workspace cards) run fine engine-side but conversationRowsRangeV4
// keeps returning just turnHeader+userInput — the phone shows a frozen
// "working" state with no reply. The stashed session/read snapshot carries the
// full transcript; merge the missing assistant rows into the rowsRange result.

package main

import (
	"encoding/json"
	"fmt"
)

type msgPart struct {
	Type   string `json:"type"`
	Text   string `json:"text"`
	CallID string `json:"callId"`
	Tool   string `json:"tool"`
	State  struct {
		Status string `json:"status"`
	} `json:"state"`
}

type msgInfo struct {
	Role      string `json:"role"`
	MessageID string `json:"messageId"`
	Finish    string `json:"finish"`
	Time      struct {
		Created   int64 `json:"created"`
		Completed int64 `json:"completed"`
	} `json:"time"`
}

// mergeMessageRows back-fills assistantText/reasoning rows the host cache is
// missing, using the stashed session/read transcript. Returns true when the
// rows were changed (the caller re-marshals so dedupe stays consistent).
func (r *officialRecovery) mergeMessageRows(sid string, rowsRes map[string]any) bool {
	if rowsRes == nil {
		return false
	}
	raw := r.snapFor(sid)
	if len(raw) == 0 {
		return false
	}
	var snap struct {
		Messages []struct {
			Info  msgInfo   `json:"info"`
			Parts []msgPart `json:"parts"`
		} `json:"messages"`
	}
	if json.Unmarshal(raw, &snap) != nil || len(snap.Messages) == 0 {
		return false
	}
	rows := fullRowsOf(rowsRes)
	seen := map[string]bool{}
	maxRow, maxSeq := 0, 0
	for _, row := range rows {
		m, ok := row.(map[string]any)
		if !ok {
			continue
		}
		if id, _ := m["entityId"].(string); id != "" {
			seen[id] = true
		}
		if v, ok := m["rowId"].(float64); ok && int(v) > maxRow {
			maxRow = int(v)
		}
		if v, ok := m["createdAtSeq"].(float64); ok && int(v) > maxSeq {
			maxSeq = int(v)
		}
	}
	var added []any
	turnID := ""
	changed := false
	for i := range snap.Messages {
		msg := &snap.Messages[i]
		if msg.Info.Role == "user" && msg.Info.MessageID != "" {
			// Official turns are keyed by the user message id (observed: the
			// host-written turnHeader/userInput rows share it).
			turnID = msg.Info.MessageID
		}
		if msg.Info.Role != "assistant" || msg.Info.MessageID == "" || seen[msg.Info.MessageID] {
			continue
		}
		if turnID == "" {
			turnID = msg.Info.MessageID
		}
		state := "complete"
		if msg.Info.Finish == "" {
			state = "streaming"
		}
		emitted := false
		for _, p := range msg.Parts {
			var kind string
			switch p.Type {
			case "text":
				kind = "assistantText"
			case "reasoning":
				kind = "reasoning"
			default:
				continue // step-start/step-finish/tool/timeline: not row-mapped yet
			}
			if p.Text == "" {
				continue
			}
			maxRow++
			maxSeq++
			added = append(added, map[string]any{
				"rowId":               maxRow,
				"turnId":              turnID,
				"entityId":            msg.Info.MessageID,
				"visibility":          "visible",
				"createdAtSeq":        maxSeq,
				"kind":                kind,
				"assistantResponseId": msg.Info.MessageID,
				"text":                p.Text,
				"state":               state,
			})
			emitted = true
		}
		if emitted {
			seen[msg.Info.MessageID] = true
			changed = true
		}
	}
	if !changed {
		return false
	}
	rowsRes["rows"] = append(rows, added...)
	fmt.Printf("zcode: recovery: back-filled %d assistant rows from session/read (%s)\n", len(added), sid)
	return true
}
