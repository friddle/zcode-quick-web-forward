package main

// Self-healing for the two ways a phone-submitted message could silently
// never run, and for the post-restart observation blind spot:
//
//   - BUG-001: turnRunning/completedAt live in memory; after a daemon restart
//     a mid-flight turn was invisible (no card, no spinner, lost completion
//     dots). recoverRecentTurns() re-observes rows for the most recent tasks
//     on every bridge-open.
//   - BUG-002: when a send goes missing — the host rejects it after its ~30s
//     admission pend, or the engine queue wedges after an abnormal turn end —
//     the message silently vanishes ("提交不成功"): we already early-acked the
//     page and nothing ever dispatches. The refresher detects queue-mirror
//     items still undelivered after the turn has ENDED and resubmits the
//     texts as fresh envelopes. While the turn is running a queued item is
//     normal followupMode=queue behavior and is never touched — an earlier
//     version stop-ed live turns here and killed healthy work.

import (
	"fmt"
	"time"

	"github.com/friddle/zcode-quick-web-forward/internal/relay"
	"github.com/friddle/zcode-quick-web-forward/internal/zcode"
)

// zcodeListTasksRecent returns up to limit recent task session ids.
func zcodeListTasksRecent(limit int) ([]string, error) {
	tasks, err := zcode.ListTasks("", "")
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, limit)
	for _, t := range tasks {
		if len(out) >= limit {
			break
		}
		out = append(out, t.TaskID)
	}
	return out, nil
}

// queueRescueAfter is how long a queue-mirror item may sit undelivered while
// the session is considered running before we conclude the host's turn lease
// is stale and force-release it.
const queueRescueAfter = 60 * time.Second

// rescueRecentTurns re-observes turn state for up to limit recent tasks.
// Called on bridge-open (the phone's own recovery moment): after a daemon
// restart the in-memory turnRunning map is empty, so running tasks had no
// card/spinner and completions went unrecorded until now.
func rescueRecentTurns(limit int) {
	rec := officialActiveRec()
	if rec == nil || limit <= 0 {
		return
	}
	officialState.mu.Lock()
	b := officialState.active
	officialState.mu.Unlock()
	if b == nil {
		return
	}
	tasks, err := zcodeListTasksRecent(limit)
	if err != nil {
		return
	}
	for _, sid := range tasks {
		rec.mu.Lock()
		running := rec.turnRunning[sid]
		_, subscribed := rec.subBySession[sid]
		rec.mu.Unlock()
		if running || subscribed {
			continue // already tracked / observed
		}
		sid := sid
		go func() {
			time.Sleep(time.Duration(len(sid)%7) * 30 * time.Millisecond) // spread the burst
			requestRecoverySnapshot(b, sid)
		}()
	}
}

// rescueStuckQueues walks the sessions the refresher believes are running and
// finds queue-mirror items stuck far past delivery. Only called from the
// refresher goroutine (officialRecovery.ensureTurnRefresher).
func rescueStuckQueues(b *officialHostBridge, sid string, now int64) {
	rec := b.rec
	rec.mu.Lock()
	items := rec.queuedSends[sid]
	if len(items) == 0 {
		rec.mu.Unlock()
		return
	}
	if rec.turnRunning[sid] {
		// A queued item while the turn is genuinely running is NORMAL
		// followupMode=queue behavior — it dispatches when the turn ends.
		// Stopping here would kill live work (this exact mistake cancelled a
		// healthy model request on 2026-09-14 14:16).
		rec.mu.Unlock()
		return
	}
	oldest := int64(0)
	for _, it := range items {
		if at, _ := it["admittedAt"].(int64); at > 0 && (oldest == 0 || at < oldest) {
			oldest = at
		}
	}
	last, _ := rec.lastQueueRescue[sid]
	if oldest == 0 || now-oldest < queueRescueAfter.Milliseconds() || now-last < 120_000 {
		rec.mu.Unlock()
		return
	}
	if rec.lastQueueRescue == nil {
		rec.lastQueueRescue = map[string]int64{}
	}
	rec.lastQueueRescue[sid] = now
	// Snapshot the texts under lock; resubmit outside it.
	texts := make([]string, 0, len(items))
	clients := make([]string, 0, len(items))
	for _, it := range items {
		t, _ := it["text"].(string)
		c, _ := it["clientId"].(string)
		texts = append(texts, t)
		clients = append(clients, c)
	}
	rec.mu.Unlock()

	// The turn has ENDED (turnRunning false) yet the message(s) never
	// dispatched — the engine queue wedged after an abnormal turn end. A
	// fresh sendText re-runs admission from a clean state. No stop needed:
	// nothing is running.
	fmt.Printf("zcode: recovery: queue for %s stuck %ds past admission while idle (%d items) — engine queue wedged, resubmitting\n",
		sid, (now-oldest)/1000, len(items))
	time.AfterFunc(2*time.Second, func() {
		officialResubmitQueued(sid, texts, clients)
	})
}

// officialResubmitQueued re-injects stuck queued texts as fresh sendText
// envelopes and clears the mirror.
func officialResubmitQueued(sid string, texts, clients []string) {
	for i, t := range texts {
		text := t
		client := clients[i]
		time.Sleep(time.Duration(i) * 800 * time.Millisecond)
		officialInjectCommand(sid, "sendText", map[string]any{"text": text}, client)
	}
	rec := officialActiveRec()
	if rec != nil {
		rec.mu.Lock()
		delete(rec.queuedSends, sid)
		rec.mu.Unlock()
	}
	fmt.Printf("zcode: recovery: resubmitted %d stuck queued send(s) for %s\n", len(texts), sid)
	if f := taskStatusNudge; f != nil {
		time.AfterFunc(300*time.Millisecond, f)
	}
}

// officialInjectCommand builds a fresh sendConversationCommandV4 envelope and
// forwards it to the official host exactly like a phone-issued call (fresh
// call id + commandId; the forward path applies its usual repairs).
func officialInjectCommand(sid, typ string, payload map[string]any, clientID string) {
	officialState.mu.Lock()
	b := officialState.active
	ws := officialState.workspace
	officialState.mu.Unlock()
	if b == nil {
		return
	}
	if clientID == "" {
		rec := b.rec
		rec.mu.Lock()
		clientID = rec.clientBySession[sid]
		rec.mu.Unlock()
	}
	if clientID == "" {
		clientID = "client-" + uuidNew()
	}
	env := map[string]any{
		"commandId": uuidNew(),
		"clientId":  clientID,
		"sessionId": sid,
		"type":      typ,
		"payload":   payload,
		"issuedAt":  time.Now().UnixMilli(),
	}
	arg := map[string]any{"workspacePath": ws, "envelope": env}
	c := &relay.ChannelCall{Kind: relay.KindPromise, ID: b.rec.mintID(), ChannelName: "zcode-agent", Name: "sendConversationCommandV4", Arg: arg}
	fmt.Printf("zcode: recovery: injecting %s for %s (rescue)\n", typ, sid)
	forwardCallToOfficialHost(c)
}
