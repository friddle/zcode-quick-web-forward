package main

import (
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
	// Real progress retires the escalation ladder and the death spiral.
	r.turnStallProbe = map[string]int{}
	r.turnKickCount = map[string]int{}
	delete(r.resurrectCount, sid)
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
// can replay it after unwedging the runtime.
func (r *officialRecovery) setStagedRetry(sid, text string) {
	r.mu.Lock()
	if r.stagedRetry == nil {
		r.stagedRetry = map[string]string{}
	}
	if r.stagedRetryTaken == nil {
		r.stagedRetryTaken = map[string]bool{}
	}
	r.stagedRetry[sid] = text
	delete(r.stagedRetryTaken, sid)
	r.mu.Unlock()
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
