package main

import "testing"

// The hand-written channel/projection layer lives on the with_self_implement
// branch; main is a pure relay<->official-host pipe. These tests cover what
// remains: the pairing-level payloads and the remembered-row buffer.

func TestWorkspaceListPayloadDedupsAndSkipsRemote(t *testing.T) {
	// The payload merges stored workspaces from ~/.zcode, so assert on the
	// requested ones (dedup, order, remote:/empty skip), not exact count.
	got := workspaceListPayload([]string{"/ws/a", "/ws/a", "remote:foo", "", "/ws/b"})
	var paths []string
	for _, w := range got {
		paths = append(paths, w.(map[string]any)["workspacePath"].(string))
	}
	seen := map[string]bool{}
	aIdx, bIdx := -1, -1
	for i, p := range paths {
		if seen[p] {
			t.Fatalf("duplicate workspace %q in %v", p, paths)
		}
		seen[p] = true
		if p == "/ws/a" {
			aIdx = i
		}
		if p == "/ws/b" {
			bIdx = i
		}
	}
	if aIdx == -1 || bIdx == -1 || aIdx > bIdx {
		t.Fatalf("want /ws/a before /ws/b (skipping remote:/empty), got %v", paths)
	}
	for _, w := range got {
		if w.(map[string]any)["connectionState"] != "connected" {
			t.Fatalf("workspace must advertise connected: %v", w)
		}
	}
}

func TestAppendRememberedCapsAndKeepsLiveTail(t *testing.T) {
	ps := &phoneSessions{}
	// 3000 rows before the boundary marker, marker, then live rows.
	for i := 0; i < 3000; i++ {
		ps.appendRemembered([]any{map[string]any{"rowId": i + 1, "kind": "assistantText"}})
	}
	ps.appendRemembered([]any{map[string]any{"rowId": 99999, "kind": "timelineMarker", "lane": "turnTailBoundary"}})
	for i := 0; i < 50; i++ {
		ps.appendRemembered([]any{map[string]any{"rowId": 100000 + i, "kind": "reasoning"}})
	}
	rows := ps.snapshotRows()
	if len(rows) > maxRememberedRows+50 {
		t.Fatalf("buffer not capped: %d rows", len(rows))
	}
	// The live tail after the marker must survive trimming.
	last := rows[len(rows)-1].(map[string]any)
	if last["rowId"].(int) != 100049 {
		t.Fatalf("live tail lost, last row %v", last["rowId"])
	}
	foundMarker := false
	for _, r := range rows {
		if m := r.(map[string]any); m["kind"] == "timelineMarker" {
			foundMarker = true
		}
	}
	if !foundMarker {
		t.Fatal("turnTailBoundary marker was trimmed")
	}
}

func TestRememberUpsertReplacesByRowID(t *testing.T) {
	ps := &phoneSessions{}
	ps.appendRemembered([]any{map[string]any{"rowId": 1, "kind": "toolCall", "status": "running"}})
	ps.rememberUpsert(map[string]any{"rowId": 1, "kind": "toolCall", "status": "success"})
	rows := ps.snapshotRows()
	if rows[0].(map[string]any)["status"] != "success" {
		t.Fatalf("upsert missed: %v", rows[0])
	}
}
