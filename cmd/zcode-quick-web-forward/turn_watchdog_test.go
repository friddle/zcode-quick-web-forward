package main

import (
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
