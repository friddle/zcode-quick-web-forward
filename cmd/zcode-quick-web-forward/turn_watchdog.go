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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

// injectStagedWithMode (re)applies the session's journaled collaboration
// mode BEFORE the text lands — and WAITS for the engine to answer the mode
// call. A fresh engine does not restore a manually switched mode, and a
// text-only re-inject raises a permission approval on the first tool call
// that fails headless ("Permission request failed") — the mode call racing
// the text used to lose that race (2026-09-18 07:25: the turns started in
// build mode and hit the wall again). On timeout the text still goes out
// (the mid-tool check below remains the backstop).
func injectStagedWithMode(b *officialHostBridge, sid, text string) {
	if mode := b.rec.stagedMode(sid); mode != "" {
		fmt.Printf("zcode: watchdog: %s re-applying collaboration mode %s before staged text\n", shortSid(sid), mode)
		officialInjectMode(b.rec, sid, mode)
		if !waitForModeAck(b.rec, sid, 10*time.Second) {
			fmt.Printf("zcode: watchdog: %s mode %s not confirmed in 10s — sending text anyway\n", shortSid(sid), mode)
		}
	}
	officialInjectCommand(sid, "sendText", map[string]any{
		"text":                 text,
		"heldQueueDisposition": "clearQueueAndSend",
	}, "")
}

// officialInjectMode sends the mode switch and arms the per-session ack
// signal consumed by waitForModeAck.
func officialInjectMode(rec *officialRecovery, sid, mode string) {
	ch := make(chan string, 1)
	rec.mu.Lock()
	if rec.modeAck == nil {
		rec.modeAck = map[string]chan string{}
	}
	rec.modeAck[sid] = ch
	rec.mu.Unlock()
	officialInjectCommand(sid, "switchCollaborationMode", map[string]any{"mode": mode}, "")
}

// waitForModeAck blocks until the mode call is answered (any ack — success or
// a retryable fault counts as "the engine saw it"; the retry ladder owns
// redelivery), the facts report the mode, or the timeout elapses.
func waitForModeAck(rec *officialRecovery, sid string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		rec.mu.Lock()
		ch := rec.modeAck[sid]
		rec.mu.Unlock()
		if ch != nil {
			select {
			case <-ch:
				rec.mu.Lock()
				delete(rec.modeAck, sid)
				rec.mu.Unlock()
				return true
			default:
			}
		}
		if m := parseSessionFacts(rec.snapFor(sid)).mode; m == rec.stagedMode(sid) && m != "" {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(500 * time.Millisecond)
	}
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
		injectStagedWithMode(b, sid, text)
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

// headlessTurnCheckAfter is how long after a daemon-injected turn ends that
// the mid-tool cut detector inspects the engine's model-io journal.
const headlessTurnCheckAfter = 75 * time.Second

// scheduleHeadlessTurnCheck covers the supervision hole that left the
// 2026-09-18 shopify/kube tasks silent for two hours: a headless turn ENDED
// (engine projection idle) while its last model response still had UNTOUCHED
// tool calls — the engine cut the turn mid-tool after a permission-request
// failure storm. The rows view only proves "not running", not "finished the
// work", and once running flips false the session left every watchdog. For
// daemon-injected tasks we CAN do better: the engine journals every model
// round-trip to ~/.zcode/cli/rollout/model-io-<sid>.jsonl, so a tail entry
// with unanswered toolCalls is a high-confidence mid-turn cut. Re-nudge the
// task (capped by the resurrect ladder; real progress resets it).
func scheduleHeadlessTurnCheck(b *officialHostBridge, sid string) {
	if b == nil || sid == "" {
		return
	}
	time.AfterFunc(headlessTurnCheckAfter, func() {
		if !b.h.Alive() {
			return
		}
		verifyHeadlessTurnCompletion(b, sid)
	})
}

// verifyHeadlessTurnCompletion resurrects a daemon-injected task whose turn
// was cut mid-tool. No-op unless ALL of these hold at fire time: no turn is
// running now, the engine's model-io tail is a model_io entry with toolCalls
// that belongs to the staged turn (started after it was staged), and the file
// has been quiet for a minute (no newer model activity).
func verifyHeadlessTurnCompletion(b *officialHostBridge, sid string) {
	rec := b.rec
	rec.mu.Lock()
	if rec.turnRunning[sid] {
		rec.mu.Unlock()
		return // a fresh turn is live and supervised
	}
	stagedAt := rec.stagedRetryAt[sid]
	daemonInjected := stagedAt > 0
	rec.mu.Unlock()
	if !daemonInjected {
		return // not ours — a human-driven session ends whenever the engine says so
	}
	cut, at := modelIOTailCut(modelIOPath(sid), stagedAt)
	if !cut {
		return
	}
	fmt.Printf("zcode: watchdog: %s turn ended %ds ago with its last model response still holding tool calls — mid-tool cut\n",
		shortSid(sid), time.Now().UnixMilli()-at/1_000_000)
	// Continuation text: the original staged text if still unconsumed, else a
	// generic resume prompt. Re-stage whichever we send so this turn is
	// itself journaled and supervised.
	text, ok := rec.takeStagedRetry(sid)
	if !ok || text == "" {
		text = "继续：上一回合在工具执行中被中断了。请从中断处继续完成原任务，不要重复已完成的部分。"
	}
	if n := rec.noteResurrect(sid); n > 4 {
		fmt.Printf("zcode: watchdog: %s cut %d turns in a row — killing engine for a clean respawn\n", shortSid(sid), n-1)
		rec.setStagedRetry(sid, text)
		killWedgedEngine(b, sid)
		return
	}
	rec.setStagedRetry(sid, text)
	injectStagedWithMode(b, sid, text)
}

// modelIOPath resolves the engine's model-io journal for a session. The
// engine (zcode.cjs) writes one rollout file per session next to its CLI
// state; missing files simply disable the mid-tool cut check.
func modelIOPath(sid string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" || sid == "" {
		return ""
	}
	return filepath.Join(home, ".zcode", "cli", "rollout", "model-io-"+sid+".jsonl")
}

// modelIOTailCut reads the engine's model-io journal and reports whether its
// final entry is a model response that still holds unanswered tool calls,
// staged no earlier than stagedAt-5s and quiet for ≥60s.
// Returns (cut, fileModTimeNs). Missing/unparseable files are "not cut" —
// this check may only ADD resurrections, never fabricate them.
func modelIOTailCut(path string, stagedAt int64) (bool, int64) {
	if path == "" {
		return false, 0
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Size() == 0 {
		return false, 0
	}
	if time.Since(fi.ModTime()) < 60*time.Second {
		return false, 0 // engine still writing — a turn may be live
	}
	f, err := os.Open(path)
	if err != nil {
		return false, 0
	}
	defer f.Close()
	// Read the final line. Lines carry the full model request body and can
	// reach megabytes — a small window regularly started mid-line and the
	// JSON parse silently bailed, disabling the whole check. Back up 4MB and
	// only parse a line whose BOTH ends are visible.
	read := int64(4 << 20)
	if fi.Size() < read {
		read = fi.Size()
	}
	buf := make([]byte, read)
	if _, err := f.ReadAt(buf, fi.Size()-read); err != nil {
		return false, 0
	}
	trimmed := strings.TrimRight(string(buf), "\n")
	var last string
	if idx := strings.LastIndexByte(trimmed, '\n'); idx >= 0 {
		last = strings.TrimSpace(trimmed[idx+1:])
	} else if read == fi.Size() {
		last = strings.TrimSpace(trimmed) // single-line file, fully visible
	} else {
		return false, 0 // last line exceeds the window — cannot see its start
	}
	if last == "" {
		return false, 0
	}
	var rec struct {
		Type      string `json:"type"`
		StartedAt string `json:"startedAt"`
		Response  struct {
			ToolCalls []any `json:"toolCalls"`
		} `json:"response"`
	}
	if json.Unmarshal([]byte(last), &rec) != nil {
		return false, 0
	}
	if rec.Type != "model_io" || len(rec.Response.ToolCalls) == 0 {
		return false, 0 // clean final answer — the turn finished its loop
	}
	started, err := time.Parse(time.RFC3339, rec.StartedAt)
	if err != nil {
		return false, 0
	}
	if started.UnixMilli() < stagedAt-5_000 {
		return false, 0 // tail belongs to an OLDER turn, not the staged one
	}
	return true, fi.ModTime().UnixNano()
}

// maybeAutoApprove resolves pending permission interactions for headless
// sessions: no page listener + a daemon-injected task (staged marker) means
// nobody else can ever click 允许 — the engine either fails the request
// ("Permission request failed" denials starved the 2026-09-18 tasks) or
// blocks on it forever. Answer with the interaction's own allow option, the
// same card click the page would produce. Never fires when a page is
// bridged (the human decides) and ZQF_HEADLESS_APPROVE=0 disables the whole
// path. Caller runs in the rows-response flow.
func (r *officialRecovery) maybeAutoApprove(b *officialHostBridge, sid string, rowsRes map[string]any) {
	if os.Getenv("ZQF_HEADLESS_APPROVE") == "0" {
		return
	}
	if len(r.listeners()) > 0 {
		return // a page is connected — the human decides
	}
	r.mu.Lock()
	injected := r.stagedRetryAt[sid] > 0 || r.stagedModeBy[sid] != ""
	dead := r.deadInteractions
	r.mu.Unlock()
	if !injected {
		return
	}
	now := time.Now().Unix()
	for _, row := range fullRowsOf(rowsRes) {
		m, ok := row.(map[string]any)
		if !ok || m["status"] != "pendingApproval" {
			continue
		}
		iid, _ := m["approvalInteractionId"].(string)
		if iid == "" || dead[iid] {
			continue
		}
		r.mu.Lock()
		seen := r.autoApproved[iid]
		if !seen {
			if r.autoApproved == nil {
				r.autoApproved = map[string]bool{}
			}
			r.autoApproved[iid] = true
		}
		r.mu.Unlock()
		if seen {
			continue
		}
		// Rate limit across interactions: one approval per 3s max.
		if now-r.lastKick < 3 {
			continue
		}
		r.mu.Lock()
		r.lastKick = now
		r.mu.Unlock()
		tool, _ := m["toolName"].(string)
		fmt.Printf("zcode: watchdog: %s headless auto-approve permission %s (%s)\n", shortSid(sid), iid, tool)
		go func(iid string) {
			officialInjectCommand(sid, "resolveInteraction", map[string]any{
				"interactionId": iid,
				"answer":        map[string]any{"optionId": "allow_once"},
			}, "")
		}(iid)
	}
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
