// Package webremote registers this machine as a "device" on the official
// ZCode web-remote relay (the same wss://<origin>/ws service the desktop
// client's "continue on your phone" uses) and produces the pairing URL a
// phone opens to attach to this machine.
//
// Protocol (reverse-engineered from the desktop client; relay protocol
// version "2026-07-28"; envelope is {"type": ...} JSON, one document per
// WebSocket text frame):
//
//	connect  wss://<origin>/ws?mid=<deviceMid>   header X-Device-ID: <deviceMid>
//	-> {type:"device_register_init", device_mid, pass_hash, meta, client_ts}
//	<- {type:"device_register_ack", device_sid}
//	-> {type:"auth_init", role:"device", device_sid, meta, client_ts}
//	<- {type:"auth_challenge", nonce}
//	-> {type:"auth_response", device_sid, proof, client_ts}
//	   proof = base64url(HMAC-SHA256(key=pass_hash, msg="<nonce>|device|<device_sid>"))
//	<- {type:"auth_ack", pair_status:"waiting"}
//	heartbeat ~10s: -> {type:"pair_status_query", device_sid, client_ts}
//	              <- {type:"pair_status_ack", pair_status, terminal_sid}
//
// pass_hash = base64(SHA256(password)) with a locally generated password.
// The pairing URL is <origin>/remote/v4?sid=<device_sid>&hash=<pass_hash>
// &t=<ms>&mid=<mid>&name=<name>&app_version=<version> — identical in shape
// to the desktop client's QR code.
package webremote

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"time"
)

// Options configures a relay client run.
type Options struct {
	Origin     string // e.g. https://zcode.z.ai (or ZCODE_BASE_URL); no trailing slash
	DeviceMid  string // stable machine id (X-Device-ID header + mid query)
	DeviceName string // shown on the phone pairing page
	AppVersion string
	StatePath  string // JSON file persisting {device_sid, pass_hash} across runs
}

// Session is a fully authenticated relay registration.
type Session struct {
	PhoneURL  string
	DeviceSid string
}

// state is the persisted device identity so restarts reuse the same sid.
type state struct {
	DeviceSid string `json:"device_sid"`
	PassHash  string `json:"pass_hash"`
}

type relayMsg struct {
	Type        string          `json:"type"`
	DeviceSid   string          `json:"device_sid,omitempty"`
	DeviceMid   string          `json:"device_mid,omitempty"`
	Nonce       string          `json:"nonce,omitempty"`
	PassHash    string          `json:"pass_hash,omitempty"`
	PairStatus  string          `json:"pair_status,omitempty"`
	TerminalSid string          `json:"terminal_sid,omitempty"`
	ClientTs    int64           `json:"client_ts,omitempty"`
	Role        string          `json:"role,omitempty"`
	Proof       string          `json:"proof,omitempty"`
	Meta        *meta           `json:"meta,omitempty"`
	Payload     json.RawMessage `json:"payload,omitempty"`
	Code        string          `json:"code,omitempty"`
	Message     string          `json:"message,omitempty"`
}

type meta struct {
	Platform string `json:"platform"`
	Version  string `json:"version"`
	Name     string `json:"name"`
}

// Handler receives live relay traffic: the payload of every {"type":"data"}
// message from the paired phone, plus a reply function that sends a payload
// back wrapped in a data message.
type Handler struct {
	OnReady  func(Session)
	OnPaired func(terminalSID string)
	OnData   func(payload json.RawMessage, reply func(payload any))
}

// Run drives the whole device lifecycle: register/auth, report the phone URL
// via h.OnReady, then keep the pairing window open (heartbeat, pair
// notifications, data forwarding, reconnect on drops) until ctx is
// cancelled. It never returns an error for a dropped connection — it
// retries; it only stops on ctx cancellation.
func Run(ctx context.Context, o Options, h Handler) {
	for ctx.Err() == nil {
		ws, st, err := connectAndAuth(o)
		if err != nil {
			fmt.Fprintf(os.Stderr, "webremote: relay connect failed: %v; retrying\n", err)
			if !sleepCtx(ctx, 5*time.Second) {
				return
			}
			continue
		}
		saveState(o.StatePath, st)
		sess := Session{PhoneURL: phoneURL(o, st), DeviceSid: st.DeviceSid}
		if h.OnReady != nil {
			h.OnReady(sess)
		}
		keepAlive(ctx, ws, st.DeviceSid, h)
		ws.close()
		if ctx.Err() != nil {
			return
		}
		sleepCtx(ctx, 2*time.Second) // dropped; reconnect shortly
	}
}

// connectAndAuth dials the relay and runs the register/auth state machine
// up to (and including) auth_ack.
func connectAndAuth(o Options) (*client, state, error) {
	st := loadState(o.StatePath)
	if st.PassHash == "" {
		pw := make([]byte, 24)
		if _, err := rand.Read(pw); err != nil {
			return nil, st, err
		}
		sum := sha256.Sum256([]byte("zqf-" + base64.RawURLEncoding.EncodeToString(pw)))
		st.PassHash = base64.StdEncoding.EncodeToString(sum[:])
	}
	m := &meta{Platform: goruntime.GOOS, Version: o.AppVersion, Name: o.DeviceName}

	ws, err := dial(o)
	if err != nil {
		return nil, st, err
	}

	// open the handshake: fresh identity registers, persisted one re-auths
	if st.DeviceSid != "" {
		ws.send(relayMsg{Type: "auth_init", DeviceSid: st.DeviceSid, Role: "device", Meta: m, ClientTs: time.Now().UnixMilli()})
	} else {
		ws.send(relayMsg{Type: "device_register_init", DeviceMid: o.DeviceMid, PassHash: st.PassHash, Meta: m, ClientTs: time.Now().UnixMilli()})
	}

	var authed bool
	var readErr error
	ws.readLoop(func(msg relayMsg) bool {
		switch msg.Type {
		case "device_register_ack":
			st.DeviceSid = msg.DeviceSid
			ws.send(relayMsg{Type: "auth_init", DeviceSid: st.DeviceSid, Role: "device", Meta: m, ClientTs: time.Now().UnixMilli()})
		case "auth_challenge":
			proof := hmacSHA256B64URL(st.PassHash, fmt.Sprintf("%s|device|%s", msg.Nonce, st.DeviceSid))
			ws.send(relayMsg{Type: "auth_response", DeviceSid: st.DeviceSid, Proof: proof, ClientTs: time.Now().UnixMilli()})
		case "auth_ack":
			authed = true
			return false // handshake done; stop this read loop
		case "error":
			readErr = fmt.Errorf("relay error %s: %s", msg.Code, msg.Message)
			return false
		}
		return true
	})
	if !authed {
		ws.close()
		if readErr != nil {
			return nil, st, readErr
		}
		return nil, st, errors.New("relay handshake incomplete")
	}
	return ws, st, nil
}

// keepAlive reads relay messages (pair notifications, phone data) and sends
// the pair-status heartbeat until the socket drops or ctx is cancelled.
func keepAlive(ctx context.Context, ws *client, sid string, h Handler) {
	hbCtx, cancelHB := context.WithCancel(ctx)
	defer cancelHB()
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-t.C:
				ws.send(relayMsg{Type: "pair_status_query", DeviceSid: sid, ClientTs: time.Now().UnixMilli()})
			}
		}
	}()
	reply := func(payload any) {
		ws.send(relayMsg{Type: "data", Payload: mustJSON(payload)})
	}
	ws.readLoop(func(msg relayMsg) bool {
		if dbg := os.Getenv("ZQF_RELAY_DEBUG"); dbg != "" {
			b, _ := json.Marshal(msg)
			fmt.Fprintf(os.Stderr, "webremote debug << %s\n", b)
		}
		switch {
		case msg.PairStatus == "matched" || msg.PairStatus == "paired" || msg.TerminalSid != "":
			if h.OnPaired != nil {
				h.OnPaired(msg.TerminalSid)
			}
		case msg.Type == "data" && len(msg.Payload) > 0 && h.OnData != nil:
			h.OnData(msg.Payload, reply)
		}
		return ctx.Err() == nil && !ws.isClosed()
	})
	cancelHB()
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("{}")
	}
	return b
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

func phoneURL(o Options, st state) string {
	u, err := url.Parse(strings.TrimRight(o.Origin, "/") + "/remote/v4")
	if err != nil {
		return o.Origin + "/remote/v4"
	}
	q := u.Query()
	q.Set("sid", st.DeviceSid)
	q.Set("hash", st.PassHash)
	q.Set("t", fmt.Sprint(time.Now().UnixMilli()))
	if o.DeviceMid != "" {
		q.Set("mid", o.DeviceMid)
	}
	if o.DeviceName != "" {
		q.Set("name", o.DeviceName)
	}
	if o.AppVersion != "" {
		q.Set("app_version", o.AppVersion)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func loadState(path string) (st state) {
	if path == "" {
		return
	}
	b, err := os.ReadFile(path)
	if err == nil {
		_ = json.Unmarshal(b, &st)
	}
	return
}

func saveState(path string, st state) {
	if path == "" || st.DeviceSid == "" || st.PassHash == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	b, _ := json.MarshalIndent(st, "", "  ")
	_ = os.WriteFile(path, b, 0o600)
}

func hmacSHA256B64URL(key, msg string) string {
	h := hmac.New(sha256.New, []byte(key))
	h.Write([]byte(msg))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

// ---- minimal RFC6455 WebSocket client (text frames only) ----

type client struct {
	conn   *tls.Conn
	br     *bufio.Reader
	wmu    sync.Mutex
	closed bool
	done   chan struct{}
}

func dial(o Options) (*client, error) {
	u, err := url.Parse(strings.TrimRight(o.Origin, "/"))
	if err != nil {
		return nil, err
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return nil, fmt.Errorf("webremote: unsupported origin %q", o.Origin)
	}
	addr := u.Host
	if !strings.Contains(addr, ":") {
		addr += ":443"
	}
	tconn, err := tls.Dial("tcp", addr, &tls.Config{ServerName: u.Hostname()})
	if err != nil {
		return nil, err
	}
	keyB := make([]byte, 16)
	rand.Read(keyB)
	path := "/ws?mid=" + url.QueryEscape(o.DeviceMid)
	fmt.Fprintf(tconn, "GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\nX-Device-ID: %s\r\n\r\n",
		path, u.Host, base64.StdEncoding.EncodeToString(keyB), o.DeviceMid)
	br := bufio.NewReader(tconn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		tconn.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		tconn.Close()
		return nil, fmt.Errorf("webremote: relay upgrade failed: %s", resp.Status)
	}
	return &client{conn: tconn, br: br, done: make(chan struct{})}, nil
}

func (c *client) send(m relayMsg) {
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	mask := make([]byte, 4)
	rand.Read(mask)
	payload := make([]byte, len(b))
	for i := range b {
		payload[i] = b[i] ^ mask[i%4]
	}
	var hdr []byte
	switch {
	case len(b) < 126:
		hdr = []byte{0x81, 0x80 | byte(len(b))}
	case len(b) < 65536:
		hdr = make([]byte, 4)
		hdr[0], hdr[1] = 0x81, 0x80|126
		binary.BigEndian.PutUint16(hdr[2:], uint16(len(b)))
	default:
		hdr = make([]byte, 10)
		hdr[0], hdr[1] = 0x81, 0x80|127
		binary.BigEndian.PutUint64(hdr[2:], uint64(len(b)))
	}
	_, _ = c.conn.Write(append(append(hdr, mask...), payload...))
}

// readLoop reads text frames and hands them to onMsg until it returns
// false, the socket closes, or a protocol error occurs.
func (c *client) readLoop(onMsg func(relayMsg) bool) {
	for {
		op, payload, err := c.readFrame()
		if err != nil {
			if dbg := os.Getenv("ZQF_RELAY_DEBUG"); dbg != "" {
				fmt.Fprintf(os.Stderr, "webremote debug: read err: %v\n", err)
			}
			c.markClosed()
			return
		}
		switch op {
		case 0x1: // text
			var m relayMsg
			if json.Unmarshal(payload, &m) == nil {
				if !onMsg(m) {
					return
				}
			}
		case 0x8: // close
			c.markClosed()
			return
		case 0x9: // ping -> pong (client frames must carry a mask)
			c.wmu.Lock()
			mask := make([]byte, 4)
			rand.Read(mask)
			_, _ = c.conn.Write(append([]byte{0x8a, 0x80}, mask...))
			c.wmu.Unlock()
		}
	}
}

func (c *client) readFrame() (byte, []byte, error) {
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(c.br, hdr); err != nil {
		return 0, nil, err
	}
	op := hdr[0] & 0x0f
	masked := hdr[1]&0x80 != 0
	var n uint64
	switch hdr[1] & 0x7f {
	case 126:
		ext := make([]byte, 2)
		if _, err := io.ReadFull(c.br, ext); err != nil {
			return 0, nil, err
		}
		n = uint64(binary.BigEndian.Uint16(ext))
	case 127:
		ext := make([]byte, 8)
		if _, err := io.ReadFull(c.br, ext); err != nil {
			return 0, nil, err
		}
		n = binary.BigEndian.Uint64(ext)
	default:
		n = uint64(hdr[1] & 0x7f)
	}
	if n > 1<<22 {
		return 0, nil, errors.New("webremote: frame too large")
	}
	var mask []byte
	if masked {
		mask = make([]byte, 4)
		if _, err := io.ReadFull(c.br, mask); err != nil {
			return 0, nil, err
		}
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return op, payload, nil
}

func (c *client) isClosed() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

func (c *client) markClosed() {
	if !c.isClosed() {
		close(c.done)
	}
}

func (c *client) close() {
	c.markClosed()
	_ = c.conn.Close()
}
