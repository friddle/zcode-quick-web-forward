// phoneSessions: the phone's live conversation/session state — event
// listeners, resume aliases, runtime tasks, queued sends, pending
// interactions and the projection's config/usage blocks.

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	enginepkg "github.com/friddle/zcode-quick-web-forward/internal/engine"
	"github.com/friddle/zcode-quick-web-forward/internal/zcode"
)

// phoneSessions tracks the phone's active conversation session and EventListen
// subscription ids so we can push conversation/sessions-index frames. The
// subscriptionId must be stable across subscribe ack / pushed frames / resync.
type phoneSessions struct {
	mu debugMutex
	// sessionId currently open in the phone (minted by createSession ack)
	sessionId string
	// workspacePath of the current bridge
	workspacePath string
	// conversation subscription id (echoed in every conversation frame + resync)
	convSubscription string
	// EventListen ids we must push frames to (onDynamicConversationFrame etc.)
	convListener, indexListener, runtimeListener int
	// logicalFrameOrdinal must strictly increase per (topic, subscriptionId)
	frameOrdinal int
	// runtimeTasks are sessions the engine created during this run (not yet in
	// the on-disk task index), so the phone shows tasks that are actually
	// running instead of "no tasks to display".
	runtimeTasks map[string]map[string]any
	// collabMode is the phone's current collaboration mode
	// (confirm/edit/plan/yolo). confirm means Edit/Write need asking.
	collabMode string
	// runningSids tracks which engine sessions have a turn in flight.
	// Multiple phone tasks can run turns concurrently; a send that targets a
	// *different* session must not be queued behind it.
	runningSids map[string]bool
	// pendingQueue holds sendText submissions that arrived while a turn was
	// still running. The phone renders them as queued bubbles (projection
	// queue.items); they drain FIFO on turn.terminal.
	pendingQueue []queuedSend
	// lastRows are the rows of the last conversation snapshot pushed for the
	// live session, so queue updates can re-send a consistent projection.
	lastRows []any
	// listeners records EventListen ids by purpose so events can be pushed to
	// them (provider registry changes, workspace events, controller frames).
	listeners map[string]int
	// resumeAlias maps a phone-visible (task) sessionId to the live engine
	// session we rebuilt it as, so continuing a historical task (whose engine
	// session died on daemon restart) executes against a real engine session
	// while the phone keeps seeing its original task id.
	resumeAlias map[string]string
	// curProvider/curModel/curThought mirror the live session's model config
	// so the conversation projection (config.thought / thoughtLevels) and the
	// model-change timeline markers report what actually runs.
	curProvider, curModel, curThought string
	// thoughtLevelsOverride carries the engine's per-session reasoning-effort
	// options (settings.thoughtLevel.available) when known.
	thoughtLevelsOverride []string
	// ctxUsed/ctxMax hold the engine's live context usage (runtime.contextUsage).
	ctxUsed, ctxMax int64
	// rowIDSeq backs nextRowID (live-pushed row ids). Seeded high so live
	// rows never collide with transcript snapshot row ids (messageRows mint
	// 1..N per snapshot — a colliding row.appended breaks the client's rows
	// window and suppresses the anchored question card).
	rowIDSeq int
	// pendingInteractions holds engine interaction/requestUserInput calls
	// (AskUserQuestion) waiting for the user's answer, keyed by interactionId
	// (= the engine's requestId). They ride the projection's
	// pendingInteractions list; the phone answers via the resolveInteraction
	// conversation command.
	pendingInteractions map[string]*pendingInteraction
}

// pendingInteraction is one engine question awaiting a user answer.
type pendingInteraction struct {
	InteractionID string
	EngineReqID   json.RawMessage
	SessionID     string
	ToolCallID    string
	Prompt        string
	Questions     []map[string]any // {question, options:[{optionId,label}]}
	Input         map[string]any   // the engine's original request input (questions verbatim)
	RowID         int              // conversation row showing the question
}

func (p *phoneSessions) recordListener(kind string, id int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.listeners == nil {
		p.listeners = map[string]int{}
	}
	p.listeners[kind] = id
}

func (p *phoneSessions) listener(kind string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.listeners[kind]
}

// setResumeAlias maps phoneSid -> engineSid (a rebuilt continuation session).
func (p *phoneSessions) setResumeAlias(phoneSid, engineSid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.resumeAlias == nil {
		p.resumeAlias = map[string]string{}
	}
	p.resumeAlias[phoneSid] = engineSid
}

// engineFor returns the live engine session id for a phone-visible session id,
// following the resume alias if one exists. If the phoneSid itself is a live
// engine session, it returns it unchanged.
func (p *phoneSessions) engineFor(phoneSid string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.resumeAlias != nil {
		if e := p.resumeAlias[phoneSid]; e != "" {
			return e
		}
	}
	return phoneSid
}

// phoneFor returns the phone-visible task id an engine session event belongs
// to (reverse alias lookup), so engine events on a rebuilt session are pushed
// under the original task id.
func (p *phoneSessions) phoneFor(engineSid string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	for phone, e := range p.resumeAlias {
		if e == engineSid {
			return phone
		}
	}
	return engineSid
}

func (p *phoneSessions) nextOrdinal() int {
	p.mu.Lock()
	p.frameOrdinal++
	n := p.frameOrdinal
	p.mu.Unlock()
	return n
}

func (p *phoneSessions) setSession(id, ws string) {
	p.mu.Lock()
	p.sessionId, p.workspacePath = id, ws
	if p.runtimeTasks == nil {
		p.runtimeTasks = map[string]map[string]any{}
	}
	if id != "" {
		now := time.Now().UnixMilli()
		if _, ok := p.runtimeTasks[id]; !ok {
			p.runtimeTasks[id] = map[string]any{
				"taskId":        id,
				"title":         "新任务",
				"workspaceKey":  ws,
				"workspacePath": ws,
				"displayStatus": "running",
				// Sessions the phone merely points at (a fresh composer, an
				// opened conversation) are drafts until real input arrives;
				// drafts stay out of the task list.
				"draft":     true,
				"createdAt": now,
				"updatedAt": now,
			}
		}
	}
	p.mu.Unlock()
}

// queuedSend is one sendText that arrived while a turn was still running.
type queuedSend struct {
	text            string
	sourceCommandID string
	queueItemID     string
	clientID        string
	admittedAt      int64
	// sessionId is the phone-visible task this submission belongs to, so
	// concurrent tasks drain their own queues instead of crossing wires.
	sessionId string
}

// queueItemPayload renders a queued send in the phone's official queue-item
// shape (strict zod: delivery/order/steer/dispatch are all required).
func queueItemPayload(q queuedSend, pos int) map[string]any {
	return map[string]any{
		"sourceCommandId": q.sourceCommandID,
		"queueItemId":     q.queueItemID,
		"clientId":        q.clientID,
		"kind":            "sendText",
		"text":            q.text,
		"attachments":     []any{},
		"delivery":        map[string]any{"requested": "auto", "admitted": "queue"},
		"order":           map[string]any{"admissionSeq": pos + 1, "queuePosition": pos},
		"steer":           map[string]any{"state": "notRequested"},
		"dispatch":        map[string]any{"state": "queued"},
		"admittedAt":      q.admittedAt,
	}
}

// enqueueSend appends a submission to the pending queue.
func (p *phoneSessions) enqueueSend(q queuedSend) {
	p.mu.Lock()
	p.pendingQueue = append(p.pendingQueue, q)
	p.mu.Unlock()
}

// popQueuedSend removes and returns the next queued submission bound to one
// phone session (empty sid takes the global head, for compatibility).
func (p *phoneSessions) popQueuedSend(sid string) (queuedSend, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if sid != "" {
		for i, q := range p.pendingQueue {
			if q.sessionId == sid {
				p.pendingQueue = append(p.pendingQueue[:i], p.pendingQueue[i+1:]...)
				return q, true
			}
		}
		return queuedSend{}, false
	}
	if len(p.pendingQueue) == 0 {
		return queuedSend{}, false
	}
	q := p.pendingQueue[0]
	p.pendingQueue = p.pendingQueue[1:]
	return q, true
}

// queueItemsPayload renders the pending queue (positions are 0-based).
func (p *phoneSessions) queueItemsPayload() []any {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]any, 0, len(p.pendingQueue))
	for i, q := range p.pendingQueue {
		out = append(out, queueItemPayload(q, i))
	}
	return out
}

// setTurnRunning records whether one engine session has a turn in flight.
func (p *phoneSessions) setTurnRunning(sid string, v bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.runningSids == nil {
		p.runningSids = map[string]bool{}
	}
	if v {
		p.runningSids[sid] = true
	} else {
		delete(p.runningSids, sid)
	}
}

// turnRunningFor reports whether a turn is in flight for one engine session.
func (p *phoneSessions) turnRunningFor(sid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.runningSids[sid]
}

// rememberRows stores the rows of the last conversation snapshot.
func (p *phoneSessions) rememberRows(rows []any) {
	p.mu.Lock()
	p.lastRows = rows
	p.mu.Unlock()
}

// snapshotRows returns a copy of the last conversation snapshot rows.
func (p *phoneSessions) snapshotRows() []any {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]any, len(p.lastRows))
	copy(out, p.lastRows)
	return out
}

// runtimeTask adds a session created by the engine so it shows in the phone.
// draft=true marks a bare composer draft: kept out of the task list until the
// first message promotes it (see bridgeSendCommand sendText).
func (p *phoneSessions) runtimeTask(sessionID, workspace, title string, draft bool) {
	p.mu.Lock()
	if p.runtimeTasks == nil {
		p.runtimeTasks = map[string]map[string]any{}
	}
	if title == "" {
		title = "新任务"
	}
	now := time.Now().UnixMilli()
	p.runtimeTasks[sessionID] = map[string]any{
		"taskId":         sessionID,
		"title":          title,
		"workspacePath":  workspace,
		"workspaceLabel": pathLabel(workspace),
		"workspaceKind":  "local",
		"displayStatus":  "running",
		"draft":          draft,
		"createdAt":      now,
		"updatedAt":      now,
	}
	p.mu.Unlock()
}

// runtimeTaskList returns the runtime-created tasks merged into the on-disk
// task list so the phone always has something to show.
func (p *phoneSessions) runtimeTaskList() []any {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := []any{}
	for _, t := range p.runtimeTasks {
		out = append(out, t)
	}
	return out
}

// removeRuntimeTask drops a session from the runtime task map (deleteSession).
func (p *phoneSessions) removeRuntimeTask(sessionID string) {
	p.mu.Lock()
	delete(p.runtimeTasks, sessionID)
	p.mu.Unlock()
}

// liveTaskIDs returns the set of task ids whose sessions are currently live
// (the open session while a turn runs, plus any running runtime tasks).
func (p *phoneSessions) liveTaskIDs() map[string]bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]bool{}
	for esid := range p.runningSids {
		for phone, e := range p.resumeAlias {
			if e == esid {
				out[phone] = true
			}
		}
		out[esid] = true
	}
	for id, t := range p.runtimeTasks {
		if s, _ := t["displayStatus"].(string); s == "running" {
			out[id] = true
		}
	}
	return out
}

// taskMeta returns the workspace path + title known for a session id (looks in
// the runtime map first, then the on-disk task index).
func taskMeta(ps *phoneSessions, sid string) (ws, title string) {
	ps.mu.Lock()
	if t, ok := ps.runtimeTasks[sid]; ok {
		ws, _ = t["workspacePath"].(string)
		title, _ = t["title"].(string)
	}
	ps.mu.Unlock()
	if ws == "" {
		if tasks, err := zcode.ListTasks("", ""); err == nil {
			for _, t := range tasks {
				if t.TaskID == sid {
					ws, title = t.WorkspacePath, t.Title
					break
				}
			}
		}
	}
	if title == "" {
		title = "新任务"
	}
	return ws, title
}

// rebuildContinuedSession recreates a live engine session for a historical task
// whose engine session died (daemon restart). It reads the saved transcript,
// creates a fresh engine session in the task's workspace, feeds the history in
// as context, and records phoneSid->engineSid so later events keep showing
// under the original task id. Returns the new engine session id ("" on error).
func rebuildContinuedSession(engClient *enginepkg.Client, ps *phoneSessions, phoneSid string) string {
	ws, _ := taskMeta(ps, phoneSid)
	if ws == "" {
		fmt.Printf("zcode: cannot rebuild %s: no workspace\n", phoneSid)
		return ""
	}
	defP, defM := zcode.DefaultModel()
	if defP == "" || defM == "" {
		defP, defM = "bigmodel", "GLM-5.3"
	}
	res, err := engClient.CreateSession(ws, ws, defP, defM, 15*time.Second)
	if err != nil {
		fmt.Printf("zcode: rebuild create failed: %v\n", err)
		return ""
	}
	newSid, _ := res["sessionId"].(string)
	if newSid == "" {
		if s, ok := res["session"].(map[string]any); ok {
			newSid, _ = s["sessionId"].(string)
		}
	}
	if newSid == "" {
		return ""
	}
	ps.setResumeAlias(phoneSid, newSid)
	fmt.Printf("zcode: rebuilt continuation %s -> engine %s (workspace %s)\n", phoneSid, newSid, ws)
	return newSid
}

func (p *phoneSessions) get() (string, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sessionId, p.workspacePath
}

func (p *phoneSessions) setConvSubscription(id string) {
	p.mu.Lock()
	p.convSubscription = id
	p.mu.Unlock()
}

func (p *phoneSessions) convSub() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.convSubscription
}

// setModelConfig records the live session's model selection (from
// createSession / switchModelConfig) for the conversation projection.
func (p *phoneSessions) setModelConfig(provider, model, thought string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if provider != "" {
		p.curProvider = provider
	}
	if model != "" {
		p.curModel = model
	}
	if thought != "" {
		p.curThought = thought
	}
}

// nextRowID mints strictly-increasing conversation row ids for live rows
// (queue bubbles, model-change markers, question cards).
func (p *phoneSessions) nextRowID() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.rowIDSeq < 100000 {
		p.rowIDSeq = 100000
	}
	p.rowIDSeq++
	return p.rowIDSeq
}

// setThoughtLevels records the engine's reasoning-effort options for the
// session's current model.
func (p *phoneSessions) setThoughtLevels(levels []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(levels) > 0 {
		p.thoughtLevelsOverride = levels
	}
}

// setContextUsage records the engine's live context-window usage.
func (p *phoneSessions) setContextUsage(used, max int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ctxUsed, p.ctxMax = used, max
}

// addPendingInteraction records an engine question awaiting an answer.
func (p *phoneSessions) addPendingInteraction(pi *pendingInteraction) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pendingInteractions == nil {
		p.pendingInteractions = map[string]*pendingInteraction{}
	}
	p.pendingInteractions[pi.InteractionID] = pi
}

// removePendingInteraction drops a question once it has been answered.
func (p *phoneSessions) removePendingInteraction(interactionID string) {
	p.mu.Lock()
	delete(p.pendingInteractions, interactionID)
	p.mu.Unlock()
}

// getPendingInteraction looks a question up by interactionId.
func (p *phoneSessions) getPendingInteraction(interactionID string) *pendingInteraction {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pendingInteractions[interactionID]
}

// oldestPendingInteractionFor returns the earliest pending question for a
// session, if any. The engine is blocked while one is open, so a typed
// composer message counts as the answer.
func (p *phoneSessions) oldestPendingInteractionFor(sessionID string) *pendingInteraction {
	p.mu.Lock()
	defer p.mu.Unlock()
	var best *pendingInteraction
	for _, pi := range p.pendingInteractions {
		if pi.SessionID != sessionID {
			continue
		}
		if best == nil || pi.RowID < best.RowID {
			best = pi
		}
	}
	return best
}

// pendingInteractionsPayload renders the pending questions in the official
// projection entry shape (wl schema: interactionId/kind/anchorRowId/createdAt/
// payload{kind:"userInput",prompt,freeText,options,questions,...}).
func (p *phoneSessions) pendingInteractionsPayload() []any {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]any, 0, len(p.pendingInteractions))
	now := time.Now().UnixMilli()
	for _, pi := range p.pendingInteractions {
		questions := make([]any, 0, len(pi.Questions))
		flatOptions := []any{}
		for i, q := range pi.Questions {
			opts, _ := q["options"].([]map[string]any)
			qo := make([]any, 0, len(opts))
			for _, o := range opts {
				// Client schema (bl) requires value+label per option; the
				// quick-reply chips (payload.options) use optionId+label.
				qo = append(qo, map[string]any{"value": o["optionId"], "label": o["label"]})
			}
			entry := map[string]any{"question": q["question"], "options": qo}
			if h, _ := q["header"].(string); h != "" {
				entry["header"] = h
			}
			questions = append(questions, entry)
			// The interaction-level option list mirrors the first question —
			// the phone renders it as the quick-reply chips.
			if i == 0 {
				for _, o := range opts {
					flatOptions = append(flatOptions, map[string]any{
						"optionId": o["optionId"], "label": o["label"],
					})
				}
			}
		}
		payload := map[string]any{
			"kind":     "userInput",
			"prompt":   pi.Prompt,
			"freeText": len(pi.Questions) == 0,
			"toolName": "AskUserQuestion",
		}
		if pi.ToolCallID != "" {
			payload["toolCallId"] = pi.ToolCallID
		}
		if len(flatOptions) > 0 {
			payload["options"] = flatOptions
		}
		if len(questions) > 0 {
			payload["questions"] = questions
		}
		out = append(out, map[string]any{
			"interactionId": pi.InteractionID,
			"kind":          "userInput",
			"anchorRowId":   pi.RowID,
			"createdAt":     now,
			"payload":       payload,
		})
	}
	return out
}

// usageCfg builds the projection's usage block from the engine's live
// contextUsage (runtime.contextUsage in the session/read reply).
func (p *phoneSessions) usageCfg() map[string]any {
	p.mu.Lock()
	used, max := p.ctxUsed, p.ctxMax
	p.mu.Unlock()
	if max <= 0 {
		// Engine default for the GLM models until runtime.contextUsage lands.
		max = 1000000
	}
	return map[string]any{
		"contextWindow": map[string]any{
			"usedTokens":                 used,
			"maxTokens":                  max,
			"autoCompactThresholdTokens": nil,
		},
		"cumulative": map[string]any{
			"inputTokens": 0, "outputTokens": 0, "cacheReadTokens": 0, "cacheWriteTokens": 0,
		},
	}
}

// thoughtLevelsFor mirrors the desktop's per-model reasoning-effort options
// (the official client shows low/high/max for the GLM models). Used as the
// fallback before the engine reports settings.thoughtLevel.available.
func thoughtLevelsFor(model string) []string {
	return []string{"low", "high", "max"}
}

// modelCfg returns the projection's config block: the real provider/model/
// thought when known, the phone's expected fallbacks otherwise.
func (p *phoneSessions) modelCfg() map[string]any {
	p.mu.Lock()
	provider, model, thought := p.curProvider, p.curModel, p.curThought
	levels := p.thoughtLevelsOverride
	mode := p.collabMode
	p.mu.Unlock()
	if provider == "" {
		if defP, _ := zcode.DefaultModel(); defP != "" {
			provider = defP
		} else {
			provider = "bigmodel"
		}
	}
	if model == "" {
		if _, defM := zcode.DefaultModel(); defM != "" {
			model = defM
		} else {
			model = "GLM-5.3"
		}
	}
	if thought == "" {
		thought = "low"
	}
	if len(levels) == 0 {
		levels = []string{"low", "high", "max"}
	}
	if mode == "" {
		mode = "build"
	}
	lv := make([]any, 0, len(levels))
	for _, l := range levels {
		lv = append(lv, l)
	}
	return map[string]any{
		"provider":      provider,
		"model":         model,
		"thought":       thought,
		"thoughtLevels": lv,
		"followupMode":  "queue",
		"mode":          mode,
	}
}

// debugMutex wraps sync.Mutex recording the holder's stack, so a wedged
// phoneSessions lock can be diagnosed from a goroutine dump or /tmp log
// instead of silently freezing the phone bridge.
type debugMutex struct {
	mu     sync.Mutex
	holder atomic.Value // string: holder stack snapshot
}

func (d *debugMutex) Lock() {
	t0 := time.Now()
	d.mu.Lock()
	b := make([]byte, 4096)
	n := runtime.Stack(b, false)
	d.holder.Store(string(b[:n]))
	if wait := time.Since(t0); wait > 2*time.Second {
		if h, ok := d.holder.Load().(string); ok {
			fmt.Fprintf(os.Stderr, "zcode: phoneSessions.mu waited %v (previous holder below)\n%s", wait, h)
		}
	}
}

func (d *debugMutex) Unlock() {
	d.holder.Store("")
	d.mu.Unlock()
}
