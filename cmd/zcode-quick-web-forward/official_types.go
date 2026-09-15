// Official-host integration (the DEFAULT): when
// ~/.zcode/host-official/shim.mjs exists, phone channel traffic is forwarded
// to the DESKTOP APP's official web-remote host (running under Node via
// official-host/shim.mjs) instead of the hand-rolled channel handlers. The
// host spawns the engine itself (ZCODE_AGENT_SERVER_COMMAND), so tasks,
// uploads, automations, file service and the v4 projection are the official
// implementation end to end.

package main

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	"github.com/friddle/zcode-quick-web-forward/internal/officialhost"
	"github.com/friddle/zcode-quick-web-forward/internal/relay"
)

type officialHostBridge struct {
	h      *officialhost.Host
	engine *relay.BridgeEngine
	sender *relaySender // routes to the phone's latest pending reply
	mu     sync.Mutex
	// pendingOut holds host->phone bytes that arrived before the phone
	// opened its workspace bridge (no rpc-frame identity yet — the framed
	// send would silently drop them). Flushed on bridge-open.
	pendingOut [][]byte
	// answered records promise-call ids the host replied to, so the bridge
	// can fall back to the built-in handlers for calls the host ignores.
	answered map[int]bool
	// svcPort is the CURRENT emulated service-port id. Each phone bridge-open
	// re-attaches with a FRESH attachmentId — the host rejects (or ignores)
	// a re-attach of an already-seen/disposed id, which silently killed the
	// pipe on the second pairing (page reload = stuck at Paired. Loading…).
	svcPort   string
	attachSeq int
	// ready flips when the host logs "local services ready, all channels
	// registered" — its workspace initialization (initializeWorkspace) is
	// async and channel messages that arrive earlier hit an empty registry,
	// where resolveWorkspaceKey(undefined) is an uncaughtException that kills
	// the host. Phone frames are buffered until then.
	ready     atomic.Bool
	pendingIn [][]byte
	// rec is the conversation-recovery synthesizer (see officialRecovery).
	rec *officialRecovery
}

// officialRecovery synthesizes the conversation topic frames (snapshot/deltas)
// the phone page's v4 store needs to render a conversation. The official host
// accepts our subscribeConversationV4 (rpc ack ok, mode=snapshot) but never
// pushes the topic frames — on the desktop those flow through the main
// process's relay bridge, which our headless pipe does not implement. We
// recover by calling zcode-session.readSession ourselves after each
// subscribe/resync (and after every sendText, so replies render) and wrapping
// the result into the frame shape the client expects, delivered as an event
// to the page's onDynamicConversationFrame listener.
type officialRecovery struct {
	mu               sync.Mutex
	listenID         int            // EventListen id for onDynamicConversationFrame
	pendingSub       map[int]string // subscribe/resync call id -> sessionId
	pendingRead      map[int]string // synthetic readSession call id -> sessionId
	priorListenIDs   []int
	subBySession     map[string]string           // sessionId -> subscriptionId (from acks)
	epochBySess      map[string]string           // sessionId -> logEpoch (from subscribe acks)
	nextID           int                         // synthetic promise-call id counter
	wireOrdinal      int                         // logicalFrameOrdinal of synthesized wire frames
	lastID           int                         // id of the most recently minted synthetic call
	pendingRows      map[int]string              // synthetic rowsRange call id -> sessionId
	lastRowsJSON     map[string]string           // sessionId -> last emitted rows JSON (dedup)
	resyncPend       map[string]bool             // sessionId -> resyncConversationV4 awaiting its recovery frame
	snapSeqBy        map[string]int              // sessionId -> last snapshot seq (must advance per emission)
	pendingTask      map[int]string              // call id -> "op|taskId" for zcode-task list mutations
	queuedSends      map[string][]map[string]any // sessionId -> pending queue items (mid-turn sends)
	turnRunning      map[string]bool             // sessionId -> last observed turnHeader.state == running
	admSeq           int                         // admissionSeq counter for synthesized queue items
	lastQEmitted     map[string]int              // sessionId -> queue size at last emitted snapshot
	termListens      []*relay.ChannelCall        // cached terminal.onDynamic* listens (forwarded after create)
	pendingCreate    map[int]bool                // terminal.create call ids awaiting their id
	lastTermID       string                      // most recently created terminal id
	snaps            map[string]json.RawMessage
	snapsOrder       []string
	pendingResolve   map[int][2]string          // sendConversationCommandV4 call id -> {sessionId, interactionId}
	deadInteractions map[string]bool            // interactionIds the engine reported as no-pending (stale rows)
	lastKick         int64                      // unix ts of last pendingApproval-triggered refresh (rate limit)
	refresherStarted bool                       // turn-running periodic refresher goroutine started
	modeBySess       map[string]string          // sessionId -> collaboration mode (build/edit/plan/yolo)
	modelTracked     map[string]map[string]any  // sessionId -> page-chosen switchModelConfig {provider,model,thought}
	modelSwitched    map[string]bool            // sessionId -> switchModelConfig already sent
	clientBySession  map[string]string          // sessionId -> the phone page's clientId (commands must reuse it)
	sessionRevision  map[string]int             // sessionId -> engine conversation revision (for fork/feedback/etc.)
	sendAt           map[string]int64           // sessionId -> unix ms of last sendText (dedupe bypass window)
	optimisticRun    map[string]int64           // sessionId -> unix ms deadline forcing phase=running
	optimisticRow    map[string]map[string]any  // sessionId -> optimistic sent userInput row (idle-session send display)
	lastTurnDone     map[string]int64           // sessionId -> unix ms of last turn.completed/failed refresh kick
	lastRefresh      map[string]int64           // sessionId -> unix ms of last refresher-issued snapshot
	pendingPlans     map[int]string             // synthetic conversationPlansV4 call id -> sessionId
	planStash        map[string]json.RawMessage // sessionId -> latest plans/goal payload
	pendingRaw       map[int]*pendingRawCall    // call id -> encoded promise call (handshake retry)
	retrying         map[int]bool               // call ids with a handshake-retry loop in flight
	suppressAck      map[int]bool               // early-acked queue-op call ids whose engine ack must not reach the page
	seenCommand      map[string]int64           // "sid|type|commandId" -> unix ms of first forward (phone re-delivery dedupe)
	completedAt      map[string]int64           // sessionId -> unix ms the last turn flipped running→ended (drives the phone's 结束蓝点)
	viewedAt         map[string]int64           // sessionId -> unix ms the phone last opened the task (clears the dot)
	lastQueueRescue  map[string]int64           // sessionId -> unix ms of the last stuck-queue rescue (rate limit)
	lastRecentRescue int64                      // unix ms of the last rescueRecentTurns sweep (rate limit)
	ctxBySess        map[string][2]int          // sessionId -> last reported {contextUsed, contextWindow} (usage meter persistence)
	lastRowsAt       map[string]int64           // sessionId -> unix ms of the last conversationRowsRangeV4 reply (rescue freshness gate)
	realFramesAt     map[string]int64           // sessionId -> unix ms of the last LIVE host conversation frame (synthesizer stand-down gate)
	lastEmittedRows  map[string][]map[string]any // sessionId -> row copies of the last emitted snapshot window (delta diff base)
	lastEmittedState map[string]map[string]any   // sessionId -> non-rows parts of the last emitted snapshot (state.updated patch diff base)
	scheduleActive   map[string]bool            // sessionId -> a post-send poll schedule is already running (don't stack more)
	selfInjected     map[int]bool               // daemon-minted call ids whose sendText bookkeeping must be skipped (rescue injects already sit in the mirror)
	lastHandshakeTry int64                      // unix ms of the last daemon-side host handshake bootstrap (rate limit)
}

// initMaps makes every map field. officialRecovery is constructed once at
// host start with zero values, and a write to any nil map panics — which
// takes the whole daemon (and every phone task) down. Call this at every
// construction site; reads of nil maps are safe, so lazily-written maps
// elsewhere stay as-is.
func (r *officialRecovery) initMaps() {
	r.pendingSub = map[int]string{}
	r.pendingRead = map[int]string{}
	r.subBySession = map[string]string{}
	r.epochBySess = map[string]string{}
	r.pendingRows = map[int]string{}
	r.lastRowsJSON = map[string]string{}
	r.resyncPend = map[string]bool{}
	r.snapSeqBy = map[string]int{}
	r.pendingTask = map[int]string{}
	r.queuedSends = map[string][]map[string]any{}
	r.turnRunning = map[string]bool{}
	r.lastQEmitted = map[string]int{}
	r.pendingCreate = map[int]bool{}
	r.snaps = map[string]json.RawMessage{}
	r.pendingResolve = map[int][2]string{}
	r.deadInteractions = map[string]bool{}
	r.modeBySess = map[string]string{}
	r.modelTracked = map[string]map[string]any{}
	r.modelSwitched = map[string]bool{}
	r.clientBySession = map[string]string{}
	r.sessionRevision = map[string]int{}
	r.sendAt = map[string]int64{}
	r.optimisticRun = map[string]int64{}
	r.optimisticRow = map[string]map[string]any{}
	r.lastTurnDone = map[string]int64{}
	r.lastRefresh = map[string]int64{}
	r.pendingPlans = map[int]string{}
	r.planStash = map[string]json.RawMessage{}
	r.pendingRaw = map[int]*pendingRawCall{}
	r.retrying = map[int]bool{}
	r.suppressAck = map[int]bool{}
	r.seenCommand = map[string]int64{}
	r.completedAt = map[string]int64{}
	r.viewedAt = map[string]int64{}
	r.lastQueueRescue = map[string]int64{}
	r.lastRecentRescue = 0
	r.ctxBySess = map[string][2]int{}
	r.lastRowsAt = map[string]int64{}
	r.realFramesAt = map[string]int64{}
	r.lastEmittedRows = map[string][]map[string]any{}
	r.lastEmittedState = map[string]map[string]any{}
	r.scheduleActive = map[string]bool{}
	r.selfInjected = map[int]bool{}
}

// taskStatusNudge is installed by startWebRemote: re-pushes the
// workspace/task list to the phone so task cards flip the 运行 badge and the
// 结束蓝点 live (the task index alone only updates on completion, and nothing
// on the headless side ever writes unread_at).
var taskStatusNudge func()

// officialTaskRuntime reports engine-observed turn state for one task:
// running = turn in flight; unreadAt = ms timestamp of the last completion
// the phone has not opened yet (0 = nothing unread).
func officialTaskRuntime(sid string) (running bool, unreadAt int64) {
	rec := officialActiveRec()
	if rec == nil {
		return false, 0
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	running = rec.turnRunning[sid]
	if rec.completedAt[sid] > rec.viewedAt[sid] {
		unreadAt = rec.completedAt[sid]
	}
	return running, unreadAt
}

// officialMarkTaskViewed records the phone opening a task (bridge-open with
// initialTaskId) so a previously-set unread dot clears on the next list push.
func officialMarkTaskViewed(sid string) {
	if sid == "" {
		return
	}
	rec := officialActiveRec()
	if rec == nil {
		return
	}
	rec.mu.Lock()
	if rec.viewedAt == nil {
		rec.viewedAt = map[string]int64{}
	}
	rec.viewedAt[sid] = time.Now().UnixMilli()
	rec.mu.Unlock()
}

// officialTurnRunning reports whether a task's turn is currently executing.
func officialTurnRunning(sid string) bool {
	if sid == "" {
		return false
	}
	rec := officialActiveRec()
	if rec == nil {
		return false
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.turnRunning[sid]
}

// officialAnyTurnRunning reports whether ANY engine session has a turn in
// flight. The bridge-open restart guard previously consulted
// phoneSessions.anyTurnRunning(), which reads a runningSids map that is never
// populated on the official path — the guard was a no-op and a workspace
// switch could restart the engine (and kill background turns) even mid-turn.
func officialAnyTurnRunning() bool {
	rec := officialActiveRec()
	if rec == nil {
		return false
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, running := range rec.turnRunning {
		if running {
			return true
		}
	}
	return false
}

// officialSyntheticTasks returns engine-known tasks that the on-disk task
// index does not have yet. The host writes the index row at first completion,
// so a task whose turn is still running would be invisible in the phone's
// task list (no card, no 运行 badge). Title falls back to the optimistic sent
// text; caller fills in workspace/timestamps.
func officialSyntheticTasks() []map[string]any {
	rec := officialActiveRec()
	if rec == nil {
		return nil
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	out := []map[string]any{}
	for sid, running := range rec.turnRunning {
		if !running {
			continue
		}
		title := ""
		if row := rec.optimisticRow[sid]; row != nil {
			title, _ = row["text"].(string)
		}
		if title == "" {
			if q := rec.queuedSends[sid]; len(q) > 0 {
				title, _ = q[0]["text"].(string)
			}
		}
		if len(title) > 60 {
			title = title[:60]
		}
		out = append(out, map[string]any{"taskId": sid, "title": title})
	}
	return out
}

// sameAsLast reports whether the rows payload is byte-identical to the last
// emitted snapshot for the session (duplicate application breaks the store).

type officialHostState struct {
	mu sync.Mutex
	// persistedClientID is the phone page's channel clientId, restored from
	// disk at startup (the page keeps the same id in localStorage). Without
	// it the daemon-side host handshake cannot run after a restart until the
	// page happens to reveal its id again.
	persistedClientID string
	active            *officialHostBridge
	nodeBin           string
	script            string
	workspace         string
	mid               string
	engine            *relay.BridgeEngine
	sender            *relaySender
}

var officialState officialHostState

type pendingRawCall struct {
	raw  []byte
	typ  string
	sid  string
	call *relay.ChannelCall // deep copy, for revision-corrected replays
}

// officialActiveRec returns the active recovery recorder, or nil.

var runtimeHeadersAnswered sync.Map
