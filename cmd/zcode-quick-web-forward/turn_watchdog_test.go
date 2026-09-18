package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWatchdogFactsProgressClearsStall(t *testing.T) {
	r := &officialRecovery{}
	r.initMaps()
	r.turnStallProbe = map[string]int{"s": 3}
	r.watchRevision = map[string]int64{"s": 7}
	r.watchTokens = map[string]int{"s": 100}
	if r.watchdogFactsProgress("s", 7, 100, 1_000) {
		t.Fatal("identical facts must not count as progress")
	}
	if !r.watchdogFactsProgress("s", 8, 100, 2_000) {
		t.Fatal("revision movement must count as progress")
	}
	if len(r.turnStallProbe) != 0 {
		t.Fatal("progress must clear stall probes")
	}
	if r.turnProgressMs("s") != 2_000 {
		t.Fatal("progress stamp not recorded")
	}
}

func TestStagedRetryConsumedOnce(t *testing.T) {
	r := &officialRecovery{}
	r.initMaps()
	r.setStagedRetry("s", "git pull first")
	text, ok := r.takeStagedRetry("s")
	if !ok || text != "git pull first" {
		t.Fatalf("first take must return the text, got %q ok=%v", text, ok)
	}
	if _, ok := r.takeStagedRetry("s"); ok {
		t.Fatal("staged retry must be consumed once")
	}
	r.setStagedRetry("s", "继续")
	if text, _ := r.takeStagedRetry("s"); text != "继续" {
		t.Fatalf("re-arming must replace the text, got %q", text)
	}
}

func TestIneffectiveKickRearmsLadder(t *testing.T) {
	r := &officialRecovery{}
	r.initMaps()
	now := time.Now().UnixMilli()
	r.noteTurnKick("s", now)
	kicks, since := r.turnKickState("s")
	if kicks != 1 || since < 0 || since > 1000 {
		t.Fatalf("fresh kick state wrong: kicks=%d since=%dms", kicks, since)
	}
	// A kick within cooldown is ignored by the tick; an INEFFECTIVE kick
	// (turn still running) must re-arm immediately.
	r.noteIneffectiveKick("s")
	kicks, since = r.turnKickState("s")
	if kicks != 1 {
		t.Fatalf("ineffective kick must not add to kick count, got %d", kicks)
	}
	if since < turnKickCooldownMs-1000 {
		t.Fatalf("ineffective kick must re-arm the cooldown, since=%dms", since)
	}
}

func TestInteractionWaitGatesEscalation(t *testing.T) {
	r := &officialRecovery{}
	r.initMaps()
	now := time.Now().UnixMilli()
	r.noteInteractionWait("s", now)
	if !r.interactionRecent("s", now) {
		t.Fatal("fresh interaction wait must gate")
	}
	if r.interactionRecent("s", now+11*60_000) {
		t.Fatal("stale interaction wait must stop gating")
	}
}

func tempJournalPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "staged-retry.json")
}

// The 2026-09-18 05:04 incident: headless tasks re-injected after a daemon
// restart hit the permission wall because the yolo mode never followed the
// text. The journal must carry the mode and the write-rebuild must not drop
// mode-only entries.
func TestStagedModeJournaledAndSurvivesRebuild(t *testing.T) {
	path := tempJournalPath(t)
	t.Setenv("ZQF_STAGED_JOURNAL", path)
	r := &officialRecovery{}
	r.initMaps()

	r.setStagedRetryMode("a", "yolo")
	r.setStagedRetry("a", "do the task")
	r.setStagedRetryMode("b", "build") // mode-only session (text consumed earlier)

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("journal missing: %v", err)
	}
	var in map[string]stagedJournalEntry
	if err := json.Unmarshal(b, &in); err != nil {
		t.Fatalf("journal unparseable: %v", err)
	}
	if in["a"].Mode != "yolo" || in["a"].Text != "do the task" {
		t.Fatalf("entry a wrong: %+v", in["a"])
	}
	if in["b"].Mode != "build" || in["b"].Text != "" {
		t.Fatalf("entry b wrong: %+v", in["b"])
	}

	// Consuming the text must keep the mode in the journal...
	if _, ok := r.takeStagedRetry("a"); !ok {
		t.Fatal("take must succeed")
	}
	b, _ = os.ReadFile(path)
	in = nil
	if err := json.Unmarshal(b, &in); err != nil {
		t.Fatalf("journal unparseable after take: %v", err)
	}
	if in["a"].Mode != "yolo" {
		t.Fatalf("mode lost after text consumption: %+v", in["a"])
	}

	// A turn that ended WITH output means the text was delivered: clearing
	// it must degrade the entry to mode-only, so a later boot re-arm cannot
	// re-send a finished task.
	r.clearStagedText("a")
	b, _ = os.ReadFile(path)
	in = nil
	if err := json.Unmarshal(b, &in); err != nil {
		t.Fatalf("journal unparseable after clear: %v", err)
	}
	if in["a"].Text != "" || in["a"].Mode != "yolo" {
		t.Fatalf("cleared entry must be mode-only, got %+v", in["a"])
	}

	// ...and a fresh boot must restore both modes (text only for unconsumed).
	r2 := &officialRecovery{}
	r2.initMaps()
	loadStagedJournal(r2)
	if got := r2.stagedMode("a"); got != "yolo" {
		t.Fatalf("mode a not restored, got %q", got)
	}
	if got := r2.stagedMode("b"); got != "build" {
		t.Fatalf("mode b not restored, got %q", got)
	}
	if _, ok := r2.takeStagedRetry("a"); ok {
		t.Fatal("cleared text must not re-arm")
	}
}

// A staged text must be replayed into a journal whose mode-only entry was
// written first: text+mode round-trip through write→load.
func TestStagedTextAfterModeOnlyRoundTrip(t *testing.T) {
	path := tempJournalPath(t)
	t.Setenv("ZQF_STAGED_JOURNAL", path)
	r := &officialRecovery{}
	r.initMaps()
	r.setStagedRetryMode("s", "yolo")
	journalOutboxText("s", "real task text")
	r2 := &officialRecovery{}
	r2.initMaps()
	loadStagedJournal(r2)
	text, ok := r2.takeStagedRetry("s")
	if !ok || text != "real task text" {
		t.Fatalf("staged text not restored: %q ok=%v", text, ok)
	}
	if got := r2.stagedMode("s"); got != "yolo" {
		t.Fatalf("mode not restored alongside text, got %q", got)
	}
}

// modelIOTailCut: only a quiet, recent, tool-call-bearing tail counts.
func TestModelIOTailCut(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "model-io-s.jsonl")
	mk := func(t *testing.T, line string, age time.Duration) {
		t.Helper()
		if err := os.WriteFile(path, []byte(line+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-age)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	tail := func(toolCalls int, startedAt string) string {
		b, _ := json.Marshal(map[string]any{
			"type": "model_io", "startedAt": startedAt,
			"response": map[string]any{"toolCalls": make([]any, toolCalls), "text": "x"},
		})
		return string(b)
	}
	staged := time.Now().Add(-10 * time.Minute).UnixMilli()

	if cut, _ := modelIOTailCut(path, staged); cut {
		t.Fatal("missing file must not count as cut")
	}

	// Clean final answer (no tool calls) — not a cut.
	mk(t, tail(0, time.Now().Add(-5*time.Minute).Format(time.RFC3339)), 2*time.Minute)
	if cut, _ := modelIOTailCut(path, staged); cut {
		t.Fatal("tail without tool calls is a finished turn")
	}

	// Tool calls + old start inside the turn window + quiet file → cut.
	mk(t, tail(2, time.Now().Add(-8*time.Minute).Format(time.RFC3339)), 2*time.Minute)
	if cut, _ := modelIOTailCut(path, staged); !cut {
		t.Fatal("unanswered tool calls in a quiet tail must count as cut")
	}

	// Tail started BEFORE the turn was staged → belongs to an older turn.
	mk(t, tail(2, time.Now().Add(-30*time.Minute).Format(time.RFC3339)), 2*time.Minute)
	if cut, _ := modelIOTailCut(path, staged); cut {
		t.Fatal("older-turn tail must not count")
	}

	// File written seconds ago → engine may still be live.
	mk(t, tail(2, time.Now().Add(-8*time.Minute).Format(time.RFC3339)), 0)
	if cut, _ := modelIOTailCut(path, staged); cut {
		t.Fatal("hot file must not count as cut")
	}

	// Unparseable garbage → not a cut.
	mk(t, strings.Repeat("junk", 100), 2*time.Minute)
	if cut, _ := modelIOTailCut(path, staged); cut {
		t.Fatal("unparseable tail must not count")
	}
}
