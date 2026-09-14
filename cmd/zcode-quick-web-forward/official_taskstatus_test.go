package main

import (
	"encoding/json"
	"testing"
	"time"
)

func taskStatusTestRec() *officialRecovery {
	r := &officialRecovery{}
	r.initMaps()
	return r
}

// A turn flipping running→ended must stamp completedAt and nudge the task
// list exactly once per transition (not on every repeated observation).
func TestRecordTurnRunningTransitions(t *testing.T) {
	r := taskStatusTestRec()
	nudges := make(chan struct{}, 8)
	taskStatusNudge = func() { nudges <- struct{}{} }
	defer func() { taskStatusNudge = nil }()

	r.mu.Lock()
	r.recordTurnRunning("sess_x", true) // idle → running
	r.mu.Unlock()
	select {
	case <-nudges:
	case <-time.After(time.Second):
		t.Fatal("no nudge on idle→running transition")
	}

	r.mu.Lock()
	r.recordTurnRunning("sess_x", true) // repeated running observation
	r.mu.Unlock()
	select {
	case <-nudges:
		t.Fatal("nudge fired without a transition")
	case <-time.After(350 * time.Millisecond):
	}

	r.mu.Lock()
	r.recordTurnRunning("sess_x", false) // running → ended
	r.mu.Unlock()
	select {
	case <-nudges:
	case <-time.After(time.Second):
		t.Fatal("no nudge on running→ended transition")
	}
	r.mu.Lock()
	done := r.completedAt["sess_x"]
	r.mu.Unlock()
	if done == 0 {
		t.Fatal("running→ended did not stamp completedAt")
	}
}

// The task list reads running state and the unread dot (completed after the
// last viewing) via officialTaskRuntime; opening the task clears the dot.
func TestOfficialTaskRuntimeUnreadFlow(t *testing.T) {
	rec := taskStatusTestRec()
	prev := officialState.active
	officialState.mu.Lock()
	officialState.active = &officialHostBridge{rec: rec}
	officialState.mu.Unlock()
	defer func() {
		officialState.mu.Lock()
		officialState.active = prev
		officialState.mu.Unlock()
	}()

	if running, unread := officialTaskRuntime("sess_y"); running || unread != 0 {
		t.Fatalf("fresh session reported running=%v unread=%d", running, unread)
	}

	rec.mu.Lock()
	rec.turnRunning["sess_y"] = true
	rec.mu.Unlock()
	if running, _ := officialTaskRuntime("sess_y"); !running {
		t.Fatal("turnRunning=true not surfaced")
	}

	rec.mu.Lock()
	rec.turnRunning["sess_y"] = false
	rec.completedAt["sess_y"] = 1000
	rec.mu.Unlock()
	if _, unread := officialTaskRuntime("sess_y"); unread != 1000 {
		t.Fatalf("unread=%d, want 1000", unread)
	}

	officialMarkTaskViewed("sess_y")
	if _, unread := officialTaskRuntime("sess_y"); unread != 0 {
		t.Fatalf("unread=%d after viewing, want 0", unread)
	}
}

// Guard against the nil-map panic class: a zero-value recovery must not blow
// up when the forward path writes it (the initMaps regression).
func TestRecoveryMapsInitialized(t *testing.T) {
	r := &officialRecovery{}
	r.initMaps()
	r.mu.Lock()
	r.seenCommand["k"] = 1
	r.completedAt["s"] = 2
	r.viewedAt["s"] = 3
	r.mu.Unlock()
	if r.seenCommand["k"] != 1 || r.completedAt["s"] != 2 || r.viewedAt["s"] != 3 {
		t.Fatal("map writes lost")
	}
}

// A queue item stuck past admission while the session is "running" must arm
// the rescue (lastQueueRescue stamped); a fresh item must not.
func TestStuckQueueRescueTrigger(t *testing.T) {
	rec := taskStatusTestRec()
	// 注意：不设 officialState.active —— rescue 内部的 inject 在无 active
	// bridge 时是 no-op，这里只验证触发判定逻辑。
	now := time.Now().UnixMilli()
	// 引擎真源闸门：status=running（host readSession 报告的活跃 turn）时
	// 队列条目是正常 followup 排队，绝不能触发自愈。
	rec.stashSnap("sess_z", json.RawMessage(`{"session":{"status":"running"}}`))
	rec.mu.Lock()
	rec.queuedSends["sess_z"] = []map[string]any{{
		"text":       "queued followup",
		"admittedAt": now - 120_000,
	}}
	rec.mu.Unlock()

	rescueStuckQueues(&officialHostBridge{rec: rec}, "sess_z", now)
	rec.mu.Lock()
	armed := rec.lastQueueRescue["sess_z"] != 0
	rec.mu.Unlock()
	if armed {
		t.Fatal("engine running must not arm the rescue")
	}

	// 引擎报告 idle（turn 已结束）而条目仍未投递 → 自愈介入。
	rec.stashSnap("sess_z", json.RawMessage(`{"session":{"status":"idle"}}`))
	rescueStuckQueues(&officialHostBridge{rec: rec}, "sess_z", now)
	rec.mu.Lock()
	stamped := rec.lastQueueRescue["sess_z"] != 0
	kept := len(rec.queuedSends["sess_z"])
	rec.mu.Unlock()
	if !stamped {
		t.Fatal("idle engine with a stuck queue did not arm the rescue")
	}
	if kept != 1 {
		t.Fatalf("mirror cleared too early (%d left) — resubmit happens in the 2s goroutine", kept)
	}

	// 未投递的新条目不触发。
	rec.stashSnap("sess_f", json.RawMessage(`{"session":{"status":"idle"}}`))
	rec.mu.Lock()
	rec.queuedSends["sess_f"] = []map[string]any{{
		"text":       "fresh message",
		"admittedAt": now - 5_000,
	}}
	rec.mu.Unlock()
	rescueStuckQueues(&officialHostBridge{rec: rec}, "sess_f", now)
	rec.mu.Lock()
	notStuck := rec.lastQueueRescue["sess_f"] == 0
	rec.mu.Unlock()
	if !notStuck {
		t.Fatal("fresh queue item wrongly armed the rescue")
	}
	// 让 2s 后的重投 goroutine 不影响其他用例（没有 active bridge 时 inject 是 no-op）
	// 让 6s 后的重投 goroutine 不影响其他用例（没有 active bridge 时 inject 是 no-op）
}

// The snapshot window must shrink by BYTE budget (huge analysis rows), keeping
// the newest rows, so a big task no longer freezes the phone on refresh.
func TestShrinkWindowToBudget(t *testing.T) {
	mk := func(id int, pad int) map[string]any {
		return map[string]any{"rowId": id, "text": make([]byte, pad)}
	}
	window := []any{mk(1, 10_000), mk(2, 10_000), mk(3, 10_000)}
	out, shrunk := shrinkWindowToBudget(window, 15_000)
	if !shrunk {
		t.Fatal("window clearly over budget was not shrunk")
	}
	if len(out) != 1 {
		t.Fatalf("kept %d rows, want 1 (only the newest fits)", len(out))
	}
	if out[0].(map[string]any)["rowId"] != 3 {
		t.Fatal("wrong row kept — newest rows must survive")
	}
	if _, shrunk := shrinkWindowToBudget(window, 1_000_000); shrunk {
		t.Fatal("window within budget was shrunk")
	}
}

// The usage meter must not flicker: once the engine reports a non-zero
// context usage, factsFor keeps serving it even after the readSession stash
// evicts the session.
func TestFactsForUsagePersistence(t *testing.T) {
	rec := taskStatusTestRec()
	rec.stashSnap("sess_u", json.RawMessage(`{"session":{"status":"idle"},"projection":{"contextUsed":12345,"contextWindow":128000}}`))
	f := rec.factsFor("sess_u")
	if f.ctxUsed != 12345 || f.ctxWindow != 128000 {
		t.Fatalf("usage not parsed: %d/%d", f.ctxUsed, f.ctxWindow)
	}
	// stash 逐出（模拟其它会话把 LRU 挤掉）
	rec.mu.Lock()
	delete(rec.snaps, "sess_u")
	rec.mu.Unlock()
	f = rec.factsFor("sess_u")
	if f.ctxUsed != 12345 || f.ctxWindow != 128000 {
		t.Fatalf("usage lost after eviction: %d/%d", f.ctxUsed, f.ctxWindow)
	}
	// 全新会话（从未上报）必须保持 0，不得编造
	if f2 := rec.factsFor("sess_never"); f2.ctxUsed != 0 || f2.ctxWindow != 0 {
		t.Fatalf("invented usage for unknown session: %d/%d", f2.ctxUsed, f2.ctxWindow)
	}
}

// officialAnyTurnRunning reads the engine-observed turn map; it must report
// running when any session is mid-turn (the bridge-open restart guard relies
// on this — the old phoneSessions.anyTurnRunning read a never-written map).
func TestOfficialAnyTurnRunning(t *testing.T) {
	rec := taskStatusTestRec()
	prev := officialState.active
	officialState.mu.Lock()
	officialState.active = &officialHostBridge{rec: rec}
	officialState.mu.Unlock()
	defer func() {
		officialState.mu.Lock()
		officialState.active = prev
		officialState.mu.Unlock()
	}()

	if officialAnyTurnRunning() {
		t.Fatal("no turns running, but reported running")
	}
	rec.mu.Lock()
	rec.turnRunning["sess_a"] = true
	rec.mu.Unlock()
	if !officialAnyTurnRunning() {
		t.Fatal("turn running but reported idle")
	}
}
