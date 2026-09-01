// Package zcode reads the real ZCode desktop/CLI state that a remote control
// session exposes to the phone: the task index (tasks-index.sqlite), the
// model-provider configuration (~/.zcode/v2/config.json, cli/config.json) and
// desktop settings (setting.json). Unlike a reverse-engineered stub, these
// come straight from the actual ZCode installation, so the phone sees the same
// workspaces and tasks the desktop does.
package zcode

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	_ "modernc.org/sqlite"
)

// Home returns the ZCode home directory (config/state lives under it).
func Home() string {
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".zcode")
	}
	return ".zcode"
}

// Task is one entry in the ZCode task index.
type Task struct {
	WorkspaceKey string `json:"workspaceKey"`
	WorkspacePath string `json:"workspacePath"`
	TaskID       string `json:"taskId"`
	Title        string `json:"title"`
	Status       string `json:"status,omitempty"`
	Mode         string `json:"mode,omitempty"`
	Model        string `json:"model,omitempty"`
	CreatedAt    int64  `json:"createdAt"`
	UpdatedAt    int64  `json:"updatedAt"`
	UnreadAt     *int64 `json:"unreadAt,omitempty"`
	Pinned       bool   `json:"pinned"`
	Archived     bool   `json:"archived"`
}

// openTaskDB opens the tasks-index sqlite database.
func openTaskDB() (*sql.DB, error) {
	path := filepath.Join(Home(), "v2", "tasks-index.sqlite")
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	return sql.Open("sqlite", "file:"+path+"?mode=ro")
}

// ListTasks returns tasks from the real task index, newest first. kind filters
// by archive/pin state ("" = all non-deleted).
func ListTasks(workspaceKey, kind string) ([]Task, error) {
	db, err := openTaskDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()

	where := []string{"deleted = 0"}
	args := []any{}
	if workspaceKey != "" {
		where = append(where, "workspace_key = ?")
		args = append(args, workspaceKey)
	}
	switch kind {
	case "pinned":
		where = append(where, "pinned = 1", "archived = 0")
	case "archived":
		where = append(where, "archived = 1")
	default:
		where = append(where, "archived = 0")
	}
	query := "SELECT workspace_key, workspace_path, task_id, title, task_status, mode, model, created_at, updated_at, unread_at, pinned, archived " +
		"FROM tasks WHERE " + strings.Join(where, " AND ") +
		" ORDER BY updated_at DESC"
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Task
	for rows.Next() {
		var t Task
		var status, mode, model sql.NullString
		var unreadAt sql.NullInt64
		if err := rows.Scan(&t.WorkspaceKey, &t.WorkspacePath, &t.TaskID, &t.Title,
			&status, &mode, &model, &t.CreatedAt, &t.UpdatedAt, &unreadAt, &t.Pinned, &t.Archived); err != nil {
			return nil, err
		}
		t.Status, t.Mode, t.Model = status.String, mode.String, model.String
		if unreadAt.Valid {
			v := unreadAt.Int64
			t.UnreadAt = &v
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListTaskIDs returns just the task ids (for pinned/archived/deleted id lists).
func ListTaskIDs(kind string) ([]string, error) {
	tasks, err := ListTasks("", kind)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(tasks))
	for _, t := range tasks {
		ids = append(ids, t.TaskID)
	}
	return ids, nil
}

// Workspaces returns the distinct local workspaces from the task index plus
// the desktop's last-known workspace. The task index is the authoritative
// list of directories ZCode has actually worked in.
func Workspaces() ([]Workspace, error) {
	db, err := openTaskDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.Query("SELECT DISTINCT workspace_key, workspace_path FROM tasks WHERE deleted = 0")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	seen := map[string]bool{}
	var out []Workspace
	for rows.Next() {
		var key, path string
		if err := rows.Scan(&key, &path); err != nil {
			return nil, err
		}
		if key == "" || seen[key] || strings.HasPrefix(key, "remote:") {
			continue
		}
		seen[key] = true
		out = append(out, Workspace{
			WorkspaceKey:  key,
			WorkspacePath: path,
			Label:         pathLabel(path),
			Kind:          "local",
			Connected:     true,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out, nil
}

// Workspace is one workspace exposed to the phone.
type Workspace struct {
	WorkspaceKey  string `json:"workspaceKey"`
	WorkspacePath string `json:"workspacePath"`
	Label         string `json:"label"`
	Kind          string `json:"kind"`
	Connected     bool   `json:"connectionState"`
}

func pathLabel(p string) string {
	p = strings.TrimRight(p, "/\\")
	if i := strings.LastIndexAny(p, "/\\"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// Providers returns model providers from ~/.zcode config (the same ones the
// desktop's modelProviderService.getAll returns).
func Providers() []Provider {
	providers := []Provider{}
	add := func(base map[string]any) {
		p, _ := base["provider"].(map[string]any)
		for name, raw := range p {
			obj, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			opts, _ := obj["options"].(map[string]any)
			prov := Provider{
				ID:       name,
				Name:     str(obj["name"]),
				Kind:     str(obj["kind"]),
				BaseURL:  str(opts["baseURL"]),
				APIKey:   str(opts["apiKey"]),
				Enabled:  boolv(obj["enabled"], true),
				Source:   str(obj["source"]),
				PresetID: str(obj["presetId"]),
			}
			if models, ok := obj["models"].(map[string]any); ok {
				names := make([]string, 0, len(models))
				for m := range models {
					names = append(names, m)
				}
				sort.Strings(names)
				prov.Models = names
			}
			providers = append(providers, prov)
		}
	}
	// desktop config: ~/.zcode/v2/config.json
	home := Home()
	add(readJSON(filepath.Join(home, "v2", "config.json")))
	// cli config: ~/.zcode/cli/config.json
	add(readJSON(filepath.Join(home, "cli", "config.json")))
	return providers
}

// Provider is one model provider entry.
type Provider struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Kind     string   `json:"apiFormat"`
	BaseURL  string   `json:"endpoints,omitempty"`
	APIKey   string   `json:"apiKey"`
	Enabled  bool     `json:"enabled"`
	Source   string   `json:"source,omitempty"`
	PresetID string   `json:"presetId,omitempty"`
	Models   []string `json:"models,omitempty"`
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func boolv(v any, def bool) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return def
}

func readJSON(path string) map[string]any {
	m := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

// Settings returns the desktop settings (setting.json), used for setting.get.
func Settings() map[string]any {
	return readJSON(filepath.Join(Home(), "v2", "setting.json"))
}

// ActiveWorkspace returns the desktop's last active local workspace, falling
// back to the first in the task index, else "". Used for the phone's active
// workspace and bootstrap view state.
func ActiveWorkspace() string {
	s := Settings()
	if lws, ok := s["lastWorkspaceSession"].([]any); ok && len(lws) > 0 {
		if first, ok := lws[0].(map[string]any); ok {
			if p := str(first["workspacePath"]); p != "" {
				return p
			}
		}
	}
	ws, _ := Workspaces()
	if len(ws) > 0 {
		return ws[0].WorkspaceKey
	}
	return ""
}

// DefaultModel returns the configured default model ref from
// ~/.zcode/cli/config.json (model.main, e.g. "bigmodel/GLM-5.3"), split into
// provider/model. Empty strings when unset.
func DefaultModel() (provider, model string) {
	cfg := readJSON(filepath.Join(Home(), "cli", "config.json"))
	mo, _ := cfg["model"].(map[string]any)
	ref, _ := mo["main"].(string)
	if ref == "" {
		return "", ""
	}
	if i := strings.Index(ref, "/"); i > 0 {
		return ref[:i], ref[i+1:]
	}
	return "", ref
}

var _ = fmt.Sprintf
