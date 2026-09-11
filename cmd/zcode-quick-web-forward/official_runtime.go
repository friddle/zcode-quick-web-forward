package main

import (
	"bytes"
	"fmt"
	"github.com/friddle/zcode-quick-web-forward/internal/relay"
	"log"
	"os"
	"regexp"
	"strings"
)

func captchaParamFile() string {
	if p := os.Getenv("ZQF_CAPTCHA_PARAM_FILE"); p != "" {
		return p
	}
	return "/tmp/zqwf-captcha-param.txt"
}

func maybeAnswerRuntimeHeadersRequest(logLine string) {
	const marker = `收到 ZCode provider runtime headers 请求`
	if !strings.Contains(logLine, marker) {
		return
	}
	m := regexp.MustCompile(`requestId\\+":\\"([A-Za-z0-9:_-]+)`).FindStringSubmatch(logLine)
	if m == nil {
		log.Println("zcode: runtime-headers request seen but requestId not parsed")
		return
	}
	requestID := m[1]
	if _, dup := runtimeHeadersAnswered.LoadOrStore(requestID, true); dup {
		return
	}
	sessionID := requestID
	if i := strings.Index(requestID, ":provider-runtime-headers:"); i > 0 {
		sessionID = requestID[:i]
	}
	officialState.mu.Lock()
	ws := officialState.workspace
	officialState.mu.Unlock()
	if ws == "" || sessionID == requestID {
		return
	}
	// NOTE: the request/response keys resolve the workspace from the TOP-LEVEL
	// workspacePath/workspaceIdentity fields (resolveWorkspaceKey of the args
	// object itself) — the desktop renderer passes them flat, not nested under
	// a "workspace" key.
	//
	// The plan gateway (provider_code 3007) REQUIRES the Aliyun captcha header
	// on every model request (F008: each verify param is single-use). The
	// captcha runner (chrome-driverless on 9333 + /tmp/zqwf-run-captcha.py)
	// produces one verify param into ZQF_CAPTCHA_PARAM_FILE; we attach it here
	// and consume the file, so a stale param can never be double-submitted.
	// Without a fresh param we still answer (fast visible failure instead of
	// an infinite "Working…").
	headers := "{}"
	if p := captchaParamFile(); p != "" {
		if b, err := os.ReadFile(p); err == nil && len(bytes.TrimSpace(b)) > 0 {
			param := strings.TrimSpace(string(b))
			headers = fmt.Sprintf(`{"X-Aliyun-Captcha-Verify-Param":%q}`, param)
			_ = os.Remove(p) // F008: single-use — never resubmit
			fmt.Printf("zcode: attaching captcha verify param (%d chars) from %s\n", len(param), p)
		}
	}
	args := fmt.Sprintf(`{"workspacePath":%q,"sessionId":%q,"requestId":%q,"response":{"headersApplied":true,"runtimeProviderHeaders":%s}}`,
		ws, sessionID, requestID, headers)
	frame := buildChannelCall("zcode-agent", "respondProviderRuntimeHeaders", args, 0x4000)
	if forwardRawToOfficialHost(frame) {
		fmt.Printf("zcode: answered provider runtime headers request %s (headersApplied=false)\n", requestID)
	}
}

// appendVQL appends v as a LEB128-style varint (VSCode rpc wire encoding).

func appendVQL(buf []byte, v int) []byte {
	for {
		b := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			buf = append(buf, b|0x80)
		} else {
			return append(buf, b)
		}
	}
}

// buildChannelCall encodes one channel PromiseCall frame:
//
//	[4,4] [6,100] [6,id] [1,len]channel [1,len]method  args
//	args := [4,1] [5,len]json   (one-element array holding the params object —
//	the host's call adapters spread the wire arg positionally)
func buildChannelCall(channel, method, argsJSON string, id int) []byte {
	var buf []byte
	buf = append(buf, 0x04, 0x04, 0x06, 0x64, 0x06)
	buf = appendVQL(buf, id)
	buf = append(buf, 0x01)
	buf = appendVQL(buf, len(channel))
	buf = append(buf, channel...)
	buf = append(buf, 0x01)
	buf = appendVQL(buf, len(method))
	buf = append(buf, method...)
	buf = append(buf, 0x04, 0x01, 0x05)
	buf = appendVQL(buf, len(argsJSON))
	buf = append(buf, argsJSON...)
	return buf
}

// channelPromiseID extracts the call id from a PromiseSuccess channel
// message: [0x04 array][len=2][0x06 kind=201][0x06 id][data]. Returns false
// for any other message kind.
func channelPromiseID(b []byte) (int, bool) {
	if len(b) < 7 || b[0] != 4 || b[1] != 2 || b[2] != 6 {
		return 0, false
	}
	kind, next, ok := leb128(b, 3)
	if !ok || kind != 201 || next >= len(b) || b[next] != 6 {
		return 0, false
	}
	id, _, ok := leb128(b, next+1)
	return id, ok
}

// leb128 reads one LEB128 varint at off; returns the value, the offset just
// past it, and whether a complete varint was present.
func leb128(b []byte, off int) (int, int, bool) {
	id, shift := 0, uint(0)
	for i := off; i < len(b) && i < off+5; i++ {
		v := b[i]
		id |= int(v&0x7f) << shift
		if v&0x80 == 0 {
			return id, i + 1, true
		}
		shift += 7
	}
	return 0, off, false
}

// onPortBytes pipes host service-port bytes back to the phone verbatim —
// responses AND the host's channel initialize; the phone's channel stack
// speaks the same protocol, so the bridge stays a pure pipe.

func (b *officialHostBridge) onPortBytes(portID string, raw []byte) {
	if len(raw) == 0 {
		return
	}
	b.mu.Lock()
	want := b.svcPort
	if want == "" {
		want = "svc"
	}
	b.mu.Unlock()
	if portID != want {
		return // stale port from a previous bridge generation
	}
	fmt.Printf("zcode: official-host <- svc %d bytes\n", len(raw))
	inspectOfficialResponse(raw)
	if !hostForwardEnabled() {
		return // pipe disabled — log only, don't leak host bytes to the phone
	}
	if id, ok := channelPromiseID(raw); ok {
		b.mu.Lock()
		if b.answered == nil {
			b.answered = map[int]bool{}
		}
		b.answered[id] = true
		b.mu.Unlock()
		// An early-acked queue op's real engine ack (often a stale
		// rejection pending replay) must not reach the page — it would
		// contradict the accepted answer and abort the user's flow.
		if b.rec != nil {
			b.rec.mu.Lock()
			sup := b.rec.suppressAck[id]
			delete(b.rec.suppressAck, id)
			b.rec.mu.Unlock()
			if sup {
				fmt.Printf("zcode: recovery: swallowed engine ack for early-acked queue op (call %d)\n", id)
				return
			}
		}
	}
	if b.engine == nil || !b.engine.HasIdentity() {
		// The host speaks before the phone's bridge exists (its channel
		// initialize arrives at attach time). Buffer until bridge-open.
		b.mu.Lock()
		b.pendingOut = append(b.pendingOut, raw)
		b.mu.Unlock()
		return
	}
	// Route through the CURRENT relay sender. An empty send func here
	// silently swallowed every host response — the phone waited forever on
	// its channel calls and re-bootstrapped in a loop (Paired. Loading
	// workspace… with RPCs answered host-side but nothing arriving).
	b.engine.SendRawChannelBytes(raw, senderSend())
}

// senderSend returns a routing func that always targets the relay connection
// current at call time.
func senderSend() func(any) {
	return func(v any) {
		officialState.mu.Lock()
		s := officialState.sender
		officialState.mu.Unlock()
		if s != nil {
			s.send(v)
		}
	}
}

// officialReattach attaches a FRESH service port on the live host. Called on
// every workspace-bridge-open; a repeated attach with the same attachmentId
// does not survive the host's dispose of the previous bridge generation.
func officialReattach() {
	officialState.mu.Lock()
	b := officialState.active
	officialState.mu.Unlock()
	if b == nil || !b.h.Alive() {
		return
	}
	b.mu.Lock()
	b.attachSeq++
	id := fmt.Sprintf("zqf-svc-%d", b.attachSeq)
	b.svcPort = id
	b.mu.Unlock()
	b.h.ParentPort(map[string]any{
		"type":         "attach-service-port",
		"requestId":    uuidNew(),
		"attachmentId": id,
		"clientMode":   "desktop-continuous",
		"scope":        map[string]any{"kind": "local"},
	}, id)
	b.h.PortOpen(id)
	fmt.Printf("zcode: official-host reattached service port %s\n", id)
}

// officialCallAnswered reports whether the host replied to a promise call.
func officialCallAnswered(id int) bool {
	officialState.mu.Lock()
	b := officialState.active
	officialState.mu.Unlock()
	if b == nil {
		return true // not active — never fall back
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.answered[id]
}

// officialFlushOut sends buffered host bytes now that the phone bridge
// identity exists. Called from the workspace-bridge-open handler.
func officialFlushOut() {
	officialState.mu.Lock()
	b := officialState.active
	officialState.mu.Unlock()
	if b == nil {
		return
	}
	b.mu.Lock()
	pending := b.pendingOut
	b.pendingOut = nil
	b.mu.Unlock()
	for _, raw := range pending {
		if b.engine == nil {
			break
		}
		fmt.Printf("zcode: official-host flushing buffered %d bytes\n", len(raw))
		b.engine.SendRawChannelBytes(raw, senderSend())
	}
}

// officialStopHost tears the host down (its engine child dies with it).
func officialStopHost() {
	officialState.mu.Lock()
	b := officialState.active
	officialState.active = nil
	officialState.mu.Unlock()
	if b != nil {
		b.h.Stop()
		fmt.Println("zcode: official host stopped")
	}
}

// officialRestartEngine rebuilds the whole host with the stored engine
// command — used as restartEngine in official-host mode (workspace switch).
func officialRestartEngine() {
	officialState.mu.Lock()
	node, script, ws, mid := officialState.nodeBin, officialState.script, officialState.workspace, officialState.mid
	engine, sender := officialState.engine, officialState.sender
	officialState.mu.Unlock()
	officialStopHost()
	engine = relay.NewBridgeEngine()
	if maybeStartOfficialHost(engine, sender, node, script, ws, mid) {
		fmt.Println("zcode: official host respawned")
	}
}
