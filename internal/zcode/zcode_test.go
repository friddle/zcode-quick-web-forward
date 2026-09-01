package zcode

import (
	"os"
	"testing"
)

// TestReadsRealState is a smoke test that runs against a real ZCode install
// (HOME/.zcode). It's skipped when no tasks-index.sqlite exists, so it works
// both locally and in CI-free environments.
func TestReadsRealState(t *testing.T) {
	if _, err := os.Stat(Home() + "/v2/tasks-index.sqlite"); err != nil {
		t.Skip("no real ZCode install, skipping")
	}
	tasks, err := ListTasks("", "")
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	t.Logf("tasks=%d", len(tasks))
	ws, err := Workspaces()
	if err != nil {
		t.Fatalf("Workspaces: %v", err)
	}
	t.Logf("workspaces=%d: %+v", len(ws), ws)
	prov := Providers()
	t.Logf("providers=%d", len(prov))
	ids, err := ListTaskIDs("pinned")
	if err != nil {
		t.Fatalf("ListTaskIDs: %v", err)
	}
	t.Logf("pinned ids=%v", ids)
}
