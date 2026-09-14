package main

import (
	"sync"
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
	var _ sync.Mutex = r.mu
}
