package main

import (
	"strings"
	"time"
)

// Watchdog bookkeeping on officialRecovery. All stamps are unix ms.

// noteTurnProgress records an observable sign of turn life: a live
// conversation frame, a turn-state transition, or revision/token movement in
// the authoritative facts.
func (r *officialRecovery) noteTurnProgress(sid string, now int64) {
	r.mu.Lock()
	r.noteTurnProgressLocked(sid, now)
	r.mu.Unlock()
}

func (r *officialRecovery) noteTurnProgressLocked(sid string, now int64) {
	if r.turnProgressAt == nil {
		r.turnProgressAt = map[string]int64{}
	}
	r.turnProgressAt[sid] = now
	// Progress retires the escalation ladder. NOTE: the resurrect-spiral
	// counter does NOT reset here — see noteRealOutputLocked.
	r.turnStallProbe = map[string]int{}
	r.turnKickCount = map[string]int{}
}

// noteRealOutputLocked records REAL turn output evidence (a live engine
// frame, projection facts movement, or a rows-observed turn state). The
// resurrect/clear gates must not count the optimistic inject stamp here:
// officialInjectCommand stamps turnProgressAt itself, which otherwise made
// hadOutput true 2s after every injection and silently disarmed the whole
// dead-turn path (2026-09-18 07:52: the staged text was cleared and the
// mid-tool check armed before the engine had even started the turn).
func (r *officialRecovery) noteRealOutputLocked(sid string, now int64) {
	if r.realOutputAt == nil {
		r.realOutputAt = map[string]int64{}
	}
	r.realOutputAt[sid] = now
	// REAL evidence alone resets the death spiral: the optimistic inject
	// stamp must not, or every re-inject resets its own resurrect counter
	// and the 4-attempt engine-kill cap never arms (2026-09-19 10:2x
	// feedback-system: the ladder looped retry->wedge forever).
	delete(r.resurrectCount, sid)
	// A turn that provably ran also earns the staged text a fresh inject
	// budget: only never-productive texts should hit the circuit breaker.
	if r.stagedTries != nil {
		delete(r.stagedTries, sid)
	}
}

// sessionWorkspaceOf returns the workspace a daemon-created session belongs
// to ("" when unknown).
func (r *officialRecovery) sessionWorkspaceOf(sid string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sessionWorkspace[sid]
}

// noteRealOutput is the locked wrapper for use outside r.mu.
func (r *officialRecovery) noteRealOutput(sid string, now int64) {
	r.mu.Lock()
	r.noteRealOutputLocked(sid, now)
	r.mu.Unlock()
}

// realOutputSinceLocked is the r.mu-held variant used inside transitions.
func (r *officialRecovery) realOutputSinceLocked(sid string, ts int64) bool {
	at := r.realOutputAt[sid]
	return at > 0 && at >= ts
}

// realOutputSince reports whether real output was observed at or after ts.
func (r *officialRecovery) realOutputSince(sid string, ts int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	at := r.realOutputAt[sid]
	return at > 0 && at >= ts
}

func (r *officialRecovery) turnProgressMs(sid string) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if at, ok := r.turnProgressAt[sid]; ok {
		return at
	}
	return 0
}

// runningSessionsSnapshot lists sessions currently believed to be running.
func (r *officialRecovery) runningSessionsSnapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.turnRunning))
	for sid, running := range r.turnRunning {
		if running {
			out = append(out, sid)
		}
	}
	return out
}

// clearTurnStall resets stall bookkeeping once the engine stops reporting
// running for the session.
func (r *officialRecovery) clearTurnStall(sid string, now int64) {
	r.mu.Lock()
	delete(r.turnStallProbe, sid)
	delete(r.turnKickCount, sid)
	r.turnProgressAt[sid] = now
	r.mu.Unlock()
}

// watchdogFactsProgress compares authoritative facts between ticks; any
// revision or token movement counts as progress even when no frames reached
// the daemon (page closed, deltas unobserved).
func (r *officialRecovery) watchdogFactsProgress(sid string, revision int64, tokens int, now int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	moved := false
	if r.watchRevision == nil {
		r.watchRevision = map[string]int64{}
	}
	if r.watchTokens == nil {
		r.watchTokens = map[string]int{}
	}
	if prev, ok := r.watchRevision[sid]; !ok || prev != revision {
		moved = true
	}
	if prev, ok := r.watchTokens[sid]; !ok || prev != tokens {
		moved = true
	}
	r.watchRevision[sid] = revision
	r.watchTokens[sid] = tokens
	if moved {
		if r.turnProgressAt == nil {
			r.turnProgressAt = map[string]int64{}
		}
		r.turnProgressAt[sid] = now
		r.noteRealOutputLocked(sid, now)
		delete(r.turnStallProbe, sid)
	}
	return moved
}

// noteStallProbe counts consecutive stalled observations; the first probe
// also triggers a rows refresh (handled by the caller), later ones arm the
// ladder. Returns the probe count after increment.
func (r *officialRecovery) noteStallProbe(sid string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.turnStallProbe == nil {
		r.turnStallProbe = map[string]int{}
	}
	r.turnStallProbe[sid]++
	return r.turnStallProbe[sid]
}

// stallProbeCount reads the consecutive-probe counter without mutating it.
func (r *officialRecovery) stallProbeCount(sid string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.turnStallProbe[sid]
}

func (r *officialRecovery) noteTurnKick(sid string, now int64) {
	r.mu.Lock()
	if r.turnKickAt == nil {
		r.turnKickAt = map[string]int64{}
	}
	if r.turnKickCount == nil {
		r.turnKickCount = map[string]int{}
	}
	r.turnKickAt[sid] = now
	r.turnKickCount[sid]++
	r.mu.Unlock()
}

// turnKickState returns (kicks since last progress, ms since last kick).
func (r *officialRecovery) turnKickState(sid string) (int, int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now().UnixMilli()
	last := r.turnKickAt[sid]
	if last == 0 {
		last = now - turnKickCooldownMs // no kick yet — cooldown already elapsed
	}
	return r.turnKickCount[sid], now - last
}

func (r *officialRecovery) noteIneffectiveKick(sid string) {
	r.mu.Lock()
	if r.turnKickCount == nil {
		r.turnKickCount = map[string]int{}
	}
	// An ineffective kick immediately re-arms the ladder: the next tick sees
	// the cooldown elapsed and escalates.
	r.turnKickAt[sid] = time.Now().UnixMilli() - turnKickCooldownMs
	r.mu.Unlock()
}

// noteInteractionWait remembers that a permission/interaction wait was seen;
// interactionRecent gates all escalation while a human may still be looking
// at the request (keep gating for 10 minutes after the last sighting — a
// stale dialog on a disconnected phone must not wedge the session forever,
// but instant cancellation would be worse).
func (r *officialRecovery) noteInteractionWait(sid string, now int64) {
	r.mu.Lock()
	if r.interactionAt == nil {
		r.interactionAt = map[string]int64{}
	}
	r.interactionAt[sid] = now
	r.turnStallProbe = map[string]int{}
	r.mu.Unlock()
}

func (r *officialRecovery) interactionRecent(sid string, now int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	at := r.interactionAt[sid]
	return at != 0 && now-at < 10*60_000
}

// setStagedRetry captures the most recent sendText text so a watchdog retry
// can replay it after unwedging the runtime. The stage timestamp drives the
// resurrect gate (output since staging = the turn actually ran; do not
// duplicate it) and the text is mirrored to the journal so a daemon restart
// can re-arm the task — the 2026-09-17 restart dropped it from memory and
// the tasks sat idle for ten hours.
func (r *officialRecovery) setStagedRetry(sid, text string) {
	now := time.Now().UnixMilli()
	r.mu.Lock()
	if r.stagedRetry == nil {
		r.stagedRetry = map[string]string{}
	}
	if r.stagedRetryAt == nil {
		r.stagedRetryAt = map[string]int64{}
	}
	if r.stagedRetryTaken == nil {
		r.stagedRetryTaken = map[string]bool{}
	}
	r.stagedRetry[sid] = text
	r.stagedRetryAt[sid] = now
	delete(r.stagedRetryTaken, sid)
	// A brand-new explicit submission is not a retry: give it a fresh
	// inject budget regardless of what an older staged text burned.
	if r.stagedTries == nil {
		r.stagedTries = map[string]int{}
	}
	delete(r.stagedTries, sid)
	writeStagedJournalLocked(r)
	r.mu.Unlock()
}

// setStagedRetryMode remembers the collaboration mode a session's tasks must
// run under (build/edit/plan/yolo) and journals it next to the staged text.
// The engine does NOT restore a manually switched mode after a respawn: a
// headless task re-injected into a fresh engine hit the permission wall again
// (every tool call raised an approval that fails with no page to answer it —
// the 2026-09-18 05:04 "Permission request failed" stall). Re-apply the mode
// before every staged re-inject.
func (r *officialRecovery) setStagedRetryMode(sid, mode string) {
	mode = strings.TrimSpace(mode)
	if sid == "" || mode == "" {
		return
	}
	r.mu.Lock()
	if r.stagedModeBy == nil {
		r.stagedModeBy = map[string]string{}
	}
	r.stagedModeBy[sid] = mode
	writeStagedJournalLocked(r)
	r.mu.Unlock()
}

// stagedMode returns the journaled collaboration mode for sid ("" when none).
func (r *officialRecovery) stagedMode(sid string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stagedModeBy[sid]
}

// clearStagedText drops a consumed/finished task's staged text while keeping
// the mode and the daemon-injected marker (stagedRetryAt drives the mid-tool
// completion check). Caller holds r.mu.
func (r *officialRecovery) clearStagedText(sid string) {
	if r.stagedRetry == nil {
		return
	}
	delete(r.stagedRetry, sid)
	delete(r.stagedRetryTaken, sid)
	writeStagedJournalLocked(r)
}

// stagedInjectMaxTries caps how many times the SAME staged text may be
// re-injected (persisted in the journal, so the cap survives restarts —
// unlike the in-memory resurrectCount, which the boot re-arm ignored). A
// text that never produced real output after this many injections is
// poisoned: it wedges or crashes every engine that loads it, and each retry
// adds one more startNow row to the engine queue while flooding attached
// pages with resynthesized snapshots.
const stagedInjectMaxTries = 4

// noteStagedInject counts one more inject attempt for sid's staged text and
// reports whether the inject may proceed. Real output resets the counter
// (noteRealOutputLocked) — a turn that provably ran earns a fresh budget for
// any LATER interruption retry; only never-productive texts hit the wall.
func (r *officialRecovery) noteStagedInject(sid string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stagedTries == nil {
		r.stagedTries = map[string]int{}
	}
	r.stagedTries[sid]++
	if r.stagedTries[sid] > stagedInjectMaxTries {
		delete(r.stagedRetry, sid)
		delete(r.stagedRetryTaken, sid)
		writeStagedJournalLocked(r)
		return false
	}
	writeStagedJournalLocked(r)
	return true
}

// takeStagedRetry pops the staged retry text, once per stall episode.
func (r *officialRecovery) takeStagedRetry(sid string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	text, ok := r.stagedRetry[sid]
	if !ok || text == "" || r.stagedRetryTaken[sid] {
		return "", false
	}
	r.stagedRetryTaken[sid] = true
	writeStagedJournalLocked(r)
	return text, true
}

func (r *officialRecovery) noteEngineKill(sid string, now int64) {
	r.mu.Lock()
	r.engineKilledAt = now
	r.turnKickCount = map[string]int{}
	r.turnStallProbe = map[string]int{}
	r.mu.Unlock()
}

// noteResurrect counts consecutive silent-turn-death resurrections for a
// session; resets when real progress (noteTurnProgress) is observed.
func (r *officialRecovery) noteResurrect(sid string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.resurrectCount == nil {
		r.resurrectCount = map[string]int{}
	}
	r.resurrectCount[sid]++
	return r.resurrectCount[sid]
}

func (r *officialRecovery) engineKillAllowed(now int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return now-r.engineKilledAt > engineKillCooldown
}
