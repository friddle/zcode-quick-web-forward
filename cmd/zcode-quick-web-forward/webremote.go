// Web-remote relay connection: relay sender routing, phone pairing URL
// printout, bootstrap/workspace-list/bridge-open request handling and the
// task/workspace payload shapes.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	enginepkg "github.com/friddle/zcode-quick-web-forward/internal/engine"
	"github.com/friddle/zcode-quick-web-forward/internal/relay"
	"github.com/friddle/zcode-quick-web-forward/internal/terminal"
	"github.com/friddle/zcode-quick-web-forward/internal/zcode"
)

// relaySender routes frames to whichever relay connection is current.
type relaySender struct {
	mu sync.Mutex
	fn func(any)
}

func (s *relaySender) set(fn func(any)) {
	s.mu.Lock()
	s.fn = fn
	s.mu.Unlock()
}

func (s *relaySender) send(v any) {
	s.mu.Lock()
	fn := s.fn
	s.mu.Unlock()
	if fn != nil {
		fn(v)
	}
}

func startWebRemote(origin, region string, engine *relay.BridgeEngine, sender *relaySender, restartEngine func(), workspaces []string, ps *phoneSessions, engClient *enginepkg.Client, termSvc *terminal.Service) {
	cache, err := os.UserCacheDir()
	if err == nil {
		cache = filepath.Join(cache, "zcode-quick-web-forward")
	}
	opts := relay.Options{
		Origin:     origin,
		DeviceMid:  loadOrCreateDeviceMid(cache),
		DeviceName: hostname(),
		AppVersion: version,
		StatePath:  filepath.Join(cache, "webremote-state.json"),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	relay.Run(ctx, opts, relay.Handler{
		OnReady: func(s relay.Session) {
			fmt.Println()
			fmt.Println("==========================================")
			fmt.Println("  ZCode web-remote / 手机配对链接(手机浏览器打开):")
			fmt.Println("  " + s.PhoneURL)
			fmt.Printf("  (relay %s, device %s, region %s)\n", origin, s.DeviceSid, region)
			fmt.Println("==========================================")
			if path, err := exec.LookPath("qrencode"); err == nil {
				c := exec.Command(path, "-t", "UTF8", s.PhoneURL)
				if out, err := c.Output(); err == nil {
					fmt.Println(string(out))
				}
			}
		},
		OnPaired: func(string) {
			fmt.Println()
			fmt.Println("*** web-remote: 手机已配对接入 ***")
			go func() {
				time.Sleep(800 * time.Millisecond)
				sender.send(workspaceListPush(workspaces, ps))
				fmt.Println("zcode: workspace list pushed to phone")
			}()
		},
		OnData: func(payload json.RawMessage, reply func(any)) {
			sender.set(reply)
			handleRemoteData(payload, reply, engine, restartEngine, sender.send, workspaces, ps, engClient, termSvc)
		},
	})
}

func workspaceListPush(workspaces []string, ps *phoneSessions) map[string]any {
	wsList := make([]any, 0, len(workspaces))
	for _, w := range workspaces {
		wsList = append(wsList, map[string]any{
			"workspacePath":   w,
			"label":           filepath.Base(w),
			"kind":            "local",
			"connectionState": "connected",
		})
	}
	active := ""
	if len(workspaces) > 0 {
		active = workspaces[0]
	}
	return map[string]any{
		"zcode_type": "workspace-list-updated",
		"result": map[string]any{
			"workspaces":         wsList,
			"tasks":              taskListPayload("", ps),
			"activeWorkspaceKey": active,
		},
	}
}

func handleRemoteData(payload json.RawMessage, reply func(any), engine *relay.BridgeEngine, restartEngine func(), replyFrames func(any), workspaces []string, ps *phoneSessions, engClient *enginepkg.Client, termSvc *terminal.Service) {
	var p struct {
		ZcodeType string `json:"zcode_type"`
		RequestID string `json:"requestId"`
	}
	if json.Unmarshal(payload, &p) != nil {
		return
	}
	if p.ZcodeType == "rpc-frame" || p.ZcodeType == "rpc-frame-ack" {
		engine.HandlePhonePayload(payload, reply, handleChannelCall(engine, reply, workspaces, ps, engClient, termSvc))
		return
	}
	if p.RequestID == "" {
		return
	}
	wsList := workspaceListPayload(workspaces)
	active := ""
	if len(workspaces) > 0 {
		active = workspaces[0]
	}
	switch p.ZcodeType {
	case "bootstrap-request":
		reply(map[string]any{
			"zcode_type": "bootstrap-response", "requestId": p.RequestID, "success": true,
			"result": map[string]any{
				"windowControlSessionId": "zqf",
				"workspaces":             wsList,
				"tasks":                  taskListPayload("", ps),
			},
		})
	case "workspace-list-request":
		reply(map[string]any{
			"zcode_type": "workspace-list-response", "requestId": p.RequestID, "success": true,
			"result": map[string]any{"workspaces": wsList, "tasks": taskListPayload("", ps)},
		})
	case "workspace-bridge-open":
		var v struct {
			RequestID        string `json:"requestId"`
			BridgeSessionID  string `json:"bridgeSessionId"`
			BridgeGeneration *int   `json:"bridgeGeneration"`
			RecoveryID       string `json:"recoveryId"`
			WorkspaceKey     string `json:"workspaceKey"`
			TaskID           string `json:"taskId"`
		}
		if json.Unmarshal(payload, &v) != nil || v.BridgeSessionID == "" {
			reply(map[string]any{
				"zcode_type": "workspace-bridge-error", "requestId": p.RequestID,
				"reason": "unexpected-error", "error": "malformed bridge-open",
			})
			return
		}
		engine.SetIdentity(v.BridgeSessionID, v.BridgeGeneration, v.RecoveryID)
		ps.mu.Lock()
		ps.workspacePath = v.WorkspaceKey
		ps.mu.Unlock()
		if restartEngine != nil {
			restartEngine()
		}
		bridge := map[string]any{
			"kind":            "local",
			"bridgeSessionId": v.BridgeSessionID,
			"workspaceKey":    v.WorkspaceKey,
			"workspacePath":   v.WorkspaceKey,
		}
		if v.BridgeGeneration != nil {
			bridge["bridgeGeneration"] = *v.BridgeGeneration
		}
		if v.RecoveryID != "" {
			bridge["recoveryId"] = v.RecoveryID
		}
		if v.TaskID != "" {
			bridge["initialTaskId"] = v.TaskID
		}
		ready := map[string]any{
			"zcode_type": "workspace-bridge-ready", "requestId": v.RequestID,
			"bridgeSessionId": v.BridgeSessionID,
			"bridge":          bridge,
		}
		reply(ready)
		reply(map[string]any{
			"zcode_type": "workspace-list-updated",
			"result": map[string]any{
				"workspaces":         wsList,
				"tasks":              taskListPayload("", ps),
				"activeWorkspaceKey": active,
			},
		})
		go func() {
			time.Sleep(1200 * time.Millisecond)
			engine.SendChannelInitialize(replyFrames)
			fmt.Println("zcode: channel initialize sent")
		}()
	case "workspace-reconnect-request":
		reply(map[string]any{
			"zcode_type": "workspace-reconnect-response", "requestId": p.RequestID, "success": true,
		})
	}
}

func workspaceListPayload(workspaces []string) []any {
	wsList := make([]any, 0, len(workspaces))
	for _, w := range workspaces {
		if strings.HasPrefix(w, "remote:") {
			continue
		}
		wsList = append(wsList, map[string]any{
			"workspacePath":   w,
			"label":           filepath.Base(w),
			"kind":            "local",
			"connectionState": "connected",
		})
	}
	return wsList
}

func boolPtr(b bool) *bool { return &b }

// taskItemPayload renders one task-index row in the phone's expected item
// shape (also echoed back from archive/delete/pin/rename mutations).
func taskItemPayload(t zcode.Task) map[string]any {
	item := map[string]any{
		"taskId":         t.TaskID,
		"title":          t.Title, // must be a string or the phone renders [object Object]
		"workspacePath":  t.WorkspacePath,
		"workspaceLabel": pathLabel(t.WorkspacePath),
		"workspaceKind":  "local",
		"displayStatus":  displayStatus(t.Status),
		"createdAt":      t.CreatedAt,
		"updatedAt":      t.UpdatedAt,
	}
	if t.Pinned {
		item["pinned"] = true
	}
	if t.Archived {
		item["archived"] = true
	}
	if t.UnreadAt != nil {
		item["unreadAt"] = *t.UnreadAt
	}
	return item
}

func taskListPayload(kind string, ps *phoneSessions) []any {
	tasks, err := zcode.ListTasks("", kind)
	if err != nil {
		return []any{}
	}
	out := make([]any, 0, len(tasks))
	seen := map[string]bool{}
	for _, t := range tasks {
		// Only expose local tasks: remote SSH workspaces (workspaceKey with a
		// remote: prefix) have no matching local workspace the phone can open,
		// and the phone stalls waiting for a bridge it can never get.
		if strings.HasPrefix(t.WorkspaceKey, "remote:") {
			continue
		}
		seen[t.TaskID] = true
		out = append(out, taskItemPayload(t))
	}
	// Runtime tasks (created this session, not yet in the index) only belong
	// in the unfiltered list — adding them to pinned/archived/deleted views
	// would surface unarchived tasks in those views.
	if ps != nil && kind == "" {
		for _, rt := range ps.runtimeTaskList() {
			m, _ := rt.(map[string]any)
			if d, _ := m["draft"].(bool); d {
				continue // composer drafts aren't tasks yet
			}
			sid, _ := m["taskId"].(string)
			if sid == "" || seen[sid] {
				continue // already surfaced from the persisted task index
			}
			seen[sid] = true
			title, _ := m["title"].(string)
			ws, _ := m["workspacePath"].(string)
			item := map[string]any{
				"taskId":         sid,
				"title":          title,
				"workspacePath":  ws,
				"workspaceLabel": pathLabel(ws),
				"workspaceKind":  "local",
				"displayStatus":  "running",
				"createdAt":      m["createdAt"],
				"updatedAt":      m["updatedAt"],
			}
			out = append(out, item)
		}
	}
	return out
}

// pathLabel returns the last path segment (used for the phone's workspaceLabel).
func pathLabel(p string) string {
	p = strings.TrimRight(p, "/\\")
	if i := strings.LastIndexAny(p, "/\\"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// firstNonEmpty returns the first non-empty string argument.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func displayStatus(s string) string {
	switch strings.ToLower(s) {
	case "running", "in-progress", "active":
		return "running"
	case "completed", "completedSuccess", "completedInterrupted":
		return "completed"
	case "error":
		return "error"
	case "idle", "cancelled", "failed", "interrupted", "paused", "":
		return "idle"
	default:
		return "idle"
	}
}
