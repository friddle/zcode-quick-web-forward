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
}

// sameAsLast reports whether the rows payload is byte-identical to the last
// emitted snapshot for the session (duplicate application breaks the store).

type officialHostState struct {
	mu        sync.Mutex
	active    *officialHostBridge
	nodeBin   string
	script    string
	workspace string
	mid       string
	engine    *relay.BridgeEngine
	sender    *relaySender
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
