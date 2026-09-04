package main

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/friddle/zcode-quick-web-forward/internal/zcode"
)

// fixtures loads the distilled official-desktop reference shapes captured from
// a real web-remote session (see docs/web-remote-protocol.md).
func fixtures(t *testing.T) map[string]any {
	t.Helper()
	b, err := os.ReadFile("testdata/official-projection.json")
	if err != nil {
		t.Fatalf("fixtures: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("fixtures: %v", err)
	}
	return m
}

// mustMap digs a nested map path, failing the test when a key is missing —
// the official client's zod schemas are strict, so shape drift breaks the UI.
func mustMap(t *testing.T, v any, path ...string) map[string]any {
	t.Helper()
	for _, k := range path {
		m, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("%v: not an object (at %q)", path, k)
		}
		v, ok = m[k]
		if !ok {
			t.Fatalf("missing key %q in %v", k, path)
		}
	}
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%v: not an object", path)
	}
	return m
}

func snapshotOf(t *testing.T, frame map[string]any) map[string]any {
	t.Helper()
	return mustMap(t, mustMap(t, frame, "frame"), "payload", "snapshot")
}

// TestSnapshotMatchesOfficialShape asserts the conversation snapshot carries
// exactly the official top-level keys and the picker-critical config/usage
// blocks (testdata/official-projection.json).
func TestSnapshotMatchesOfficialShape(t *testing.T) {
	want := fixtures(t)
	wantKeys := want["snapshot"].(map[string]any)["keys"].([]any)

	ps := &phoneSessions{}
	ps.setSession("sess_t", "/ws")
	ps.setModelConfig("bigmodel", "GLM-5.3-Flash", "high")
	ps.setThoughtLevels([]string{"low", "high", "max"})
	ps.setContextUsage(13857, 1000000)
	ps.collabMode = "build"

	frame := conversationSnapshotFrame(ps, "sess_t", "/ws", "sess_t:sub", "initial", 1, nil, "build", "draft")
	// JSON round-trip so numeric comparisons see the wire types.
	b, _ := json.Marshal(frame)
	var rt map[string]any
	_ = json.Unmarshal(b, &rt)
	snap := snapshotOf(t, rt)

	for _, k := range wantKeys {
		if _, ok := snap[k.(string)]; !ok {
			t.Errorf("snapshot missing official key %q", k)
		}
	}

	cfg := mustMap(t, snap, "config")
	if cfg["thought"] != "high" || cfg["model"] != "GLM-5.3-Flash" || cfg["provider"] != "bigmodel" {
		t.Errorf("config = %v", cfg)
	}
	levels, ok := cfg["thoughtLevels"].([]any)
	if !ok || len(levels) != 3 || levels[0] != "low" || levels[2] != "max" {
		t.Fatalf("config.thoughtLevels = %v — picker hides when empty", cfg["thoughtLevels"])
	}
	if cfg["followupMode"] != "queue" || cfg["mode"] != "build" {
		t.Errorf("config followup/mode = %v", cfg)
	}

	usage := mustMap(t, snap, "usage")
	cw := mustMap(t, usage, "contextWindow")
	if cw["usedTokens"] != float64(13857) || cw["maxTokens"] != float64(1000000) {
		t.Errorf("contextWindow = %v", cw)
	}
	if _, ok := usage["cumulative"]; !ok {
		t.Error("usage.cumulative missing")
	}

	q := mustMap(t, snap, "queue")
	if q["autoDrain"] != true {
		t.Errorf("queue.autoDrain = %v, official sends true", q["autoDrain"])
	}

	avail := mustMap(t, snap, "availability")
	pause := mustMap(t, avail, "pauseGoal")
	if pause["reasonCode"] != "noGoalToPause" {
		t.Errorf("pauseGoal = %v", pause)
	}
}

// TestRunningControlPatchShape mirrors the official running-state patch: the
// desktop's "工作中 N 秒 / 停止生成" display is driven by exactly these fields.
func TestRunningControlPatchShape(t *testing.T) {
	frame := stateUpdatedFrame("sess_t", "running", "sess_t:sub", 1)
	b, _ := json.Marshal(frame)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	payload := mustMap(t, mustMap(t, m, "frame"), "payload")
	dl := payload["deltas"].([]any)[0].(map[string]any)
	if dl["op"] != "state.updated" {
		t.Fatalf("op = %v", dl["op"])
	}
	patch := dl["patch"].(map[string]any)

	control := patch["control"].(map[string]any)
	if control["phase"] != "running" || control["canStop"] != true || control["stopState"] != "stoppable" {
		t.Errorf("running control = %v", control)
	}
	works, ok := control["activeWorks"].([]any)
	if !ok || len(works) != 1 {
		t.Fatalf("activeWorks = %v", control["activeWorks"])
	}
	w := works[0].(map[string]any)
	if w["kind"] != "primaryTurn" || w["foregroundExecutionId"] == "" || w["startedAt"] == nil {
		t.Errorf("activeWork = %v", w)
	}
	routing := patch["inputRouting"].(map[string]any)
	if routing["mode"] != "enqueue" {
		t.Errorf("running routing = %v", routing)
	}
	sqn := patch["availability"].(map[string]any)["sendQueuedNow"].(map[string]any)
	if sqn["allowed"] != true {
		t.Errorf("sendQueuedNow = %v", sqn)
	}
}

// TestCompletedControlPatchShape covers the turn-end patch (已处理 state).
func TestCompletedControlPatchShape(t *testing.T) {
	frame := stateUpdatedFrame("sess_t", "completedSuccess", "sess_t:sub", 1)
	d, _ := json.Marshal(frame)
	var m map[string]any
	_ = json.Unmarshal(d, &m)
	payload := mustMap(t, mustMap(t, m, "frame"), "payload")
	dl := payload["deltas"].([]any)[0].(map[string]any)
	patch := dl["patch"].(map[string]any)

	control := patch["control"].(map[string]any)
	if control["phase"] != "completedSuccess" || control["canStop"] != false || control["stopState"] != "idle" {
		t.Errorf("completed control = %v", control)
	}
	if len(control["activeWorks"].([]any)) != 0 {
		t.Errorf("completed activeWorks = %v", control["activeWorks"])
	}
	routing := patch["inputRouting"].(map[string]any)
	if routing["mode"] != "startNow" {
		t.Errorf("completed routing = %v", routing)
	}
	sqn := patch["availability"].(map[string]any)["sendQueuedNow"].(map[string]any)
	if sqn["reasonCode"] != "sendQueuedNowRequiresRunning" {
		t.Errorf("sendQueuedNow = %v", sqn)
	}
}

// TestTaskIndexItemShape asserts the controller/tasks-index record layout the
// sidebar's live status reads.
func TestTaskIndexItemShape(t *testing.T) {
	item := taskIndexItem(zcode.Task{
		TaskID: "sess_t", Title: "T", WorkspacePath: "/ws",
		Status: "running", UpdatedAt: 1234,
	}, map[string]bool{"sess_t": true})
	b, _ := json.Marshal(item)
	var m map[string]any
	_ = json.Unmarshal(b, &m)

	for _, k := range []string{"address", "meta", "membership", "sourceAvailability", "liveStatus", "activity"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("task record missing %q", k)
		}
	}
	meta := m["meta"].(map[string]any)
	if meta["status"] != "running" || meta["thoughtLevel"] == "" || meta["provider"] == "" {
		t.Errorf("meta = %v", meta)
	}
	if m["liveStatus"] != "running" {
		t.Errorf("liveStatus = %v", m["liveStatus"])
	}
	activity := m["activity"].(map[string]any)
	if activity["phase"] != "running" {
		t.Errorf("activity = %v", activity)
	}
}

// TestTurnRowsCarryCommandChain asserts the extended row fields the official
// rows always include (executionKind, sourceCommandId chain, historyRoundCount).
func TestTurnRowsCarryCommandChain(t *testing.T) {
	tx := map[string]any{
		"messages": []any{
			map[string]any{
				"role": "user",
				"info": map[string]any{"time": map[string]any{"created": 1000.0, "completed": 2000.0}},
				"parts": []any{
					map[string]any{"type": "text", "text": "hi"},
				},
			},
		},
	}
	rows := messageRows(tx, "sess_t", func() int { return 1 })
	if len(rows) < 2 {
		t.Fatalf("rows = %d", len(rows))
	}
	hdr := rows[0].(map[string]any)
	if hdr["kind"] != "turnHeader" || hdr["executionKind"] != "agent" {
		t.Errorf("header = %v", hdr)
	}
	if hdr["sourceCommandId"] == "" || hdr["historyRoundCount"] == nil || hdr["endedAt"] == nil {
		t.Errorf("header fields = %v", hdr)
	}
	row := rows[1].(map[string]any)
	if row["kind"] != "userInput" || row["rootSourceCommandId"] == "" || row["origin"] != "realUser" {
		t.Errorf("userInput = %v", row)
	}
}
