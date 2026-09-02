// Package terminal implements the ZCode "terminal" channel service: it spawns
// a real shell in a pty (matching the desktop host's node-pty implementation)
// and streams output/exit events. The phone drives it via terminal.create /
// terminal.write / terminal.resize / terminal.dispose and listens to
// onDynamicData / onDynamicExit.
package terminal

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/creack/pty"
)

// Service manages terminal instances.
type Service struct {
	mu     sync.Mutex
	next   int
	terms  map[string]*Term
	onData func(listenID int, data string)
	onExit func(listenID, code int)
	// defaultDataListen / defaultExitListen are used when the phone subscribes
	// onDynamicData/onDynamicExit without a terminal id (arg=nil) — the phone
	// attaches a single global listener and routes by the payload we send.
	defaultDataListen int
	defaultExitListen int
}

// Term is one running shell pty.
type Term struct {
	ID         string
	pty        *os.File
	cmd        *exec.Cmd
	DataListen int // EventListen id for onDynamicData
	ExitListen int // EventListen id for onDynamicExit
	done       chan struct{}
	mu         sync.Mutex
}

// New returns a terminal service.
func New() *Service {
	return &Service{next: 0, terms: map[string]*Term{}}
}

// Create spawns a shell in a pty. Returns the terminal descriptor the phone
// expects ({id, shell, fontFamily, fontFamilySource, windowsPty}).
func (s *Service) Create(cols, rows int, cwd string) (map[string]any, error) {
	shell := resolveShell()
	dir := resolveCwd(cwd)
	env := terminalEnv(dir)
	s.mu.Lock()
	s.next++
	id := fmt.Sprintf("%d", s.next-1)
	s.mu.Unlock()

	cmd := exec.Command(shell)
	cmd.Dir = dir
	cmd.Env = env
	if cols < 1 {
		cols = 80
	}
	if rows < 1 {
		rows = 24
	}
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		return nil, fmt.Errorf("failed to start terminal with shell %q in %q: %w", shell, dir, err)
	}

	t := &Term{
		ID:   id,
		pty:  f,
		cmd:  cmd,
		done: make(chan struct{}),
	}
	s.mu.Lock()
	s.terms[id] = t
	s.mu.Unlock()

	go s.pump(t)
	return map[string]any{
		"id":               id,
		"shell":            shell,
		"fontFamily":       "ui-monospace, SFMono-Regular, 'SF Mono', Menlo, Consolas, monospace",
		"fontFamilySource": "fallback",
		"windowsPty":       nil,
	}, nil
}

// pump reads pty output and invokes the data/exit callbacks.
func (s *Service) pump(t *Term) {
	defer close(t.done)
	buf := make([]byte, 4096)
	for {
		n, err := t.pty.Read(buf)
		if n > 0 {
			// node-pty decodes utf8 (lossy for invalid bytes); match it.
			data := strings.ToValidUTF8(string(buf[:n]), "\uFFFD")
			t.mu.Lock()
			dl := t.DataListen
			t.mu.Unlock()
			if dl == 0 {
				s.mu.Lock()
				dl = s.defaultDataListen
				s.mu.Unlock()
			}
			if dl != 0 && s.onData != nil {
				s.onData(dl, data)
			}
		}
		if err != nil {
			break
		}
	}
	// Wait for the process to reap the exit code.
	_ = t.cmd.Wait()
	code := 0
	if t.cmd.ProcessState != nil {
		code = t.cmd.ProcessState.ExitCode()
		if code < 0 {
			code = 128 - code
		}
	}
	t.mu.Lock()
	el := t.ExitListen
	t.mu.Unlock()
	if el == 0 {
		s.mu.Lock()
		el = s.defaultExitListen
		s.mu.Unlock()
	}
	if el != 0 && s.onExit != nil {
		s.onExit(el, code)
	}
	s.mu.Lock()
	delete(s.terms, t.ID)
	s.mu.Unlock()
	_ = t.pty.Close()
}

// OnData / OnExit are wired by the caller to push EventFire frames.
func (s *Service) SetCallbacks(onData func(listenID int, data string), onExit func(listenID, code int)) {
	s.mu.Lock()
	s.onData = onData
	s.onExit = onExit
	s.mu.Unlock()
}

// Write sends user input to the terminal.
func (s *Service) Write(id, data string) error {
	t, err := s.get(id)
	if err != nil {
		return err
	}
	_, err = t.pty.WriteString(data)
	return err
}

// Resize updates the pty window size.
func (s *Service) Resize(id string, cols, rows int) error {
	t, err := s.get(id)
	if err != nil {
		return err
	}
	if cols < 1 {
		cols = 80
	}
	if rows < 1 {
		rows = 24
	}
	return pty.Setsize(t.pty, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}

// SetDataListener records the EventListen id for onDynamicData. If the phone
// subscribed without a terminal id (arg=nil) it applies as a default for all
// terminals; with an id it applies to just that terminal.
func (s *Service) SetDataListener(id string, listenID int) error {
	if id == "" {
		s.mu.Lock()
		s.defaultDataListen = listenID
		s.mu.Unlock()
		return nil
	}
	t, err := s.get(id)
	if err != nil {
		return err
	}
	t.mu.Lock()
	t.DataListen = listenID
	t.mu.Unlock()
	return nil
}

// SetExitListener records the EventListen id for onDynamicExit.
func (s *Service) SetExitListener(id string, listenID int) error {
	if id == "" {
		s.mu.Lock()
		s.defaultExitListen = listenID
		s.mu.Unlock()
		return nil
	}
	t, err := s.get(id)
	if err != nil {
		return err
	}
	t.mu.Lock()
	t.ExitListen = listenID
	t.mu.Unlock()
	return nil
}

// Dispose kills a terminal.
func (s *Service) Dispose(id string) {
	s.mu.Lock()
	t := s.terms[id]
	s.mu.Unlock()
	if t == nil {
		return
	}
	_ = t.cmd.Process.Kill()
	s.mu.Lock()
	delete(s.terms, id)
	s.mu.Unlock()
}

func (s *Service) get(id string) (*Term, error) {
	s.mu.Lock()
	t := s.terms[id]
	s.mu.Unlock()
	if t == nil {
		return nil, fmt.Errorf("Terminal not found: %s", id)
	}
	return t, nil
}

func resolveShell() string {
	if v := os.Getenv("SHELL"); v != "" {
		if executable(v) {
			return v
		}
	}
	for _, c := range []string{"/bin/zsh", "/bin/bash", "/bin/sh"} {
		if executable(c) {
			return c
		}
	}
	return "/bin/sh"
}

func executable(p string) bool {
	if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
		return true
	}
	return false
}

func resolveCwd(cwd string) string {
	cands := []string{cwd, os.Getenv("HOME"), "/"}
	if h, err := os.UserHomeDir(); err == nil {
		cands = append(cands, h)
	}
	for _, c := range cands {
		if c != "" {
			if fi, err := os.Stat(c); err == nil && fi.IsDir() {
				return c
			}
		}
	}
	return "/"
}

func terminalEnv(cwd string) []string {
	env := os.Environ()
	haveTERM, haveCOLOR, haveLANG := false, false, false
	out := []string{}
	for _, e := range env {
		switch {
		case strings.HasPrefix(e, "TERM="):
			out = append(out, "TERM=xterm-256color")
			haveTERM = true
		case strings.HasPrefix(e, "COLORTERM="):
			out = append(out, "COLORTERM=truecolor")
			haveCOLOR = true
		case strings.HasPrefix(e, "LANG=") || strings.HasPrefix(e, "LC_CTYPE="):
			out = append(out, e)
			if strings.Contains(strings.ToLower(e), "utf") {
				haveLANG = true
			}
		case strings.HasPrefix(e, "TMUX") || strings.HasPrefix(e, "STY=") ||
			strings.HasPrefix(e, "WINDOW") || strings.HasPrefix(e, "TERMCAP") ||
			strings.HasPrefix(e, "COLUMNS=") || strings.HasPrefix(e, "LINES="):
			// drop terminal-polluting vars
		default:
			out = append(out, e)
		}
	}
	if !haveTERM {
		out = append(out, "TERM=xterm-256color")
	}
	if !haveCOLOR {
		out = append(out, "COLORTERM=truecolor")
	}
	if !haveLANG {
		out = append(out, "LANG=C.UTF-8")
	}
	out = append(out, "PWD="+cwd)
	return out
}
