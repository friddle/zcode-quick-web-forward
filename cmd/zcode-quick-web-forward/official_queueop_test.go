package main

import (
	"testing"
)

// queueFixture builds a recovery with two mirrored queue items for sid, the
// shape the phone's queue chips are rendered from (sourceCommandId doubles as
// the queue_ id suffix the engine uses).
func queueFixture(t *testing.T) (*officialRecovery, string) {
	t.Helper()
	r := &officialRecovery{queuedSends: map[string][]map[string]any{}}
	sid := "sess_test"
	mk := func(cmd, text string) map[string]any {
		return map[string]any{
			"sourceCommandId": cmd,
			"queueItemId":     "queue_" + cmd,
			"text":            text,
			"order":           map[string]any{"admissionSeq": 1, "queuePosition": 0},
		}
	}
	r.queuedSends[sid] = []map[string]any{mk("cmd-a", "first"), mk("cmd-b", "second")}
	return r, sid
}

func texts(items []map[string]any) []string {
	out := []string{}
	for _, it := range items {
		out = append(out, it["text"].(string))
	}
	return out
}

func TestApplyQueueOpDelete(t *testing.T) {
	r, sid := queueFixture(t)
	r.applyQueueOp(sid, "deleteQueueItem", map[string]any{"queueItemId": "queue_cmd-a"})
	got := texts(r.queuedSends[sid])
	if len(got) != 1 || got[0] != "second" {
		t.Fatalf("delete left %v, want [second]", got)
	}
	if p := r.queuedSends[sid][0]["order"].(map[string]any)["queuePosition"].(int); p != 0 {
		t.Fatalf("queuePosition not renumbered: %v", p)
	}
}

func TestApplyQueueOpSendNowDrops(t *testing.T) {
	r, sid := queueFixture(t)
	r.applyQueueOp(sid, "sendQueuedNow", map[string]any{"queueItemId": "queue_cmd-b"})
	got := texts(r.queuedSends[sid])
	if len(got) != 1 || got[0] != "first" {
		t.Fatalf("sendQueuedNow left %v, want [first]", got)
	}
}

func TestApplyQueueOpEditRetexts(t *testing.T) {
	r, sid := queueFixture(t)
	r.applyQueueOp(sid, "editQueueItem", map[string]any{"queueItemId": "queue_cmd-b", "newText": "rewritten"})
	got := texts(r.queuedSends[sid])
	if got[1] != "rewritten" {
		t.Fatalf("edit produced %v, want rewritten tail", got)
	}
}

func TestApplyQueueOpReorder(t *testing.T) {
	r, sid := queueFixture(t)
	// move cmd-b BEFORE cmd-a (the payload shape observed on the wire)
	r.applyQueueOp(sid, "reorderQueueItem", map[string]any{
		"queueItemId": "queue_cmd-b", "beforeQueueItemId": "queue_cmd-a",
	})
	got := texts(r.queuedSends[sid])
	if len(got) != 2 || got[0] != "second" || got[1] != "first" {
		t.Fatalf("reorder produced %v, want [second first]", got)
	}
	for i, it := range r.queuedSends[sid] {
		if p := it["order"].(map[string]any)["queuePosition"].(int); p != i {
			t.Fatalf("queuePosition[%d]=%d after reorder", i, p)
		}
	}
}

func TestApplyQueueOpReorderToTail(t *testing.T) {
	r, sid := queueFixture(t)
	r.applyQueueOp(sid, "reorderQueueItem", map[string]any{
		"queueItemId": "queue_cmd-a", "beforeQueueItemId": nil,
	})
	got := texts(r.queuedSends[sid])
	if len(got) != 2 || got[0] != "second" || got[1] != "first" {
		t.Fatalf("reorder-to-tail produced %v, want [second first]", got)
	}
}

func TestApplyQueueOpUnknownItemIsNoop(t *testing.T) {
	r, sid := queueFixture(t)
	r.applyQueueOp(sid, "deleteQueueItem", map[string]any{"queueItemId": "queue_cmd-zz"})
	if len(r.queuedSends[sid]) != 2 {
		t.Fatalf("unknown queueItemId mutated the mirror")
	}
}
