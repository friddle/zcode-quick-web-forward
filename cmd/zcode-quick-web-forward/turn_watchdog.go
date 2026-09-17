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
	turnKickCooldownMs  = 120_000       // min gap between stop-kicks per session
	turnKickVerifyAfter = 45 * 1000     // wait before judging a stop-kick effective
	turnKickMax         = 2             // ineffective stop-kicks before killing the engine
	engineKillCooldown  = 600_000       // min gap between engine kills (global)
	turnRetryDelay      = 6 * 1000      // pause between turn-end and input resubmit
	watchdogTick        = 30 * time.Second
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
	go func() {
		for {
			time.Sleep(watchdogTick)
			if !b.h.Alive() {
				continue
			}
			turnWatchdogTick(b)
		}
	}()
	fmt.Println("zcode: turn watchdog started (stall>2.5m → stop → engine kill → retry)")
}

// turnWatchdogTick scans every session the engine believes is running.
func turnWatchdogTick(b *officialHostBridge) {
	rec := b.rec
	now := time.Now().UnixMilli()
	for _, sid := range rec.runningSessionsSnapshot() {
		facts := parseSessionFacts(rec.snapFor(sid))
		// Unknown facts: no authoritative signal — observe, never act.
		if facts.status == "" {
			continue
		}
		if facts.status != "running" && facts.status != "in-progress" && facts.status != "active" {
			// Engine says the turn ended; if a retry was staged for a wedge
			// that has now cleared, fire it.
			rec.clearTurnStall(sid, now)
			maybeRetryStagedInput(b, sid, "turn ended on its own")
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

// maybeRetryStagedInput resubmits the captured last user input once the turn
// actually ended — the 修复后重试 half of the ladder. Only text the daemon
// itself observed being sent is retried, exactly once per stall episode.
func maybeRetryStagedInput(b *officialHostBridge, sid, why string) {
	rec := b.rec
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
		fmt.Printf("zcode: watchdog: %s engine kill due but no engine process found for %s\n", shortSid(sid), ws)
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
// request hang forever. The canary probes the two hot locks; on failure it
// dumps all goroutine stacks next to the log and exits nonzero so the
// supervisor restarts the daemon — exit-fast with a post-mortem instead of a
// silent hang.
func startMutexCanary() {
	go func() {
		for {
			time.Sleep(15 * time.Second)
			if probeHotLocks() {
				continue
			}
			dump := fmt.Sprintf("/tmp/zqf-canary-%d.log", time.Now().Unix())
			_ = os.WriteFile(dump, stackDumpAll(), 0644)
			fmt.Printf("zcode: canary: hot lock held >15s — dumping %s and exiting for supervisor restart\n", dump)
			os.Exit(1)
		}
	}()
}

// probeHotLocks tries the daemon's hottest locks without blocking; false
// means one stayed held for a whole canary interval — the deadlock shape
// (the phaseForSession self-deadlock class).
func probeHotLocks() bool {
	if !officialState.mu.TryLock() {
		return false
	}
	officialState.mu.Unlock()
	if rec := officialActiveRec(); rec != nil {
		if !rec.mu.TryLock() {
			return false
		}
		rec.mu.Unlock()
	}
	return true
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
