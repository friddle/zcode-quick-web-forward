// Bridge wire framing for ZCode web-remote rpc-frame transport.
//
// A bridge carries a byte stream (here: the app-server's newline-delimited
// JSON stdio protocol) between the phone and this device. Each logical
// message is checksummed (crc32, 8 lowercase hex chars), optionally split
// into fragments, and sent as {"zcode_type":"rpc-frame", ...} envelopes of
// at most maxPhysicalFrameBytes each. The receiver reassembles by messageSeq
// and acknowledges with {"zcode_type":"rpc-frame-ack", ackMessageSeq}.
//
// Limits mirror the desktop client (le={...}): 16 MiB per message, at most
// 64 fragments, 1024 bytes per physical frame.
package webremote

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"sort"
	"sync"
)

const (
	maxPhysicalFrameBytes = 1024
	maxMessageBytes       = 16 * 1024 * 1024
	maxFragments          = 64
	// dataBudget is a conservative base64 payload budget per frame; the
	// whole JSON envelope stays comfortably under maxPhysicalFrameBytes.
	dataBudget = 512
)

type frameIdentity struct {
	BridgeSessionID  string `json:"bridgeSessionId"`
	BridgeGeneration *int   `json:"bridgeGeneration,omitempty"`
	RecoveryID       string `json:"recoveryId,omitempty"`
}

type rpcFrame struct {
	ZcodeType string `json:"zcode_type"`
	frameIdentity
	Seq           int    `json:"seq"`
	MessageSeq    int    `json:"messageSeq"`
	FragmentIndex int    `json:"fragmentIndex"`
	FragmentCount int    `json:"fragmentCount"`
	MessageBytes  int    `json:"messageBytes"`
	Checksum      crcSum `json:"checksum"`
	DataBase64    string `json:"dataBase64"`
}

type rpcAck struct {
	ZcodeType string `json:"zcode_type"`
	frameIdentity
	AckMessageSeq int `json:"ackMessageSeq"`
}

type crcSum struct {
	Algorithm string `json:"algorithm"`
	Value     string `json:"value"`
}

func crc32Hex(b []byte) crcSum {
	return crcSum{Algorithm: "crc32", Value: fmt.Sprintf("%08x", crc32.ChecksumIEEE(b))}
}

// frameEncoder produces sequential physical frames across messages. Frame
// seqs start at 1: the protocol requires positive safe integers
// (assertPositiveSafe on firstPhysicalSeq).
type frameEncoder struct {
	mu      sync.Mutex
	nextSeq int
	nextMsg int
}

func newFrameEncoder() *frameEncoder {
	return &frameEncoder{nextSeq: 1}
}

// encode splits one logical message into rpc-frame envelopes.
func (e *frameEncoder) encode(id frameIdentity, msg []byte) ([]rpcFrame, error) {
	if len(msg) == 0 {
		return nil, fmt.Errorf("bridge: empty message")
	}
	if len(msg) > maxMessageBytes {
		return nil, fmt.Errorf("bridge: message too large (%d bytes)", len(msg))
	}
	e.mu.Lock()
	seq0 := e.nextSeq
	msgSeq := e.nextMsg + 1
	e.nextMsg = msgSeq
	e.mu.Unlock()

	sum := crc32Hex(msg)
	count := (len(msg) + dataBudget - 1) / dataBudget
	if count > maxFragments {
		return nil, fmt.Errorf("bridge: message needs %d fragments (max %d)", count, maxFragments)
	}
	frames := make([]rpcFrame, 0, count)
	for i := 0; i < count; i++ {
		lo := i * dataBudget
		hi := lo + dataBudget
		if hi > len(msg) {
			hi = len(msg)
		}
		e.mu.Lock()
		e.nextSeq++
		e.mu.Unlock()
		frames = append(frames, rpcFrame{
			ZcodeType:     "rpc-frame",
			frameIdentity: id,
			Seq:           seq0 + i,
			MessageSeq:    msgSeq,
			FragmentIndex: i,
			FragmentCount: count,
			MessageBytes:  len(msg),
			Checksum:      sum,
			DataBase64:    base64.StdEncoding.EncodeToString(msg[lo:hi]),
		})
	}
	return frames, nil
}

// assembler reassembles inbound rpc-frames into logical messages.
type assembler struct {
	mu       sync.Mutex
	pending  map[int]map[int][]byte // messageSeq -> fragmentIndex -> data
	counts   map[int]int            // messageSeq -> fragmentCount
	bytes    map[int]int            // messageSeq -> total received bytes
	expected map[int]int            // messageSeq -> messageBytes
}

func newAssembler() *assembler {
	return &assembler{
		pending:  map[int]map[int][]byte{},
		counts:   map[int]int{},
		bytes:    map[int]int{},
		expected: map[int]int{},
	}
}

// feed returns the completed message when its last fragment arrives.
func (a *assembler) feed(f rpcFrame) ([]byte, bool, error) {
	data, err := base64.StdEncoding.DecodeString(f.DataBase64)
	if err != nil {
		return nil, false, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	frags, ok := a.pending[f.MessageSeq]
	if !ok {
		frags = map[int][]byte{}
		a.pending[f.MessageSeq] = frags
		a.counts[f.MessageSeq] = f.FragmentCount
		a.expected[f.MessageSeq] = f.MessageBytes
	}
	if _, dup := frags[f.FragmentIndex]; !dup {
		frags[f.FragmentIndex] = data
		a.bytes[f.MessageSeq] += len(data)
	}
	if len(frags) < a.counts[f.MessageSeq] {
		return nil, false, nil
	}
	idx := make([]int, 0, len(frags))
	for i := range frags {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	msg := make([]byte, 0, a.expected[f.MessageSeq])
	for _, i := range idx {
		msg = append(msg, frags[i]...)
	}
	delete(a.pending, f.MessageSeq)
	delete(a.counts, f.MessageSeq)
	delete(a.bytes, f.MessageSeq)
	delete(a.expected, f.MessageSeq)
	if f.Checksum.Value != "" && crc32Hex(msg).Value != f.Checksum.Value {
		return nil, true, fmt.Errorf("bridge: checksum mismatch on message %d", f.MessageSeq)
	}
	return msg, true, nil
}

// BridgeEngine wires an app-server stdio stream to the phone: phone frames
// in, engine stdin out; engine stdout lines in, phone frames out.
type BridgeEngine struct {
	enc      *frameEncoder
	asm      *assembler
	mu       sync.Mutex
	sink     io.Writer     // app-server stdin (newline-delimited JSON)
	identity frameIdentity // set when the phone opens a workspace bridge
	pending  map[int]bool  // JSON-RPC ids awaiting channel replies
}

// NewBridgeEngine creates an engine; call Attach before use.
func NewBridgeEngine() *BridgeEngine {
	return &BridgeEngine{enc: newFrameEncoder(), asm: newAssembler(), pending: map[int]bool{}}
}

// SendChannelInitialize performs the channel handshake: without it the phone
// stays Uninitialized and never issues any request.
func (e *BridgeEngine) SendChannelInitialize(send func(any)) {
	e.sendChannelBytes(initializeMessage(), send)
}

// ReplyChannelPromise wraps a JSON result for a phone channel request id.
func (e *BridgeEngine) ReplyChannelPromise(id int, result []byte, send func(any)) {
	e.sendChannelBytes(promiseSuccess(id, result), send)
}

// RegisterCall marks a JSON-RPC id as originating from a phone channel call.
func (e *BridgeEngine) RegisterCall(id int) {
	e.mu.Lock()
	e.pending[id] = true
	e.mu.Unlock()
}

// ServerLine routes one app-server stdout line: replies to phone-originated
// calls become channel PromiseSuccess frames; everything else is dropped
// (the phone cannot parse raw JSON frames).
func (e *BridgeEngine) ServerLine(line []byte, send func(any)) {
	var msg struct {
		ID     int             `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if json.Unmarshal(line, &msg) != nil || msg.ID == 0 {
		return
	}
	e.mu.Lock()
	wanted := e.pending[msg.ID]
	delete(e.pending, msg.ID)
	e.mu.Unlock()
	if !wanted {
		return
	}
	if len(msg.Error) > 0 {
		e.sendChannelBytes(promiseSuccess(msg.ID, msg.Error), send)
		return
	}
	e.sendChannelBytes(promiseSuccess(msg.ID, msg.Result), send)
}

func (e *BridgeEngine) sendChannelBytes(b []byte, send func(any)) {
	e.mu.Lock()
	id := e.identity
	enc := e.enc
	e.mu.Unlock()
	if id.BridgeSessionID == "" {
		return
	}
	frames, err := enc.encode(id, b)
	if err != nil {
		fmt.Fprintf(os.Stderr, "webremote bridge: %v\n", err)
		return
	}
	for _, f := range frames {
		send(f)
	}
}

// WriteToServer sends one raw protocol line to the app-server's stdin
// (device-initiated requests, e.g. priming workspace state after a bridge
// opens, mirroring the desktop's per-workspace host attach).
func (e *BridgeEngine) WriteToServer(line string) {
	e.mu.Lock()
	sink := e.sink
	e.mu.Unlock()
	if sink != nil {
		_, _ = sink.Write([]byte(line + "\n"))
	}
}

// Attach binds the app-server's stdin as the sink for phone messages.
func (e *BridgeEngine) Attach(sink io.Writer) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sink = sink
}

// SetIdentity records the bridge session the phone opened, so outbound
// frames carry the right bridgeSessionId/bridgeGeneration/recoveryId. Each
// bridge is a fresh frame channel: the phone expects the seq/messageSeq
// series to restart at 1, so the encoder resets with the identity.
func (e *BridgeEngine) SetIdentity(sessionID string, generation *int, recoveryID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.identity = frameIdentity{BridgeSessionID: sessionID, BridgeGeneration: generation, RecoveryID: recoveryID}
	e.enc = newFrameEncoder()
}

// PumpServerLine observes an outbound engine line; channel replies are sent
// via ServerLine, so raw JSON is never framed to the phone.
func (e *BridgeEngine) PumpServerLine(line []byte, send func(any)) {
	msg := append(append([]byte{}, line...), '\n')
	e.mu.Lock()
	id := e.identity
	enc := e.enc
	e.mu.Unlock()
	if id.BridgeSessionID == "" {
		return
	}
	frames, err := enc.encode(id, msg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "webremote bridge: %v\n", err)
		return
	}
	for _, f := range frames {
		send(f)
	}
}

// HandlePhonePayload processes one phone data payload. onCall receives
// decoded channel requests (Promise/EventListen); send replies to the phone.
func (e *BridgeEngine) HandlePhonePayload(payload json.RawMessage, send func(any), onCall func(*ChannelCall)) {
	var head struct {
		ZcodeType string `json:"zcode_type"`
	}
	if json.Unmarshal(payload, &head) != nil {
		return
	}
	switch head.ZcodeType {
	case "rpc-frame":
		var f rpcFrame
		if json.Unmarshal(payload, &f) != nil {
			return
		}
		msg, done, err := e.asm.feed(f)
		if err != nil {
			fmt.Fprintf(os.Stderr, "webremote bridge: %v\n", err)
			return
		}
		if !done {
			return
		}
		send(rpcAck{ZcodeType: "rpc-frame-ack", frameIdentity: f.frameIdentity, AckMessageSeq: f.MessageSeq})
		if call := parseChannelCall(msg); call != nil {
			if onCall != nil {
				onCall(call)
			}
			return
		}
		e.mu.Lock()
		sink := e.sink
		e.mu.Unlock()
		if sink != nil {
			_, _ = sink.Write(msg)
		}
	case "rpc-frame-ack":
		// informational; the CLI keeps no replay buffer in v1
	}
}

// jsonRawMarshal is a tiny helper for building payload objects.
func jsonRawMarshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("{}")
	}
	return b
}
