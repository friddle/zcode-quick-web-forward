package main

// Turn-stall watchdog: automatic recovery for wedged agent runtimes.
//
// The official stack supervises engine PROCESS LIFETIME (the host's engine
// process manager keys one child per workspaceKey, respawns when
// child.killed, fires runtimeRestarted) but nothing detects an ALIVE yet
// wedged runtime — a turn that reports running while no frames, no revision
// and no token movement happen for minutes (observed after cancellations and
// mid-turn daemon restarts; manual recovery was stop → engine restart →
// resubmit). This watchdog automates exactly that ladder, using only
// official mechanisms for the recovery steps:
//
//	probe      requestConversationRows — fresh authoritative facts
//	stop       sendConversationCommandV4 type=stop (the page's own 停止生成)
//	kill       SIGKILL the engine child → the HOST's process manager does the
//	           standard respawn (getClient sees child.killed), nothing custom
//	retry      resubmit the captured last user input with the standard
//	           heldQueueDisposition=clearQueueAndSend
//
// Hard rails: a session with pendingInteractions is waiting on a HUMAN and
// is never touched; every escalation is rate-limited; kick→verify→escalate
// only after the stop demonstrably failed to end the turn.

import (
	"fmt"
	"os"
	"runtime"
	"time"
)

const (
	turnStallSoftMs     = 150_000       // no progress for this long while engine says running
	turnStallProbeMax   = 3             // consecutive stalled probes before acting
	turnNoFactsProbeMax = 20            // no-facts probes (≈10 min) before last-resort escalation
	turnKickCooldownMs  = 120_000       // min gap between stop-kicks per session
	turnKickVerifyAfter = 45 * 1000     // wait before judging a stop-kick effective
	turnKickMax         = 2             // ineffective stop-kicks before killing the engine
	engineKillCooldown  = 600_000       // min gap between engine kills (global)
	turnRetryDelay      = 6 * 1000      // pause between turn-end and input resubmit
	watchdogTick        = 30 * time.Second
	canaryProbeEvery    = time.Second  // canary sample interval
	canaryFailSamples   = 15           // consecutive failed samples before exit (one full window)
)

// startTurnWatchdog launches the stall patrol for this host bridge. Runs one
// goroutine per daemon process.
func startTurnWatchdog(b *officialHostBridge) {
	if b == nil {
		return
	}
	if os.Getenv("ZQF_TURN_WATCHDOG") == "0" {
		fmt.Println("zcode: turn watchdog disabled (ZQF_TURN_WATCHDOG=0)")
		return
	}
	rec := b.rec
	rec.mu.Lock()
	if rec.watchdogStarted {
		rec.mu.Unlock()
		return
	}
	rec.watchdogStarted = true
	rec.mu.Unlock()
	loadStagedJournal(rec)
	go rearmStagedOnBoot(b)
	go func() {
		for {
			time.Sleep(watchdogTick)
			if !b.h.Alive() {
				continue
			}
			turnWatchdogTick(b)
		}
	}()
	fmt.Println("zcode: turn watchdog started (stall>2.5m → stop → engine kill → retry, staged journal re-arm)")
}

// turnWatchdogTick scans every session the engine believes is running.
func turnWatchdogTick(b *officialHostBridge) {
	rec := b.rec
	now := time.Now().UnixMilli()
	for _, sid := range rec.runningSessionsSnapshot() {
		facts := parseSessionFacts(rec.snapFor(sid))
		// Unknown facts: the host only stashes readSession snapshots for
		// tasks the page has OPEN — a headless send was invisible here and
		// wedged silently. Fetch rows once per probe cycle instead: the
		// response drives recordTurnRunning (correct running state) and
		// surfaces any hidden permission interactions.
		if facts.status == "" {
			// Headless sessions never get page-open readSession snapshots —
			// absent facts here usually mean "page closed", NOT "wedged".
			// The old blind stop after 3 probes cancelled healthy in-flight
			// turns wholesale (the 2026-09-18 04:5x TURN_CANCELLED cluster,
			// and yesterday's 16:47/17:05 deaths — "v4 session stopped").
			// Keep fetching rows (facts arrive as soon as the host can serve
			// them) and escalate only after a LONG dead-silent window.
			p := rec.noteStallProbe(sid)
			if p == 1 || p%2 == 1 {
				fmt.Printf("zcode: watchdog: %s optimistic running but no facts — fetching rows (probe %d)\n", shortSid(sid), p)
				requestConversationRows(b, sid)
			}
			if p > turnNoFactsProbeMax {
				rec.handleNoFactsStall(b, sid, now)
			}
			continue
		}
		if facts.status != "running" && facts.status != "in-progress" && facts.status != "active" {
			// Engine says the turn ended; reconcile the optimistic running
			// mark (otherwise the session stays in the scan forever).
			weStarted := false
			rec.mu.Lock()
			if rec.turnRunning[sid] {
				rec.recordTurnRunning(sid, false)
				weStarted = true
			}
			rec.mu.Unlock()
			rec.clearTurnStall(sid, now)
			// weStarted means OUR injected send never produced a completed
			// response — the turn died silently (observed pattern: status
			// flips to idle with the assistant row left unfinished). The
			// recordTurnRunning(false) transition above already fired the
			// staged-input retry; the retry counter inside caps runaway
			// loops with an engine kill.
			_ = weStarted
			continue
		}
		// A pending interaction means the turn is waiting on a HUMAN —
		// cancelling it would be wrong. Only clear stale bookkeeping.
		if facts.interactions > 0 {
			rec.noteInteractionWait(sid, now)
			continue
		}
		// Progress = revision/token movement observed between ticks. The
		// facts comparison catches engines that stream nothing while the
		// page is closed (no live frames, no rows replies).
		if rec.watchdogFactsProgress(sid, facts.revision, facts.tokens, now) {
			continue
		}
		progressAge := now - rec.turnProgressMs(sid)
		if progressAge < turnStallSoftMs {
			continue
		}
		probes := rec.noteStallProbe(sid)
		if probes == 1 {
			// First confirmation: pull fresh rows so the next tick judges
			// on current facts, and say why we are watching.
			fmt.Printf("zcode: watchdog: %s running with no progress for %ds — probing rows\n", shortSid(sid), progressAge/1000)
			requestConversationRows(b, sid)
			continue
		}
		if probes < turnStallProbeMax {
			continue
		}
		kicks, sinceKick := rec.turnKickState(sid)
		if sinceKick < turnKickCooldownMs {
			continue
		}
		if kicks >= turnKickMax {
			killWedgedEngine(b, sid)
			continue
		}
		// Layer 2: the standard cancel, identical to the page's 停止生成.
		rec.noteTurnKick(sid, now)
		fmt.Printf("zcode: watchdog: %s stalled %ds, %d prior kicks — sending stop\n", shortSid(sid), progressAge/1000, kicks)
		officialInjectCommand(sid, "stop", map[string]any{}, "")
		time.AfterFunc(time.Duration(turnKickVerifyAfter)*time.Millisecond, func() {
			verifyStopKickedTurn(b, sid)
		})
	}
}

// verifyStopKickedTurn judges a stop-kick: if the turn is genuinely over,
// resubmit the captured user input (修复后重试); if the engine STILL reports
// running, the kick counted as ineffective and the ladder moves toward the
// engine kill on a later tick.
func verifyStopKickedTurn(b *officialHostBridge, sid string) {
	if !b.h.Alive() {
		return
	}
	rec := b.rec
	facts := parseSessionFacts(rec.snapFor(sid))
	if facts.status == "" {
		return // no facts yet — let the tick decide
	}
	if facts.status != "running" && facts.status != "in-progress" && facts.status != "active" {
		fmt.Printf("zcode: watchdog: %s stop-kick cleared the wedged turn\n", shortSid(sid))
		maybeRetryStagedInput(b, sid, "after stop-kick")
		return
	}
	rec.noteIneffectiveKick(sid)
	fmt.Printf("zcode: watchdog: %s still running after stop-kick — escalation armed\n", shortSid(sid))
}

// handleNoFactsStall unwedges a session whose optimistic running mark never
// met any facts: the standard stop clears whatever the engine still holds.
func (r *officialRecovery) handleNoFactsStall(b *officialHostBridge, sid string, now int64) {
	kicks, sinceKick := r.turnKickState(sid)
	if sinceKick < turnKickCooldownMs {
		return
	}
	if kicks >= turnKickMax {
		killWedgedEngine(b, sid)
		return
	}
	r.noteTurnKick(sid, now)
	fmt.Printf("zcode: watchdog: %s no facts after %d kicks — sending blind stop\n", shortSid(sid), kicks)
	officialInjectCommand(sid, "stop", map[string]any{}, "")
}

// maybeRetryStagedInput resubmits the captured last user input once the turn
// actually ended — the 修复后重试 half of the ladder. Only text the daemon
// itself injected is retried, capped at 4 consecutive dead turns before the
// engine itself gets killed (host respawn clears a sick runtime).
func maybeRetryStagedInput(b *officialHostBridge, sid, why string) {
	rec := b.rec
	if n := rec.noteResurrect(sid); n > 4 {
		fmt.Printf("zcode: watchdog: %s died %d times in a row — killing engine for a clean respawn\n", shortSid(sid), n-1)
		killWedgedEngine(b, sid)
		return
	}
	text, ok := rec.takeStagedRetry(sid)
	if !ok || text == "" {
		return
	}
	fmt.Printf("zcode: watchdog: %s retrying interrupted input (%d chars) — %s\n", shortSid(sid), len(text), why)
	go func() {
		time.Sleep(time.Duration(turnRetryDelay) * time.Millisecond)
		officialInjectCommand(sid, "sendText", map[string]any{"text": text, "heldQueueDisposition": "clearQueueAndSend"}, "")
	}()
}

// killWedgedEngine kills the engine child process for the session's
// workspace. Recovery from here on is entirely the official host's: its
// process manager sees child.killed and respawns a fresh engine on the next
// getClient, firing runtimeRestarted so attached clients reconnect.
func killWedgedEngine(b *officialHostBridge, sid string) {
	officialState.mu.Lock()
	ws := officialState.workspace
	officialState.mu.Unlock()
	if ws == "" {
		return
	}
	rec := b.rec
	if !rec.engineKillAllowed(time.Now().UnixMilli()) {
		return
	}
	pids := findEnginePIDs(ws)
	if len(pids) == 0 {
		// No engine child for the workspace: the turn cannot be running —
		// the tree died with a previous daemon exit (the daemon parents the
		// host, the host parents the engine). Reconcile the optimistic mark
		// instead of dead-ending: the running→false transition fires the
		// staged-input resurrect, whose sendText makes the host load the
		// persisted session back into a fresh engine (observed working).
		fmt.Printf("zcode: watchdog: %s engine already gone for %s — reconciling optimistic running\n", shortSid(sid), ws)
		rec.mu.Lock()
		running := rec.turnRunning[sid]
		if running {
			rec.recordTurnRunning(sid, false)
		}
		rec.mu.Unlock()
		if !running {
			// Nothing left to reconcile; drop the stall bookkeeping so the
			// session exits the scan.
			rec.clearTurnStall(sid, time.Now().UnixMilli())
		}
		return
	}
	fmt.Printf("zcode: watchdog: %s killing wedged engine pid(s) %v (workspace %s) — host will respawn\n", shortSid(sid), pids, ws)
	for _, pid := range pids {
		_ = killPID(pid)
	}
	rec.noteEngineKill(sid, time.Now().UnixMilli())
	// The respawn is the host's job; once facts flow again and the turn is
	// gone, the staged input retries through the normal path.
	time.AfterFunc(90*time.Second, func() {
		if !b.h.Alive() {
			return
		}
		facts := parseSessionFacts(rec.snapFor(sid))
		if facts.status == "" || facts.status == "running" {
			fmt.Printf("zcode: watchdog: %s engine not back yet after kill — waiting\n", shortSid(sid))
			return
		}
		maybeRetryStagedInput(b, sid, "after engine respawn")
	})
}

// startMutexCanary is the daemon-level panic mechanism: a wedged event loop
// or a self-deadlock (the phaseForSession class of bug) makes every further
// request hang forever. The canary samples the hot locks once a second; a
// real deadlock is declared only when TryLock fails AND the lock's churn
// counter is frozen for a full window — then all goroutine stacks are dumped
// next to the log and the daemon exits nonzero for the supervisor.
//
// Churn matters because TryLock alone also fails on a CONTENDED lock: in Go
// starvation mode (waiters queued >1ms) the mutex is handed waiter→waiter
// continuously and TryLock never succeeds even though the daemon is healthy.
// The 2026-09-17/18 false exits all dumped stacks with no owner at all —
// five-plus queued waiters, zero deadlock. Frozen churn is the difference.
func startMutexCanary() {
	go func() {
		streak := 0
		hot := ""
		for {
			time.Sleep(canaryProbeEvery)
			name, ok := probeHotLocks()
			churn := daemonLockChurn.Load()
			if ok {
				streak = 0
				hot = ""
				continue
			}
			time.Sleep(canaryProbeEvery)
			if daemonLockChurn.Load() != churn {
				// Locks are being handed around — contention, not deadlock.
				streak = 0
				hot = ""
				continue
			}
			if streak == 0 {
				hot = name
				fmt.Printf("zcode: canary: %s held and churn frozen — watching (fails %d/%d)\n", name, streak+1, canaryFailSamples)
			}
			streak++
			if streak < canaryFailSamples {
				continue
			}
			lockHolderSites.Range(func(k, v any) bool {
				fmt.Printf("zcode: canary: lock %p last acquired at %s\n", k, v)
				return true
			})
			dump := fmt.Sprintf("/tmp/zqf-canary-%d.log", time.Now().Unix())
			_ = os.WriteFile(dump, stackDumpAll(), 0644)
			fmt.Printf("zcode: canary: %s deadlocked for a full %d-sample window — dumping %s and exiting for supervisor restart\n", hot, canaryFailSamples, dump)
			os.Exit(1)
		}
	}()
}

// probeHotLocks tries the daemon's hottest locks without blocking. Returns
// the lock name and false when one is held — the deadlock shape (the
// phaseForSession self-deadlock class).
func probeHotLocks() (string, bool) {
	if !officialState.mu.TryLock() {
		return "officialState.mu", false
	}
	officialState.mu.Unlock()
	if rec := officialActiveRec(); rec != nil {
		if !rec.mu.TryLock() {
			return "rec.mu", false
		}
		rec.mu.Unlock()
	}
	return "", true
}

func stackDumpAll() []byte {
	buf := make([]byte, 4<<20)
	n := runtime.Stack(buf, true)
	return buf[:n]
}

func shortSid(sid string) string {
	if len(sid) > 18 {
		return sid[:18] + "…"
	}
	return sid
}
