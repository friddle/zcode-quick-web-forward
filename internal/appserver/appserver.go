// Package appserver spawns the official ZCode app-server runtime
// (`node <runtime>/zcode.cjs app-server`) and talks to it over the
// newline-delimited JSON protocol it uses on its stdin/stdout.
package appserver

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Server wraps a running ZCode app-server child process.
type Server struct {
	Cmd     *exec.Cmd
	stdin   io.WriteCloser
	mu      sync.Mutex
	nextID  int
	pending map[int]chan json.RawMessage
	OnLine  func(line string)
	Started bool
	done    chan struct{}
	initErr error
}

// Options configures an app-server launch.
type Options struct {
	RuntimeDir string
	Node       string
	Args       []string
	OnLine     func(line string)
}

var urlRe = regexp.MustCompile(`https?://[A-Za-z0-9._/\-?&=:%#~+{}$]+`)

// FindURLs extracts candidate URLs (login / app-server / remote) from a line.
func FindURLs(line string) []string {
	return urlRe.FindAllString(line, -1)
}

// New starts the app-server process. opts.RuntimeDir is the glm dir that
// contains zcode.cjs (already resolved by the runtime package).
func New(opts Options) (*Server, error) {
	node := opts.Node
	if node == "" {
		node = "node"
	}
	script := opts.RuntimeDir
	if !strings.HasSuffix(script, ".cjs") {
		script = script + "/zcode.cjs"
	}
	args := []string{script, "app-server"}
	args = append(args, opts.Args...)

	cmd := exec.Command(node, args...)
	cmd.Dir = filepathDir(script)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = io.Discard

	s := &Server{
		Cmd:     cmd,
		stdin:   stdin,
		pending: make(map[int]chan json.RawMessage),
		done:    make(chan struct{}),
		OnLine:  opts.OnLine,
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("spawn %s %s: %w", node, script, err)
	}
	s.Started = true

	go s.readLoop(stdout)
	go func() {
		err := cmd.Wait()
		if err != nil {
			select {
			case <-s.done:
			default:
				s.initErr = err
			}
		}
		close(s.done)
	}()
	return s, nil
}

func filepathDir(p string) string {
	if i := strings.LastIndexAny(p, "/\\"); i >= 0 {
		return p[:i]
	}
	return "."
}

func (s *Server) readLoop(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var env struct {
			ID     json.RawMessage `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(line), &env); err == nil {
			var id int
			if json.Unmarshal(env.ID, &id) == nil && id > 0 {
				s.mu.Lock()
				ch, ok := s.pending[id]
				if ok {
					delete(s.pending, id)
				}
				s.mu.Unlock()
				if ok {
					if env.Error != nil {
						ch <- nil
					} else {
						ch <- env.Result
					}
					close(ch)
					continue
				}
			}
		}
		if s.OnLine != nil {
			s.OnLine(line)
		}
	}
}

// Request performs an app-server call and returns the result envelope.
func (s *Server) Request(method string, params any) (json.RawMessage, error) {
	s.mu.Lock()
	s.nextID++
	id := s.nextID
	ch := make(chan json.RawMessage, 1)
	s.pending[id] = ch
	s.mu.Unlock()

	payload, err := json.Marshal(map[string]any{
		"id":     id,
		"method": method,
		"params": params,
	})
	if err != nil {
		return nil, err
	}
	if _, err := s.stdin.Write(append(payload, '\n')); err != nil {
		return nil, err
	}

	select {
	case res := <-ch:
		if res == nil {
			return nil, fmt.Errorf("%s: app-server returned error", method)
		}
		return res, nil
	case <-s.done:
		return nil, fmt.Errorf("app-server exited: %v", s.initErr)
	case <-time.After(120 * time.Second):
		return nil, fmt.Errorf("%s: app-server timeout", method)
	}
}

// Ping reports whether the app-server answers a ping.
func (s *Server) Ping() bool {
	_, err := s.Request("app.ping", map[string]any{})
	if err != nil {
		// Some runtimes accept "ping" or "app-server.status"; try again.
		_, err = s.Request("ping", map[string]any{})
		return err == nil
	}
	return true
}

// Close stops the app-server child.
func (s *Server) Close() error {
	if s.Cmd.Process != nil {
		_ = s.Cmd.Process.Kill()
	}
	return nil
}
