package main

// Self-healing for the two ways a phone-submitted message could silently
// never run, and for the post-restart observation blind spot:
//
//   - BUG-001: turnRunning/completedAt live in memory; after a daemon restart
//     a mid-flight turn was invisible (no card, no spinner, lost completion
//     dots). recoverRecentTurns() re-observes rows for the most recent tasks
//     on every bridge-open.
//   - BUG-002: when a turn ends abnormally (e.g. AiSdkModelAdapterError) the
//     host's projection can keep the turnHeader "running" (endedAt None)
//     forever; every later sendText for the session is then queued by the
//     host and NEVER dispatched, while we already early-acked it — the
//     message silently vanishes ("提交不成功"). The refresher detects a queue
//     mirror that stayed undelivered too long, force-releases the stale turn
//     with a stop, and resubmits the queued texts as fresh envelopes.

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
	running := rec.turnRunning[sid]
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

	if running {
		fmt.Printf("zcode: recovery: queue for %s stuck %ds past admission while running (%d items) — stale host turn lease suspected, force-releasing\n",
			sid, (now-oldest)/1000, len(items))

		// 1) Force-release the stale running turn: a bare stop (we strip
		// expectedForegroundExecutionId on the forward path) ends it host-side,
		// which unblocks the host's queue dispatch.
		officialInjectCommand(sid, "stop", map[string]any{}, "")
		// 2) Give the stop a moment, then resubmit the queued texts as fresh
		// envelopes (new commandIds — the originals were answered by our early
		// ack, and the host never dispatched them).
		time.AfterFunc(6*time.Second, func() {
			officialResubmitQueued(sid, texts, clients)
		})
		return
	}
	// Not running but the engine still holds the message(s): a turn that
	// ended abnormally wedged the engine's queue — nothing will dispatch it.
	// A fresh sendText re-runs the whole admission path from a clean state.
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
