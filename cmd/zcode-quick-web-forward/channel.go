// Phone channel-call dispatch: EventListen subscriptions, method
// translation onto the engine's ZCode Protocol, and desktop-owned
// services (model-provider, zcode-task, settings, terminal, zcode-agent).

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	enginepkg "github.com/friddle/zcode-quick-web-forward/internal/engine"
	"github.com/friddle/zcode-quick-web-forward/internal/relay"
	"github.com/friddle/zcode-quick-web-forward/internal/terminal"
	"github.com/friddle/zcode-quick-web-forward/internal/zcode"
)

// handleChannelCall answers phone channel calls. Desktop-owned services
// (model-provider, zcode-task, setting, window-controller, …) are answered
// from the real ZCode state; the app-server engine gets only the calls it
// actually implements (session/* conversation traffic).

func handleChannelCall(engine *relay.BridgeEngine, send func(any), workspaces []string, ps *phoneSessions, engClient *enginepkg.Client, termSvc *terminal.Service) func(*relay.ChannelCall) {
	return func(c *relay.ChannelCall) {
		fmt.Printf("zcode: [call] kind=%d id=%d %s.%s\n", c.Kind, c.ID, c.ChannelName, c.Name)
		// EventListen (102): record the subscription so we can push EventFire.
		if c.Kind == 102 {
			switch c.Name {
			case "onDynamicConversationFrame":
				ps.convListener = c.ID
				fmt.Printf("zcode: [listen] onDynamicConversationFrame id=%d (监听 ✓)\n", c.ID)
			case "onDynamicSessionsIndexFrame":
				ps.indexListener = c.ID
				fmt.Printf("zcode: [listen] onDynamicSessionsIndexFrame id=%d (监听 ✓)\n", c.ID)
			case "onAgentRuntimeLifecycle", "onAgentRuntimeRestarted":
				ps.runtimeListener = c.ID
				fmt.Printf("zcode: [listen] %s id=%d (监听 ✓)\n", c.Name, c.ID)
			case "onDynamicData":
				// terminal/onDynamicData — c.Arg may be the terminal id string,
				// or nil when the phone subscribes a single global listener.
				id, _ := c.Arg.(string)
				_ = termSvc.SetDataListener(id, c.ID)
				fmt.Printf("zcode: [listen] terminal.onDynamicData id=%d term=%q (监听 ✓)\n", c.ID, id)
			case "onDynamicExit":
				id, _ := c.Arg.(string)
				_ = termSvc.SetExitListener(id, c.ID)
				fmt.Printf("zcode: [listen] terminal.onDynamicExit id=%d term=%q (监听 ✓)\n", c.ID, id)
			case "onDidChangeProviderRegistry":
				ps.recordListener("providerRegistry", c.ID)
				fmt.Printf("zcode: [listen] onDidChangeProviderRegistry id=%d (监听 ✓)\n", c.ID)
			case "onDynamicWorkspaceEvent":
				ps.recordListener("workspaceEvent", c.ID)
				fmt.Printf("zcode: [listen] onDynamicWorkspaceEvent id=%d (监听 ✓)\n", c.ID)
			case "onDynamicControllerFrame":
				ps.recordListener("controllerFrame", c.ID)
				fmt.Printf("zcode: [listen] onDynamicControllerFrame id=%d (监听 ✓)\n", c.ID)
			case "onMessage":
				// Transport-level socket event (phone internals); nothing for
				// us to push — ack by recording so it never stalls.
				ps.recordListener("onMessage", c.ID)
				fmt.Printf("zcode: [listen] onMessage id=%d (记录，无需推送)\n", c.ID)
			default:
				fmt.Printf("zcode: [listen] UNTRACKED subscribe %s id=%d (未监听，EventListen 未记录)\n", c.Name, c.ID)
			}
			return
		}
		if c.Kind != 100 || c.ID == 0 {
			return
		}
		if answerDesktopChannel(engine, c, send, workspaces, ps, engClient, termSvc) {
			fmt.Printf("zcode: [done] %s.%s answered locally\n", c.ChannelName, c.Name)
			return
		}
		engine.RegisterCall(c.ID)
		method, params := translateChannelMethod(c, workspaces)
		if method == "__local__" {
			engine.ReplyChannelPromise(c.ID, []byte(`{"runtimeModel":null}`), send)
			fmt.Printf("zcode: [done] %s.%s handled as __local__\n", c.ChannelName, c.Name)
			return
		}
		engine.WriteToServer(fmt.Sprintf(`{"id":%d,"method":%q,"params":%s}`, c.ID, method, params))
		fmt.Printf("zcode: [fwd] %s.%s -> engine %s\n", c.ChannelName, c.Name, method)
	}
}

// translateChannelMethod maps phone channel calls onto the engine's real
// ZCode Protocol methods. The phone's zcode-session/* names differ from the
// engine's workspace/* + session/* methods; the engine also persists model
// selection to ~/.zcode/cli/config.json automatically.
func translateChannelMethod(c *relay.ChannelCall, workspaces []string) (method, params string) {
	raw := []byte("null")
	if c.Arg != nil {
		if b, ok := c.Arg.(json.RawMessage); ok && len(b) > 0 {
			raw = b
		} else if b, err := json.Marshal(c.Arg); err == nil {
			raw = b
		}
	}
	key := c.ChannelName + "/" + c.Name
	// helper to wrap args as {workspace:{...}} — engine workspace/* requires
	// workspaceKey, which the phone doesn't send; derive it from the bridge.
	withWorkspace := func(body map[string]any) string {
		if body == nil {
			body = map[string]any{}
		}
		var in struct {
			WorkspacePath     string `json:"workspacePath"`
			WorkspaceIdentity string `json:"workspaceIdentity"`
		}
		_ = json.Unmarshal(raw, &in)
		ws := map[string]any{"workspacePath": in.WorkspacePath}
		if in.WorkspaceIdentity != "" {
			ws["workspaceIdentity"] = in.WorkspaceIdentity
		}
		ws["workspaceKey"] = in.WorkspacePath
		if in.WorkspacePath == "" && len(workspaces) > 0 {
			ws["workspaceKey"] = workspaces[0]
			ws["workspacePath"] = workspaces[0]
		}
		body["workspace"] = ws
		b, _ := json.Marshal(body)
		return string(b)
	}

	switch key {
	case "zcode-session/setWorkspaceDefaultModel":
		// keep the model field, wrap workspacePath/Identity into workspace.
		var in struct {
			WorkspacePath     string `json:"workspacePath"`
			WorkspaceIdentity string `json:"workspaceIdentity"`
			Model             any    `json:"model"`
		}
		_ = json.Unmarshal(raw, &in)
		body := map[string]any{}
		if in.Model != nil {
			body["model"] = in.Model
		}
		return "workspace/setDefaultModel", withWorkspace(body)
	case "zcode-session/readWorkspaceState":
		return "workspace/readState", withWorkspace(nil)
	case "zcode-session/setModel":
		// session/setModel is strict: only sessionId + model (+persist flag).
		var in struct {
			SessionID string `json:"sessionId"`
			Model     any    `json:"model"`
		}
		_ = json.Unmarshal(raw, &in)
		body := map[string]any{}
		if in.SessionID != "" {
			body["sessionId"] = in.SessionID
		}
		if in.Model != nil {
			body["model"] = in.Model
		}
		body["persistAsWorkspaceLastUsed"] = true
		b, _ := json.Marshal(body)
		return "session/setModel", string(b)
	case "zcode-session/setThoughtLevel":
		return "session/setThoughtLevel", string(raw)
	case "zcode-session/setWorkspaceDefaultThoughtLevel":
		return "workspace/setDefaultThoughtLevel", withWorkspace(map[string]any{})
	case "zcode-session/closeDeferredDraftSession", "zcode-session/closeSession":
		// Engine schemas are strict (unknown keys like workspacePath fail with
		// ZodError -32602) — forward only the fields session/close accepts.
		var in struct {
			SessionID string `json:"sessionId"`
		}
		_ = json.Unmarshal(raw, &in)
		body := map[string]any{}
		if in.SessionID != "" {
			body["sessionId"] = in.SessionID
		}
		b, _ := json.Marshal(body)
		return "session/close", string(b)
	case "zcode-session/readSession":
		// Same strictness: the phone adds workspacePath/workspaceIdentity,
		// which session/read rejects.
		var in struct {
			SessionID string `json:"sessionId"`
		}
		_ = json.Unmarshal(raw, &in)
		b, _ := json.Marshal(map[string]any{"sessionId": in.SessionID})
		return "session/read", string(b)
	case "zcode-session/resolveRuntimeModelForV4":
		// Resolve a runtime model for a model ref; answer with a minimal
		// structure so the phone continues (the model itself is already
		// applied via session/setModel / session/create).
		return "__local__", string(raw)
	default:
		return key, string(raw)
	}
}

func answerDesktopChannel(engine *relay.BridgeEngine, c *relay.ChannelCall, send func(any), workspaces []string, ps *phoneSessions, engClient *enginepkg.Client, termSvc *terminal.Service) bool {
	reply := func(result any) {
		b, _ := json.Marshal(result)
		engine.ReplyChannelPromise(c.ID, b, send)
	}
	replyNil := func() {
		engine.ReplyChannelPromise(c.ID, []byte("null"), send)
	}
	switch key := c.ChannelName + "/" + c.Name; key {
	case "terminal/create":
		var p struct {
			Cols int    `json:"cols"`
			Rows int    `json:"rows"`
			Cwd  string `json:"cwd"`
		}
		var raw json.RawMessage
		if b, ok := c.Arg.(json.RawMessage); ok {
			raw = b
		} else if b, err := json.Marshal(c.Arg); err == nil {
			raw = b
		}
		_ = json.Unmarshal(raw, &p)
		desc, err := termSvc.Create(p.Cols, p.Rows, p.Cwd)
		if err != nil {
			reply(map[string]any{"error": err.Error()})
			return true
		}
		reply(desc)
		fmt.Printf("zcode: terminal created id=%s\n", desc["id"])
	case "terminal/write":
		var p struct {
			ID   string `json:"id"`
			Data string `json:"data"`
		}
		var raw json.RawMessage
		if b, ok := c.Arg.(json.RawMessage); ok {
			raw = b
		} else if b, err := json.Marshal(c.Arg); err == nil {
			raw = b
		}
		_ = json.Unmarshal(raw, &p)
		if err := termSvc.Write(p.ID, p.Data); err != nil {
			reply(map[string]any{"error": err.Error()})
			return true
		}
		replyNil()
	case "terminal/resize":
		var p struct {
			ID   string `json:"id"`
			Cols int    `json:"cols"`
			Rows int    `json:"rows"`
		}
		var raw json.RawMessage
		if b, ok := c.Arg.(json.RawMessage); ok {
			raw = b
		} else if b, err := json.Marshal(c.Arg); err == nil {
			raw = b
		}
		_ = json.Unmarshal(raw, &p)
		if err := termSvc.Resize(p.ID, p.Cols, p.Rows); err != nil {
			reply(map[string]any{"error": err.Error()})
			return true
		}
		replyNil()
	case "terminal/dispose":
		var p struct {
			ID string `json:"id"`
		}
		var raw json.RawMessage
		if b, ok := c.Arg.(json.RawMessage); ok {
			raw = b
		} else if b, err := json.Marshal(c.Arg); err == nil {
			raw = b
		}
		_ = json.Unmarshal(raw, &p)
		termSvc.Dispose(p.ID)
		replyNil()
	case "model-provider/getAll", "model-provider/getAllCached":
		reply(providerPayload())
	case "model-provider/getDisplayOrder":
		order := make([]any, 0, len(zcode.Providers()))
		for _, p := range zcode.Providers() {
			order = append(order, p.ID)
		}
		reply(order)
	case "model-provider/getProviderRegistrySnapshot":
		reply(providerPayload())
	case "setting/get":
		homeDir, _ := os.UserHomeDir()
		reply(map[string]any{
			"language":       "en",
			"locale":         "zh-CN",
			"dataBaseDir":    homeDir,
			"defaultHomeDir": homeDir,
		})
	case "system/info":
		reply(map[string]any{
			"version":       "0.7.0",
			"appName":       "zcode-quick-web-forward",
			"platform":      "linux",
			"arch":          "amd64",
			"nodeVersion":   "",
			"runtime":       "web-remote",
			"home":          "",
			"userName":      "",
			"workspacePath": "/home/friddle/zqf-work",
		})
	case "setting/update":
		reply(map[string]any{})
	case "oauth/restoreCachedSessionState":
		reply(map[string]any{})
	case "oauth/getActiveProvider":
		reply(nil)
	case "git/refresh":
		reply([]any{})
	case "coding-plan-subscription/getBillingDiscount", "coding-plan-subscription/getManualClaimPlanPreviews":
		reply(map[string]any{})
	case "usage-stats/getEntitlementSnapshot":
		// The engine has no usage-stats service. Return the phone's neutral
		// state shape (snapshot:null) so the UI shows no plan/entitlement.
		reply(map[string]any{"snapshot": nil})
	case "file/ensureConversationWorkspace":
		// Phone-side file service: confirm the workspace dir for a conversation
		// (returns {path}). The engine has no file/* protocol — resolve the
		// path from the active workspace / session.
		pth := ""
		var fp struct {
			SessionID      string `json:"sessionId"`
			WorkspacePath  string `json:"workspacePath"`
			WorkspaceID    string `json:"workspaceId"`
			LocalWorkspace string `json:"localWorkspacePath"`
		}
		var raw json.RawMessage
		if b, ok := c.Arg.(json.RawMessage); ok {
			raw = b
		} else if b, err := json.Marshal(c.Arg); err == nil {
			raw = b
		}
		_ = json.Unmarshal(raw, &fp)
		pth = firstNonEmpty(fp.WorkspacePath, fp.LocalWorkspace, fp.WorkspaceID, ps.workspacePath)
		if pth == "" && len(workspaces) > 0 {
			pth = workspaces[0]
		}
		fmt.Printf("zcode: file.ensureConversationWorkspace path=%q session=%q\n", pth, fp.SessionID)
		reply(map[string]any{"path": pth})
	case "zcode-task/getTaskSessionFilePath":
		reply(map[string]any{"path": nil, "exists": false})
	case "zcode-task/getTaskNativeSessionLogFile":
		reply(map[string]any{"provider": nil, "path": nil, "exists": false})
	case "zcode-task/getTaskSessionId", "zcode-task/getTaskNativeSessionId":
		reply(map[string]any{"sessionId": nil})
	case "settings-sync/getFirstRunPromptState":
		// Returning handled:true suppresses the "欢迎使用 ZCode" first-run
		// wizard on every new pairing session (the phone checks e.handled
		// before opening it).
		reply(map[string]any{"handled": true})
	case "settings-sync/markFirstRunPromptHandled":
		reply(map[string]any{})
	case "zcode-task/getWorkspaceProviderConfigFile":
		reply(nil)
	case "zcode-task/listTasks", "zcode-task/listPinnedTasks", "zcode-task/listArchivedTasks":
		kind := ""
		switch c.Name {
		case "listPinnedTasks":
			kind = "pinned"
		case "listArchivedTasks":
			kind = "archived"
		}
		reply(taskListPayload(kind, ps))
	case "zcode-task/listPinnedTaskIds", "zcode-task/listArchivedTaskIds", "zcode-task/listDeletedTaskIds", "zcode-task/listRecentTasks":
		kind := ""
		switch c.Name {
		case "listPinnedTaskIds":
			kind = "pinned"
		case "listArchivedTaskIds":
			kind = "archived"
		case "listDeletedTaskIds":
			// Must be the real deleted set: the UI subtracts these ids from
			// merged views, so answering with archived ids would hide tasks.
			kind = "deleted"
		}
		ids, _ := zcode.ListTaskIDs(kind)
		anyIDs := make([]any, 0, len(ids))
		for _, id := range ids {
			anyIDs = append(anyIDs, id)
		}
		reply(anyIDs)
	case "zcode-task/deleteTask", "zcode-task/archiveTask", "zcode-task/unarchiveTask",
		"zcode-task/setTaskPinned", "zcode-task/setTaskUnread", "zcode-task/renameTask":
		// The phone's task-list actions. Persisting here is what makes them
		// survive reloads — the UI itself only removes the row optimistically.
		var a struct {
			TaskID        string `json:"taskId"`
			WorkspacePath string `json:"workspacePath"`
			Title         string `json:"title"`
			Pinned        *bool  `json:"pinned"`
			Unread        *bool  `json:"unread"`
		}
		switch v := c.Arg.(type) {
		case map[string]any:
			raw, _ := json.Marshal(v)
			_ = json.Unmarshal(raw, &a)
		case json.RawMessage:
			_ = json.Unmarshal(v, &a)
		}
		if a.TaskID == "" {
			reply(nil)
			return true
		}
		var err error
		switch c.Name {
		case "deleteTask":
			err = zcode.SetTaskFlags(a.TaskID, boolPtr(true), nil, nil)
		case "archiveTask":
			err = zcode.SetTaskFlags(a.TaskID, nil, boolPtr(true), nil)
		case "unarchiveTask":
			err = zcode.SetTaskFlags(a.TaskID, nil, boolPtr(false), nil)
		case "setTaskPinned":
			err = zcode.SetTaskFlags(a.TaskID, nil, nil, a.Pinned)
		case "setTaskUnread":
			err = zcode.SetTaskUnread(a.TaskID, a.Unread != nil && *a.Unread)
		case "renameTask":
			err = zcode.RenameTask(a.TaskID, a.Title)
		}
		if err != nil {
			fmt.Printf("zcode: %s %s failed: %v\n", c.Name, a.TaskID, err)
			reply(nil)
			return true
		}
		fmt.Printf("zcode: task %s %s persisted\n", c.Name, a.TaskID)
		if t, ok, gerr := zcode.GetTask(a.TaskID); gerr == nil && ok {
			// Echo the updated row: the UI's .then() uses it as the new state.
			reply(taskItemPayload(t))
		} else {
			reply(nil)
		}
		return true
	case "window-controller/subscribeControllerV4", "window-controller/getSnapshot", "window-controller/getControllerSnapshot":
		reply([]any{})
		// The official client renders the task list's live status from the
		// controller/tasks-index topic. Push a snapshot when it subscribes
		// (getSnapshot/getControllerSnapshot callers get the RPC ack only).
		if c.Name == "subscribeControllerV4" {
			go func() {
				time.Sleep(250 * time.Millisecond)
				if id := ps.listener("controllerFrame"); id > 0 {
					b, _ := json.Marshal(tasksIndexFrame(ps))
					engine.SendChannelEvent(id, b, send)
					fmt.Println("zcode: pushed controller/tasks-index snapshot")
				}
			}()
		}
	case "zcode-agent/syncAppRuntimePreferences", "bots/syncAppRuntimePreferences":
		// Fire-and-forget settings sync ({askUserQuestionAutoResolutionEnabled,
		// modelIoFullRetentionEnabled}); acknowledge locally — forwarding it to
		// the engine fails with Method not found.
		reply(map[string]any{})
	case "client-scenes/list":
		reply([]any{})
	case "subagents/list":
		reply([]any{})
	case "zcode-agent/getAgentRuntimeLifecycle":
		reply(map[string]any{"status": "running"})
	case "zcode-agent/helloConversationV4":
		// The phone's strict zod schema (Nle) validates this exact shape:
		// .strict() rejects any extra field, all fields required, and
		// deliveryProfile must pair with clientMode.
		reply(map[string]any{
			"kind":            "hello",
			"protocolVersion": 3,
			"connectionId":    uuidNew(),
			"clientMode":      "web-remote-replayable",
			"deliveryProfile": "replayable",
			"serverTime":      time.Now().UnixMilli(),
			"capabilities": map[string]any{
				"nativeDialogs": true,
				"localTerminal": true,
				"binaryFrames":  true,
				"compression":   "none",
			},
			"auth": map[string]any{},
		})
	case "zcode-agent/initializeConversationV4":
		reply(map[string]any{})
	case "zcode-agent/subscribeConversationV4", "zcode-agent/subscribeSessionsIndexV4", "zcode-agent/resyncConversationV4", "zcode-agent/resyncSessionsIndexV4":
		// The subscriptionId must be stable and echoed in resync (never
		// minted fresh) — a mismatch throws resyncGenerationMismatch. We
		// derive it from the sessionId so pushed frames and the ack agree.
		// The phone may ask to subscribe/resync a SPECIFIC session (opening an
		// old task from the list) — honor that instead of only the bridge's
		// current session.
		var subReq struct {
			SessionID string `json:"sessionId"`
		}
		if raw, ok := c.Arg.(json.RawMessage); ok {
			_ = json.Unmarshal(raw, &subReq)
		} else if b, err := json.Marshal(c.Arg); err == nil {
			_ = json.Unmarshal(b, &subReq)
		}
		if subReq.SessionID != "" {
			ps.setSession(subReq.SessionID, "") // keep workspacePath
		}
		sid, _ := ps.get()
		subID := ps.convSub()
		if subID == "" {
			if sid != "" {
				subID = sid + ":sub"
			} else {
				subID = uuidNew()
			}
			ps.setConvSubscription(subID)
		}
		// A resync means the phone already applied our snapshot base, so the
		// ack mode must be "resume" (and logEpoch must match the base). A
		// fresh subscribe is "snapshot".
		mode := "snapshot"
		if c.Name == "resyncConversationV4" || c.Name == "resyncSessionsIndexV4" {
			mode = "resume"
		}
		ack := map[string]any{
			"ack": map[string]any{
				"subscriptionId": subID,
				"mode":           mode,
				"logEpoch":       "0", // must be a string
			},
		}
		reply(ack)
		go pushSubscriptionFrames(engine, send, ps, c, engClient)
	case "zcode-agent/sendConversationCommandV4":
		ack := bridgeSendCommand(c, engClient, ps, workspaces, engine, send)
		reply(ack)
		if sess, ok := ack["result"].(map[string]any); ok {
			if s, _ := sess["sessionId"].(string); s != "" {
				ps.setSession(s, "")
				go pushConversationFrame(engine, send, ps, ack)
			}
		}
		// When a text message was accepted, render the user's message in the
		// conversation immediately (the engine's transcript only comes back at
		// turn end, so without this the phone stays blank while it runs).
		if txt, ok := ack["userTextSent"].(string); ok && txt != "" {
			sid, _ := ps.get()
			if sid != "" {
				cmdID, _ := ack["commandId"].(string)
				clientID, _ := ack["clientId"].(string)
				go func() {
					time.Sleep(150 * time.Millisecond)
					ps.mu.Lock()
					convID, convSub, ws := ps.convListener, ps.convSubscription, ps.workspacePath
					indexID := ps.indexListener
					ps.mu.Unlock()
					now := time.Now().UnixMilli()
					turnID := "turn-" + sid
					if cmdID == "" {
						cmdID = "cmd-" + shortSessionID(sid)
					}
					// Immediate send feedback, mirroring the desktop: a running
					// turn header (the "已工作" indicator) plus the user's
					// message row, and the projection flipped to running.
					hdr := map[string]any{
						"rowId":           1,
						"turnId":          turnID,
						"createdAt":       now,
						"createdAtSeq":    1,
						"kind":            "turnHeader",
						"origin":          "userInput",
						"executionKind":   "agent",
						"state":           "running",
						"startedAt":       now,
						"sourceCommandId": cmdID,
					}
					row := map[string]any{
						"rowId":               2,
						"turnId":              turnID,
						"createdAt":           now,
						"createdAtSeq":        2,
						"kind":                "userInput",
						"text":                txt,
						"origin":              "realUser",
						"sourceCommandId":     cmdID,
						"rootSourceCommandId": cmdID,
					}
					if clientID != "" {
						row["clientId"] = clientID
					}
					b, _ := json.Marshal(conversationSnapshotFrame(ps, sid, ws, convSub, "recovery", ps.nextOrdinal(), []any{hdr, row}, ps.collabMode, "running"))
					ps.rememberRows([]any{hdr, row})
					engine.SendChannelEvent(convID, b, send)
					// The desktop's running-state control patch: stoppable,
					// primaryTurn active work, follow-ups route to the queue.
					cb, _ := json.Marshal(stateUpdatedFrame(sid, "running", convSub, ps.nextOrdinal()))
					engine.SendChannelEvent(convID, cb, send)
					fmt.Printf("zcode: pushed running-turn snapshot session=%s text=%q\n", sid, txt)
					// Flip the sidebar entry to running as well.
					if indexID > 0 {
						ib, _ := json.Marshal(sessionsIndexFrame(convSub, ps))
						engine.SendChannelEvent(indexID, ib, send)
						fmt.Println("zcode: pushed sessions-index snapshot")
					}
					if controllerID := ps.listener("controllerFrame"); controllerID > 0 {
						cb, _ := json.Marshal(tasksIndexFrame(ps))
						engine.SendChannelEvent(controllerID, cb, send)
					}
				}()
			}
		}
		// A collaboration-mode switch needs a snapshot push so the phone's
		// picker reflects the new mode (the engine doesn't relay setMode).
		if mode, ok := ack["modeChanged"].(string); ok && mode != "" {
			sid, _ := ps.get()
			if sid != "" {
				go func() {
					time.Sleep(400 * time.Millisecond)
					ps.mu.Lock()
					convID, convSub := ps.convListener, ps.convSubscription
					ps.mu.Unlock()
					if convID > 0 {
						b, _ := json.Marshal(conversationSnapshotFrame(ps, sid, ps.workspacePath, convSub, "recovery", ps.nextOrdinal(), nil, mode, "running"))
						engine.SendChannelEvent(convID, b, send)
						fmt.Printf("zcode: pushed mode snapshot session=%s mode=%s\n", sid, mode)
					}
				}()
			}
		}
	case "zcode-agent/queryConversationCommandsV4":
		reply(map[string]any{"results": []any{}})
	case "zcode-agent/unsubscribeConversationV4", "zcode-agent/unsubscribeSessionsIndexV4":
		reply(map[string]any{})
	case "zcode-agent/conversationRowsRangeV4":
		// Official reply shape: {rows, atSeq, atLogEpoch, hasMore}. Serve the
		// remembered rows of the live session; empty for anything else.
		var rq struct {
			SessionID string `json:"sessionId"`
		}
		if raw, ok := c.Arg.(json.RawMessage); ok {
			_ = json.Unmarshal(raw, &rq)
		} else if b, err := json.Marshal(c.Arg); err == nil {
			_ = json.Unmarshal(b, &rq)
		}
		rows, sid := ps.snapshotRows(), ""
		if rq.SessionID != "" {
			sid, _ = ps.get()
			if sid != rq.SessionID {
				rows = []any{}
			}
		}
		reply(map[string]any{
			"rows":       rows,
			"atSeq":      len(rows),
			"atLogEpoch": "0",
			"hasMore":    false,
		})
	case "zcode-agent/conversationPlansV4", "zcode-agent/conversationFileChangesV4", "zcode-agent/conversationFileRewindPreviewV4":
		reply(map[string]any{})
	case "coding-plan-subscription/getStaticTeamProducts":
		reply([]any{})
	case "coding-plan-subscription/getEnterprisePricing":
		reply(map[string]any{})
	case "settings-sync/detect":
		reply(map[string]any{})
	case "broadcast/getState":
		reply(map[string]any{})
	case "broadcast/listeners":
		reply([]any{})
	case "zcode-agent/getDynamicSessionsIndex", "zcode-agent/getConversationRowsRange", "zcode-agent/getConversationFileChanges":
		reply(map[string]any{})
	default:
		return false
	}
	fmt.Printf("zcode: answered %s.%s from real state\n", c.ChannelName, c.Name)
	return true
}
