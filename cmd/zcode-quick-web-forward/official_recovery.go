package main

import (
	"encoding/json"
	"fmt"
	"github.com/friddle/zcode-quick-web-forward/internal/relay"
	"os"
)

func (r *officialRecovery) sameAsLast(sid string, data json.RawMessage) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastRowsJSON != nil && r.lastRowsJSON[sid] == string(data)
}

// rememberRows stores the last emitted rows payload per session.
func (r *officialRecovery) rememberRows(sid string, data json.RawMessage) {
	r.mu.Lock()
	if r.lastRowsJSON == nil {
		r.lastRowsJSON = map[string]string{}
	}
	r.lastRowsJSON[sid] = string(data)
	r.mu.Unlock()
}

// cacheTerminalListen parks a terminal.onDynamic* listen until a
// terminal.create reply provides the terminal id.
func (r *officialRecovery) cacheTerminalListen(c *relay.ChannelCall) {
	r.mu.Lock()
	if r.lastTermID != "" {
		// a terminal already exists — bind and forward immediately.
		// The host's onDynamicData(a) expects the BARE terminal id string.
		id := r.lastTermID
		r.mu.Unlock()
		fmt.Printf("zcode: recovery: terminal listen %s bound to existing id %v\n", c.Name, id)
		forwardRawToOfficialHost(relay.ListenBytes(c.ID, c.ChannelName, c.Name, id))
		return
	}
	if len(r.termListens) > 8 {
		r.termListens = r.termListens[1:]
	}
	r.termListens = append(r.termListens, c)
	r.mu.Unlock()
}

// flushTerminalListens forwards cached terminal listens bound to a freshly
// created terminal id.
func (r *officialRecovery) flushTerminalListens(terminalID string) {
	r.mu.Lock()
	pending := r.termListens
	r.termListens = nil
	r.mu.Unlock()
	for _, c := range pending {
		out := relay.ListenBytes(c.ID, c.ChannelName, c.Name, terminalID)
		fmt.Printf("zcode: recovery: flushing terminal listen %s with id %v\n", c.Name, terminalID)
		forwardRawToOfficialHost(out)
	}
}

// mintID allocates a synthetic promise-call id (high range, never clashes
// with the page's own call ids).
func (r *officialRecovery) mintID() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.nextID < 100000 {
		r.nextID = 100000
	}
	r.nextID++
	r.lastID = r.nextID
	return r.lastID
}

// stashSnap remembers a readSession result for the rows-merge step.
func (r *officialRecovery) stashSnap(sid string, raw json.RawMessage) {
	r.mu.Lock()
	if r.snaps == nil {
		r.snaps = map[string]json.RawMessage{}
		r.snapsOrder = []string{}
	}
	r.snaps[sid] = raw
	r.snapsOrder = append(r.snapsOrder, sid)
	if len(r.snapsOrder) > 8 {
		delete(r.snaps, r.snapsOrder[0])
		r.snapsOrder = r.snapsOrder[1:]
	}
	r.mu.Unlock()
}

// snapFor returns the stashed readSession snapshot of a session.
func (r *officialRecovery) snapFor(sid string) json.RawMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snaps[sid]
}

// stashPlans remembers the latest conversationPlansV4 payload for a session.
func (r *officialRecovery) stashPlans(sid string, raw json.RawMessage) {
	r.mu.Lock()
	if r.planStash == nil {
		r.planStash = map[string]json.RawMessage{}
	}
	r.planStash[sid] = raw
	r.mu.Unlock()
	_ = os.WriteFile("/tmp/zqf-plans.json", raw, 0644)
}

// plansFor returns the stashed conversationPlansV4 payload of a session.
func (r *officialRecovery) plansFor(sid string) json.RawMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.planStash[sid]
}

// buildProjectionSnapshot composes the conversation projection snapshot the
// client validates (protocolVersion 1: control/availability/inputRouting/
// meta/config/usage/queue/pendingCommands/backgroundWorks/rows/...). The
// official rows come from conversationRowsRangeV4; everything static is
// filled with the client-schema-required defaults.

func (r *officialRecovery) epochFor(sid string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.epochBySess[sid]
}

// sessionForSub resolves a subscriptionId (from a resyncConversationV4 call)
// back to its session via the recorded subscribe acks.
func (r *officialRecovery) sessionForSub(sub string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for sid, s := range r.subBySession {
		if s == sub {
			return sid
		}
	}
	return ""
}

func (r *officialRecovery) setEpoch(sid, epoch string) {
	if epoch == "" {
		return
	}
	r.mu.Lock()
	if r.epochBySess == nil {
		r.epochBySess = map[string]string{}
	}
	r.epochBySess[sid] = epoch
	r.mu.Unlock()
}

func (r *officialRecovery) listener() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.listenID
}

// listeners returns the most recent EventListen ids (the page re-registers
// its onDynamicConversationFrame listener on every reconnect/resubscribe;
// emitting to a superseded id is silently dropped, to a current one lands).
func (r *officialRecovery) listeners() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []int{}
	if r.listenID != 0 {
		out = append(out, r.listenID)
	}
	for _, id := range r.priorListenIDs {
		if id != 0 && id != r.listenID {
			out = append(out, id)
		}
	}
	return out
}

func (r *officialRecovery) recordListen(id int) {
	r.mu.Lock()
	if id != 0 && id != r.listenID {
		if r.listenID != 0 {
			r.priorListenIDs = append([]int{r.listenID}, r.priorListenIDs...)
			if len(r.priorListenIDs) > 3 {
				r.priorListenIDs = r.priorListenIDs[:3]
			}
		}
		r.listenID = id
	}
	r.mu.Unlock()
}

func (r *officialRecovery) subFor(sid string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.subBySession[sid]
}

func (r *officialRecovery) setSub(sid, sub string) {
	r.mu.Lock()
	if r.subBySession == nil {
		r.subBySession = map[string]string{}
	}
	r.subBySession[sid] = sub
	r.mu.Unlock()
}

// argMap normalizes a ChannelCall argument into a map.
