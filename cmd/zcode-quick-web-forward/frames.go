// Wire frames pushed to the phone: sessions-index and controller
// tasks-index snapshots, conversation snapshot / delta / chunk frames and
// the state.updated control patches.

package main

import (
	"strings"
	"time"

	"github.com/friddle/zcode-quick-web-forward/internal/zcode"
)

// sessionsIndexFrame builds a sessions-index snapshot wire frame. The phone
// binds one index stream per workspace: the topic/workspaceId carry the real
// workspace path and the subscriptionId is the index's own (never shared with
// the conversation stream, or the client drops the frames and @ 会话 stays
// empty).
func sessionsIndexFrame(ps *phoneSessions) map[string]any {
	idx := "0"
	ws := ps.indexWorkspace()
	subID := ps.indexSub()
	ordinal := ps.nextOrdinal()
	all, _ := zcode.ListTasks("", "")
	inWorkspace := make([]zcode.Task, 0, len(all))
	for _, t := range all {
		if t.WorkspacePath == ws {
			inWorkspace = append(inWorkspace, t)
		}
	}
	if len(inWorkspace) == 0 {
		inWorkspace = all // workspace unknown — expose everything rather than nothing
	}
	tasks := inWorkspace
	// Cap the index: 100+ sessions pushed as one snapshot produce a >48KiB
	// fragmented rpc-frame, and the frontend chokes on reassembly — the
	// connection drops and re-pairs in a tight loop. The landing shows the
	// most recent tasks anyway (ListTasks is newest-first).
	const maxIndexSessions = 30
	if len(tasks) > maxIndexSessions {
		tasks = tasks[:maxIndexSessions]
	}
	sessions := make([]any, 0, len(tasks))
	seen := map[string]bool{}
	for _, t := range tasks {
		if strings.HasPrefix(t.WorkspaceKey, "remote:") {
			continue
		}
		seen[t.TaskID] = true
		phase, ended := phaseForStatus(displayStatus(t.Status))
		sessions = append(sessions, map[string]any{
			"sessionId":            t.TaskID,
			"workspaceId":          ws,
			"title":                t.Title,
			"titleSource":          "generated",
			"phase":                phase,
			"sessionEnded":         ended,
			"hasBackgroundWork":    false,
			"createdAt":            t.CreatedAt,
			"lastActivityAt":       t.UpdatedAt,
			"lastAssistantPreview": "",
		})
	}
	if ps != nil {
		for _, rt := range ps.runtimeTaskList() {
			m, _ := rt.(map[string]any)
			sid, _ := m["taskId"].(string)
			if sid == "" || seen[sid] || strings.HasPrefix(sid, "draft-") {
				// synthetic drafts ("draft-*") never live in the engine and
				// their id shape breaks the client's session schema
				continue
			}
			seen[sid] = true
			title, _ := m["title"].(string)
			rtws, _ := m["workspacePath"].(string)
			sessions = append(sessions, map[string]any{
				"sessionId":            sid,
				"workspaceId":          rtws,
				"title":                title,
				"titleSource":          "generated",
				"phase":                "running",
				"sessionEnded":         false,
				"hasBackgroundWork":    false,
				"createdAt":            m["createdAt"],
				"lastActivityAt":       m["updatedAt"],
				"lastAssistantPreview": "",
			})
		}
	}
	return map[string]any{
		"wireVersion":         3,
		"kind":                "complete",
		"deliveryKind":        "initial",
		"logicalFrameId":      uuidNew(),
		"logicalFrameOrdinal": ordinal,
		"topic":               "sessions-index/" + ws,
		"subscriptionId":      subID,
		"frame": map[string]any{
			"topic":          "sessions-index/" + ws,
			"subscriptionId": subID,
			"fromSeq":        1,
			"toSeq":          1,
			"sentAt":         time.Now().UnixMilli(),
			"payload": map[string]any{
				"kind": "snapshot",
				"snapshot": map[string]any{
					"protocolVersion": 1,
					"workspaceId":     ws,
					"logEpoch":        idx,
					"sessions":        sessions,
				},
			},
		},
	}
}

// taskIndexItem renders one task in the official controller/tasks-index
// record shape: address + meta + membership + sourceAvailability + liveStatus
// + activity (the sidebar's live status reads from this, not from listTasks).
func taskIndexItem(t zcode.Task, extraLive map[string]bool) map[string]any {
	phase, _ := phaseForStatus(displayStatus(t.Status))
	live := phase
	if extraLive[t.TaskID] {
		live = "running"
		phase = "running"
	}
	// meta.model is "<providerId>/<modelId>"; fall back to the configured
	// default model when the task index has none stored.
	model := t.Model
	if model == "" {
		if p, m := zcode.DefaultModel(); p != "" {
			model = p + "/" + m
		}
	}
	provider := "glm"
	if i := strings.Index(model, "/"); i > 0 {
		provider = strings.ToLower(model[:i])
	}
	return map[string]any{
		"address": map[string]any{
			"workspacePath": t.WorkspacePath,
			"taskId":        t.TaskID,
		},
		"meta": map[string]any{
			"taskId":          t.TaskID,
			"traceId":         t.TaskID,
			"title":           t.Title,
			"titleOverridden": false,
			"workspacePath":   t.WorkspacePath,
			"createdAt":       t.CreatedAt,
			"updatedAt":       t.UpdatedAt,
			"mode":            "build",
			"model":           model,
			"thoughtLevel":    "low",
			"provider":        provider,
			"status":          displayStatus(t.Status),
			"target":          nil,
		},
		"membership":         map[string]any{"pinned": t.Pinned, "archived": t.Archived, "active": !t.Archived},
		"sourceAvailability": "online",
		"liveStatus":         live,
		"activity": map[string]any{
			"phase":             phase,
			"lastActivityAt":    t.UpdatedAt,
			"hasBackgroundWork": false,
		},
	}
}

// tasksIndexFrame builds the controller/tasks-index snapshot the official
// client subscribes to (window-controller.subscribeControllerV4 →
// onDynamicControllerFrame). Note: this topic uses a FLAT envelope (no
// wireVersion wrapper — unlike conversation/sessions-index frames).
func tasksIndexFrame(ps *phoneSessions) map[string]any {
	tasks, _ := zcode.ListTasks("", "")
	// Cap at the most recent 30: with ~100 tasks the frame crossed 48KiB and
	// the fragmented rpc-frame made the frontend choke (reconnect loop).
	if len(tasks) > 30 {
		tasks = tasks[:30]
	}
	live := map[string]bool{}
	if ps != nil {
		live = ps.liveTaskIDs()
	}
	items := make([]any, 0, len(tasks)+1)
	for _, t := range tasks {
		if strings.HasPrefix(t.WorkspaceKey, "remote:") {
			continue
		}
		items = append(items, taskIndexItem(t, live))
	}
	return map[string]any{
		"topic":          "controller/tasks-index",
		"subscriptionId": uuidNew(),
		"logEpoch":       "0",
		"fromSeq":        0,
		"toSeq":          1,
		"sentAt":         time.Now().UnixMilli(),
		"payload": map[string]any{
			"kind": "snapshot",
			"snapshot": map[string]any{
				"protocolVersion": 1,
				"logEpoch":        "0",
				"tasks":           items,
			},
		},
	}
}

// stateUpdatedFrame wraps a projection state change as a conversation frame
// delta. Shape mirrors the official desktop's state.updated ops: a flat patch
// of top-level snapshot keys (control / availability / inputRouting).
func stateUpdatedFrame(ps *phoneSessions, sessionID, phase, convSub string, ordinal int) map[string]any {
	if convSub == "" {
		convSub = sessionID + ":sub"
	}
	fromSeq, toSeq := ps.convoFrameSeq(false)
	ended := phase != "running" && phase != "prewarming" && phase != "draft"
	control := map[string]any{
		"phase": phase, "sessionEnded": ended,
		"canStop": false, "stopState": "idle", "stopTargetKind": "unknown",
		"activeWorks": []any{}, "lastError": nil, "apiRetry": nil,
	}
	availability := map[string]any{
		"fork": map[string]any{"allowed": true}, "compact": map[string]any{"allowed": true},
		"switchModelConfig": map[string]any{"allowed": true}, "setFollowupMode": map[string]any{"allowed": true},
		"queueEdit":     map[string]any{"allowed": true},
		"sendQueuedNow": map[string]any{"allowed": false, "reasonCode": "sendQueuedNowRequiresRunning"},
		"pauseGoal":     map[string]any{"allowed": false, "reasonCode": "noGoalToPause"},
		"resumeGoal":    map[string]any{"allowed": false, "reasonCode": "noGoalToResume"},
	}
	routing := map[string]any{"mode": "startNow"}
	if !ended {
		// A turn is in flight: the desktop flips the projection to running —
		// stoppable, a primaryTurn activeWork, follow-ups route to the queue.
		control["phase"] = phase
		control["canStop"] = true
		control["stopState"] = "stoppable"
		control["stopTargetKind"] = "assistant"
		control["activeWorks"] = []any{map[string]any{
			"kind": "primaryTurn", "foregroundExecutionId": "runtime_command_local", "startedAt": time.Now().UnixMilli(),
		}}
		availability["sendQueuedNow"] = map[string]any{"allowed": true}
		routing = map[string]any{"mode": "enqueue"}
	}
	return map[string]any{
		"wireVersion":         3,
		"kind":                "complete",
		"deliveryKind":        "online",
		"logicalFrameId":      uuidNew(),
		"logicalFrameOrdinal": ordinal,
		"topic":               "conversation/" + sessionID,
		"subscriptionId":      convSub,
		"frame": map[string]any{
			"topic":          "conversation/" + sessionID,
			"subscriptionId": convSub,
			"fromSeq":        fromSeq,
			"toSeq":          toSeq,
			"sentAt":         time.Now().UnixMilli(),
			"payload": map[string]any{
				"kind": "deltas",
				"deltas": []any{
					map[string]any{
						"op": "state.updated",
						"patch": map[string]any{
							"control":      control,
							"availability": availability,
							"inputRouting": routing,
						},
					},
				},
			},
		},
	}
}

// conversationDeltaFrame wraps arbitrary projection deltas (row.appended /
// state.updated / row.delta ops) as one conversation frame. The inner frame's
// fromSeq/toSeq MUST continue the client's transcript seq (see
// phoneSessions.convoFrameSeq) — constant values made the client treat every
// frame after the first as stale (toSeq <= local seq) or as a 帧断档 gap.
func conversationDeltaFrame(ps *phoneSessions, sessionID, convSub string, ordinal int, deltas []any) map[string]any {
	if convSub == "" {
		convSub = sessionID + ":sub"
	}
	fromSeq, toSeq := ps.convoFrameSeq(false)
	return map[string]any{
		"wireVersion":         3,
		"kind":                "complete",
		"deliveryKind":        "online",
		"logicalFrameId":      uuidNew(),
		"logicalFrameOrdinal": ordinal,
		"topic":               "conversation/" + sessionID,
		"subscriptionId":      convSub,
		"frame": map[string]any{
			"topic":          "conversation/" + sessionID,
			"subscriptionId": convSub,
			"fromSeq":        fromSeq,
			"toSeq":          toSeq,
			"sentAt":         time.Now().UnixMilli(),
			"payload": map[string]any{
				"kind":   "deltas",
				"deltas": deltas,
			},
		},
	}
}

// conversationChunkFrame wraps one assistant text chunk as a conversation
// delta (row.append-style) so the phone renders streaming output.
func conversationChunkFrame(ps *phoneSessions, sessionID, text, convSub string, ordinal int) map[string]any {
	if convSub == "" {
		convSub = sessionID + ":sub"
	}
	now := time.Now().UnixMilli()
	fromSeq, toSeq := ps.convoFrameSeq(false)
	return map[string]any{
		"wireVersion":         3,
		"kind":                "complete",
		"deliveryKind":        "online",
		"logicalFrameId":      uuidNew(),
		"logicalFrameOrdinal": ordinal,
		"topic":               "conversation/" + sessionID,
		"subscriptionId":      convSub,
		"frame": map[string]any{
			"topic":          "conversation/" + sessionID,
			"subscriptionId": convSub,
			"fromSeq":        fromSeq,
			"toSeq":          toSeq,
			"sentAt":         now,
			"payload": map[string]any{
				"kind": "deltas",
				"deltas": []any{
					map[string]any{
						"op": "row.appended",
						"row": map[string]any{
							"rowId":               1,
							"turnId":              "turn-" + sessionID,
							"createdAt":           now,
							"createdAtSeq":        1,
							"kind":                "assistantText",
							"assistantResponseId": "ar-" + sessionID,
							"text":                text,
							"state":               "streaming",
						},
					},
				},
			},
		},
	}
}

// conversationSnapshotFrame builds a conversation snapshot wire frame matching
// the phone's strict Hoe schema. deliveryKind is "initial" (fresh subscribe) or
// "recovery" (resync). ordinal must strictly increase per subscription.
// phaseForSession resolves the projection phase for a session: the live
// session counts as running only while a turn is in flight, anything else
// falls back to its persisted task status. (No manual ps.mu section here —
// turnRunningFor/engineFor lock internally; nesting them self-deadlocks.)
func phaseForSession(ps *phoneSessions, sessionID string) string {
	if ps != nil {
		if ps.turnRunningFor(ps.engineFor(sessionID)) {
			return "running"
		}
	}
	if t, ok, err := zcode.GetTask(sessionID); err == nil && ok {
		phase, _ := phaseForStatus(displayStatus(t.Status))
		return phase
	}
	return "completedSuccess"
}

// sessionTitle resolves the display title for a conversation view header:
// the persisted task title first, then the runtime draft entry. The client
// falls back to "New session" when the snapshot meta carries no title.
func sessionTitle(ps *phoneSessions, sessionID string) string {
	if t, ok, err := zcode.GetTask(sessionID); err == nil && ok && t.Title != "" {
		return t.Title
	}
	if ps != nil {
		for _, rt := range ps.runtimeTaskList() {
			if m, _ := rt.(map[string]any); m != nil && m["taskId"] == sessionID {
				if s, _ := m["title"].(string); s != "" {
					return s
				}
			}
		}
	}
	return ""
}

func conversationSnapshotFrame(ps *phoneSessions, sessionID, workspace, convSub, deliveryKind string, ordinal int, rows []any, mode, phase string) map[string]any {
	if convSub == "" {
		convSub = sessionID + ":sub"
	}
	if deliveryKind == "" {
		deliveryKind = "initial"
	}
	if rows == nil {
		rows = []any{}
	}
	if mode == "" {
		mode = "build"
	}
	if phase == "" {
		phase = "running"
	}
	// Control/availability/inputRouting mirror the official desktop's
	// projection. canStop/activeWorks stay empty in snapshots (they ride the
	// state.updated control patches pushed on turn start/end).
	control := map[string]any{
		"phase": phase, "sessionEnded": false,
		"canStop": false, "stopState": "idle", "stopTargetKind": "unknown",
		"activeWorks": []any{}, "lastError": nil, "apiRetry": nil,
	}
	// A snapshot re-bases the client's transcript seq — reset the delta
	// ledger so the next deltas frame continues from the snapshot's seq.
	snapSeq, _ := ps.convoFrameSeq(true)
	title := sessionTitle(ps, sessionID)
	titleSource := "default"
	if title != "" {
		titleSource = "generated"
	}
	// The composer's "/" palette lists the session's slash commands from this
	// snapshot field; without it the palette renders "没有匹配的命令".
	snapshot := map[string]any{
		"protocolVersion": 1,
		"sessionId":       sessionID,
		"logEpoch":        "0",
		"seq":             snapSeq,
		"revision":        0,
		"control":         control,
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
		"inputRouting":        map[string]any{"mode": "startNow"},
		"meta":                map[string]any{"title": title, "titleSource": titleSource},
		"config":              ps.modelCfg(),
		"modelTransition":     nil,
		"usage":               ps.usageCfg(),
		"queue":               map[string]any{"items": []any{}, "autoDrain": true},
		"pendingInteractions": ps.pendingInteractionsPayload(),
		"pendingCommands":     []any{},
		"backgroundWorks":     []any{},
		"subagents": map[string]any{
			"revision": 0, "childSessionIds": []any{}, "running": []any{}, "endedTotal": 0,
		},
		"goal":                   nil,
		"plan":                   nil,
		"workspaceHookAdmission": nil,
		"rows": map[string]any{
			"window":     rows,
			"totalCount": len(rows),
			"firstRowId": nil,
		},
	}
	return map[string]any{
		"wireVersion":         3,
		"kind":                "complete",
		"deliveryKind":        deliveryKind,
		"logicalFrameId":      uuidNew(),
		"logicalFrameOrdinal": ordinal,
		"topic":               "conversation/" + sessionID,
		"subscriptionId":      convSub,
		"frame": map[string]any{
			"topic":          "conversation/" + sessionID,
			"subscriptionId": convSub,
			"fromSeq":        0,
			"toSeq":          1,
			"sentAt":         time.Now().UnixMilli(),
			"payload": map[string]any{
				"kind":     "snapshot",
				"snapshot": snapshot,
			},
		},
	}
}
