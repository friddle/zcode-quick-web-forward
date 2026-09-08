// Package officialhost runs the OFFICIAL ZCode web-remote host bundle
// (out/host from the desktop app) as a plain Node process and speaks its
// Electron-utilityProcess parentPort protocol over stdio JSON lines.
//
// The desktop forks host/index.js via utilityProcess.fork; the host then talks
// to main over process.parentPort (message events {data, ports}) and attached
// MessagePorts carry the phone's channel traffic. We emulate parentPort and
// MessagePorts in official-host/shim.mjs and drive them from Go:
//
//	Go -> shim: {"t":"pp","msg":{...},"ports":["id",...]}
//	Go -> shim: {"t":"port","id":"I","b64":"..."}
//	shim -> Go: {"t":"pp","msg":{...}}
//	shim -> Go: {"t":"port","id":"I","b64":"..."}
//
// With ZCODE_AGENT_SERVER_COMMAND set the host spawns the engine itself, so
// everything above the app-server (tasks, uploads, automations, file service,
// v4 projection) is the official implementation.
package officialhost

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"
)

type Host struct {
	cmd    *exec.Cmd
	mu     sync.Mutex
	stdin  *json.Encoder
	stdout *bufio.Reader
	exited chan struct{} // closed once cmd.Wait reaps the process
	ready  chan struct{} // closed when the shim reports the host booted

	OnParentPort  func(msg map[string]any)
	OnPortData    func(portID string, value any)
	OnRawPortData func(portID string, b []byte)
	OnLog         func(line string)
}

// Start launches `node shim.mjs` in dir with the current environment.
// Node must be >= 18.
func Start(nodeBin, dir string) (*Host, error) {
	return StartEnv(nodeBin, dir, nil)
}

// StartEnv launches `node shim.mjs` in dir with an explicit environment.
// Without ZCODE_AGENT_SERVER_COMMAND* the host falls back to the desktop's
// default engine command (which does not exist on a bare server) and its
// engine spawn fails — every engine-backed channel then hangs.
func StartEnv(nodeBin, dir string, env []string) (*Host, error) {
	h := &Host{exited: make(chan struct{}), ready: make(chan struct{})}
	cmd := exec.Command(nodeBin, "shim.mjs")
	cmd.Dir = dir
	if len(env) > 0 {
		cmd.Env = env
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = stderrWriter{h}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	h.cmd = cmd
	h.stdin = json.NewEncoder(stdin)
	h.stdout = bufio.NewReaderSize(stdout, 1<<20)
	go func() {
		// reap; without this Alive() lies and the child zombifies. The exit
		// reason matters: a silent host exit is indistinguishable from a
		// wedged one otherwise.
		werr := cmd.Wait()
		fmt.Fprintf(os.Stderr, "[officialhost] host exited: err=%v\n", werr)
		close(h.exited)
	}()
	go h.readLoop()
	return h, nil
}

type stderrWriter struct{ h *Host }

func (w stderrWriter) Write(p []byte) (int, error) {
	if w.h.OnLog != nil {
		w.h.OnLog(string(p))
	}
	return len(p), nil
}

func (h *Host) readLoop() {
	for {
		line, err := h.stdout.ReadString('\n')
		if line != "" {
			// A panicking callback must not take the whole daemon down (a
			// panic in this goroutine crashes the process); log and keep
			// draining so the pipe never backs up.
			func() {
				defer func() {
					if r := recover(); r != nil {
						fmt.Fprintf(os.Stderr, "[officialhost] callback panic: %v\n", r)
					}
				}()
				h.dispatch(line)
			}()
		}
		if err != nil {
			return
		}
	}
}

func (h *Host) dispatch(line string) {
	var m struct {
		T   string         `json:"t"`
		ID  string         `json:"id"`
		B64 string         `json:"b64"`
		Msg map[string]any `json:"msg"`
	}
	if json.Unmarshal([]byte(line), &m) != nil {
		return
	}
	switch m.T {
	case "pp":
		if h.OnParentPort != nil && m.Msg != nil {
			h.OnParentPort(m.Msg)
		}
	case "port":
		var v any
		if raw, err := base64.StdEncoding.DecodeString(m.B64); err == nil {
			_ = json.Unmarshal(raw, &v)
		}
		if h.OnPortData != nil {
			h.OnPortData(m.ID, v)
		}
	case "ready":
		select {
		case <-h.ready:
		default:
			close(h.ready)
		}
	case "raw":
		if raw, err := base64.StdEncoding.DecodeString(m.B64); err == nil && h.OnRawPortData != nil {
			h.OnRawPortData(m.ID, raw)
		}
	}
}

// ParentPort posts a message to the host's process.parentPort; portIDs are
// materialized as the message event's `ports` array.
func (h *Host) ParentPort(msg map[string]any, portIDs ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ids := portIDs
	if ids == nil {
		ids = []string{}
	}
	_ = h.stdin.Encode(map[string]any{"t": "pp", "msg": msg, "ports": ids})
}

// PortData delivers a JSON value into the emulated MessagePort.
func (h *Host) PortData(portID string, value any) {
	raw, err := json.Marshal(value)
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	_ = h.stdin.Encode(map[string]any{
		"t":   "port",
		"id":  portID,
		"b64": base64.StdEncoding.EncodeToString(raw),
	})
}

// WaitReady blocks until the shim reports the host booted (the host's
// parentPort listener only exists after its import completes — messages sent
// earlier are silently dropped). Returns false on timeout or exit.
func (h *Host) WaitReady(timeout time.Duration) bool {
	select {
	case <-h.ready:
		return true
	case <-h.exited:
		return false
	case <-time.After(timeout):
		return false
	}
}

// Alive reports whether the host process is still running.
func (h *Host) Alive() bool {
	select {
	case <-h.exited:
		return false
	default:
		return h.cmd != nil
	}
}

// PortOpen marks an emulated MessagePort started, flushing queued messages to
// the host's listener (the shim queues deliveries until start()).
func (h *Host) PortOpen(portID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	_ = h.stdin.Encode(map[string]any{"t": "port-open", "id": portID})
}

func (h *Host) Stop() {
	if h.cmd != nil && h.cmd.Process != nil {
		_ = h.cmd.Process.Kill()
	}
	<-h.exited
}

// Env gathers the environment for the host process: engine command override,
// ZCode home, label. Extra env vars (BIGMODEL keys etc.) inherit from the
// caller via extras.
func Env(engineCommand string, engineArgs []string, engineCwd, zcodeHome, label string, extras map[string]string) []string {
	env := append(os.Environ(),
		"ZCODE_PROCESS_LABEL="+label,
		"ZCODE_HOME="+zcodeHome,
		"ZCODE_AGENT_SERVER_COMMAND="+engineCommand,
		"ZCODE_AGENT_SERVER_ARGS_JSON="+mustJSON(engineArgs),
		"ZCODE_AGENT_SERVER_CWD="+engineCwd,
	)
	for k, v := range extras {
		env = append(env, k+"="+v)
	}
	return env
}

func mustJSON(v []string) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// RawPortData delivers raw bytes to the emulated MessagePort. The official
// host's service port speaks the binary channel serialization (the same
// wire form as the relay's rpc-frames), not JSON.
func (h *Host) RawPortData(portID string, b []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	_ = h.stdin.Encode(map[string]any{
		"t":   "raw",
		"id":  portID,
		"b64": base64.StdEncoding.EncodeToString(b),
	})
}
