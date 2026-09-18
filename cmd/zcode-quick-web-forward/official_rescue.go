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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	// bridge-open fires on every task view open and every page reconnect —
	// without a rate limit a reconnect storm would multiply the burst.
	rec.mu.Lock()
	now := time.Now().UnixMilli()
	if now-rec.lastRecentRescue < 60_000 {
		rec.mu.Unlock()
		return
	}
	rec.lastRecentRescue = now
	rec.mu.Unlock()
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
	rec.mu.Unlock()

	// 引擎真源闸门：readSession（host 每 ~10s 轮询、stash 在此）里的
	// session.status 才是「turn 是否还在跑」的权威信号。我们自己的
	// turnRunning 推断（乐观标记 + turnHeader 行）两个方向都会错——
	// 错误的自愈曾取消健康的模型请求（14:16 事故）。引擎报告 running
	// 时绝不介入；队列条目只是在正常排队。
	f := parseSessionFacts(rec.snapFor(sid))
	switch f.status {
	case "running", "in-progress", "active":
		return
	}

	rec.mu.Lock()
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
	// Resubmit only on a FRESH observation. The mirror retires when a rows
	// fetch shows the matching userInput row; if our last rows view is stale,
	// the engine may have already taken the message and resubmitting now
	// would run it twice (both copies landed in the engine DB on 09-14
	// 19:48). Ask for rows instead and let a later refresher tick decide.
	if now-rec.lastRowsAt[sid] > 15_000 {
		rec.mu.Unlock()
		requestConversationRows(b, sid)
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
	// nothing is running. BUT: a turn that died with AiSdkModelAdapterError
	// can leave the agent RUNTIME with a zombie foreground turn — the
	// projection reports idle, yet every new send is enqueued behind the
	// zombie and silently dropped after the host's ~30s admission pend
	// (seen 2026-09-15 morning: hours of submissions vanished). Kick the
	// runtime with a stop before resubmitting: on a truly idle engine the
	// stop is a harmless no-op, and the engine-truth gate above guarantees
	// no live work is cancelled.
	fmt.Printf("zcode: recovery: queue for %s stuck %ds past admission while idle (%d items) — kicking runtime and resubmitting\n",
		sid, (now-oldest)/1000, len(items))
	time.AfterFunc(time.Second, func() {
		officialInjectCommand(sid, "stop", map[string]any{}, "")
	})
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

// dispatchQueuedAfterTurn fires ~4s after a turn ends: the engine is
// supposed to auto-dispatch messages that were queued behind that turn, but
// in headless mode that drain intermittently never happens (the message then
// sits invisible until the 60s patrol). When the turn really ended, the
// engine is idle, and mirror items are still undelivered on a fresh rows
// view, inject them now. When the engine DID start the next turn itself,
// session.status reports running and this aborts — no double dispatch.
func (r *officialRecovery) dispatchQueuedAfterTurn(sid string) {
	r.mu.Lock()
	items := r.queuedSends[sid]
	if len(items) == 0 {
		r.mu.Unlock()
		return
	}
	now := time.Now().UnixMilli()
	if now-r.lastQueueRescue[sid] < 120_000 {
		r.mu.Unlock()
		return
	}
	// Same freshness rule as rescueStuckQueues: act only on a rows view that
	// had the chance to retire delivered items.
	if now-r.lastRowsAt[sid] > 15_000 {
		officialState.mu.Lock()
		b := officialState.active
		officialState.mu.Unlock()
		if b != nil {
			r.mu.Unlock()
			requestConversationRows(b, sid)
			return
		}
	}
	texts := make([]string, 0, len(items))
	clients := make([]string, 0, len(items))
	for _, it := range items {
		t, _ := it["text"].(string)
		c, _ := it["clientId"].(string)
		texts = append(texts, t)
		clients = append(clients, c)
	}
	r.mu.Unlock()

	f := parseSessionFacts(r.snapFor(sid))
	switch f.status {
	case "running", "in-progress", "active":
		return // engine took the queued message itself — a new turn is live
	}
	r.mu.Lock()
	if r.lastQueueRescue == nil {
		r.lastQueueRescue = map[string]int64{}
	}
	r.lastQueueRescue[sid] = now
	r.mu.Unlock()
	// Without an active bridge the inject inside is a no-op; the mirror clear
	// still happens, matching the rescue path's behavior.
	fmt.Printf("zcode: recovery: turn ended with %d undelivered queued send(s) for %s — dispatching\n", len(texts), sid)
	officialResubmitQueued(sid, texts, clients)
}

// pageClientStateFile persists the phone page's channel clientId across
// daemon restarts. The page keeps the same id in localStorage
// ("zcode-v4-client-id:v1") for the lifetime of the browser, so the daemon
// can complete the host handshake on its behalf after a restart — without
// it, a silently reconnected page (no reload → no bridge-open → no
// hello/initialize) stays handshake-rejected forever.
const pageClientStateFile = "/root/data/zqf-page-client.json"

// pageClientStatePath resolves the page-client persistence file. The
// historical constant was CP-specific (/root/data, root-owned); everywhere
// else the file lives next to the relay state under the user cache dir.
func pageClientStatePath() string {
	if v := os.Getenv("ZQF_PAGE_CLIENT"); v != "" {
		return v
	}
	if cache, err := os.UserCacheDir(); err == nil {
		return filepath.Join(cache, "zcode-quick-web-forward", "page-client.json")
	}
	return pageClientStateFile
}

func loadPersistedPageClient() {
	b, err := os.ReadFile(pageClientStatePath())
	if err != nil {
		return
	}
	var st struct {
		ClientID string `json:"clientId"`
	}
	if json.Unmarshal(b, &st) != nil || st.ClientID == "" {
		return
	}
	officialState.mu.Lock()
	officialState.persistedClientID = st.ClientID
	officialState.mu.Unlock()
	fmt.Printf("zcode: recovery: restored page clientId %s\n", st.ClientID)
}

func persistPageClient(clientID string) {
	if clientID == "" {
		return
	}
	officialState.mu.Lock()
	known := officialState.persistedClientID
	if known == clientID {
		officialState.mu.Unlock()
		return
	}
	officialState.persistedClientID = clientID
	officialState.mu.Unlock()
	st, _ := json.Marshal(map[string]any{"clientId": clientID, "savedAt": time.Now().UnixMilli()})
	if err := os.WriteFile(pageClientStatePath(), st, 0644); err != nil {
		fmt.Printf("zcode: recovery: persisting page clientId failed: %v\n", err)
		return
	}
	fmt.Printf("zcode: recovery: persisted page clientId %s\n", clientID)
}

// bootstrapHostHandshake completes the host-side connection handshake on
// behalf of the page. The host gates every state-changing call behind
// helloConversationV4 + initializeConversationV4 (fault.connection.
// handshakeRequired otherwise); the page only runs that dance on a
// workspace-bridge-open, so after a daemon/host restart with a silently
// reconnected page (no reload → no bridge-open) every page write was
// rejected forever while the daemon's early-acks hid it from the user. The
// daemon is the service-port client: speaking the handshake here is the
// transport's job. Uses the page's CURRENT clientId (learned from its own
// envelopes, or the persisted one — the page keeps it in localStorage, so
// it is stable across reloads and restarts) so later page commands pass
// the clientMismatch check.
func bootstrapHostHandshake(b *officialHostBridge) {
	if b == nil || !b.h.Alive() {
		return
	}
	r := b.rec
	r.mu.Lock()
	now := time.Now().UnixMilli()
	if now-r.lastHandshakeTry < 3000 {
		r.mu.Unlock()
		return
	}
	// A real page owns the connection while its handshake is fresh: it
	// registers ITS client, and a daemon-side hello for another client only
	// poisons the registration (hello ping-pong, both sides then fail with
	// clientMismatch). Bootstrap exclusively for the truly headless case —
	// no page has handshaken recently.
	if now-r.lastPageHandshakeAt < 5*60_000 {
		r.mu.Unlock()
		return
	}
	r.lastHandshakeTry = now
	clientID := ""
	// Prefer the LATEST page client (persisted on every page-issued command)
	// over any session-era id: after a page reload the host expects the new
	// client, and bootstrapping the handshake with a stale one only sets the
	// connection up for clientMismatch on every later command.
	officialState.mu.Lock()
	clientID = officialState.persistedClientID
	officialState.mu.Unlock()
	if clientID == "" {
		for _, c := range r.clientBySession {
			if c != "" {
				clientID = c
				break
			}
		}
	}
	r.mu.Unlock()
	if clientID == "" {
		return // page identity unknown — nothing safe to register as
	}
	hello := &relay.ChannelCall{Kind: relay.KindPromise, ID: r.mintID(),
		ChannelName: "zcode-agent", Name: "helloConversationV4", Arg: map[string]any{}}
	init := &relay.ChannelCall{Kind: relay.KindPromise, ID: r.mintID(),
		ChannelName: "zcode-agent", Name: "initializeConversationV4",
		Arg: map[string]any{
			"kind":            "clientHello",
			"protocolVersion": 3,
			"clientId":        clientID,
			"clientKind":      "desktop",
			"appVersion":      "unknown",
			"capabilities":    map[string]any{"workspaceHookReviewUi": true},
		}}
	// Same-port frames are processed in arrival order, so hello flips
	// helloDone before initialize checks it — no reply round-trip needed.
	// The synthetic replies must not reach the page (unknown call ids).
	r.mu.Lock()
	r.suppressAck[hello.ID] = true
	r.suppressAck[init.ID] = true
	r.mu.Unlock()
	fmt.Printf("zcode: recovery: bootstrapping host handshake for page client %s\n", clientID)
	forwardCallToOfficialHost(hello)
	forwardCallToOfficialHost(init)
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
		// The host binds commands to the client of the LATEST handshake —
		// after any page reload that is a NEWER client than the one that
		// created the session. Prefer the freshly persisted id; the
		// session-era id is only a fallback, otherwise every injected send
		// after a page reload dies with fault.command.clientMismatch.
		officialState.mu.Lock()
		clientID = officialState.persistedClientID
		officialState.mu.Unlock()
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
	// Optimistically mark the turn running so the turn-stall watchdog
	// supervises it: turnRunning otherwise only updates on rows fetches, and
	// a headless send with nobody watching the task was invisible to the
	// watchdog — exactly the turns that wedge silently.
	if typ == "sendText" {
		// recordTurnRunning needs the optimistic running mark; setStagedRetry
		// takes rec.mu ITSELF — Go mutexes are not reentrant, so holding the
		// wrapper lock across it self-deadlocked every sendText inject (the
		// stuck goroutine held rec.mu forever and every dump showed the
		// holder disguised as a waiter). Sequential acquisitions, no nesting.
		text, _ := payload["text"].(string)
		b.rec.mu.Lock()
		b.rec.recordTurnRunning(sid, true)
		b.rec.mu.Unlock()
		// Stage the text for the watchdog's dead-turn resurrect.
		b.rec.setStagedRetry(sid, text)
	}
	// The interceptor's sendText bookkeeping (optimistic row / queue mirror)
	// must not fire for our own inject: a rescue resubmit of an undelivered
	// item would otherwise be re-queued behind a stale optimistic running
	// mark and duplicate the very item it is rescuing.
	b.rec.mu.Lock()
	if b.rec.selfInjected == nil {
		b.rec.selfInjected = map[int]bool{}
	}
	b.rec.selfInjected[c.ID] = true
	b.rec.mu.Unlock()
	fmt.Printf("zcode: recovery: injecting %s for %s (rescue)\n", typ, sid)
	forwardCallToOfficialHost(c)
	b.rec.mu.Lock()
	delete(b.rec.selfInjected, c.ID)
	b.rec.mu.Unlock()
}
