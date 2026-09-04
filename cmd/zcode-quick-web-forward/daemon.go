// `remote` command front-end plus the background daemon: stop/foreground
// flag handling, pid-file lifecycle, self re-exec (setsid, log redirect)
// and the r.log tail loop.

package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func doRemote(args []string) {
	// daemon/foreground/stop handling. Default (no flag): run as a background
	// daemon (no tmux needed) and tail the log to stdout.
	if hasFlag(args, "--stop", "-s") {
		stopDaemon()
		return
	}
	foreground := hasFlag(args, "--foreground", "-f", "--fg")
	logPath := flagValue(args, "--log", "")
	args = stripFlags(args, "--stop", "-s", "--foreground", "-f", "--fg", "--log")
	if foreground {
		doRemoteOpts(parseCommon(args))
		return
	}
	daemonMain(args, logPath)
}

// --- daemon / log tail -------------------------------------------------

func zqfLogPath(hint string) string {
	if hint != "" {
		return hint
	}
	if v := os.Getenv("ZQF_LOG"); v != "" {
		return v
	}
	if dir, err := os.Getwd(); err == nil {
		return filepath.Join(dir, "r.log")
	}
	return "r.log"
}

func pidPath(log string) string { return log + ".pid" }

func daemonPID(log string) int {
	b, err := os.ReadFile(pidPath(log))
	if err != nil {
		return 0
	}
	var pid int
	if _, err := fmt.Sscanf(string(b), "%d", &pid); err != nil {
		return 0
	}
	if pid <= 0 {
		return 0
	}
	if !processAlive(pid) {
		return 0
	}
	return pid
}

// processAlive reports whether pid is a live process. A zombie still answers
// Signal(0) on Linux, which made stop/start cycles wedge ("daemon already
// running" against a reaped-pending corpse whose parent never waits) — check
// the /proc state and treat Z as gone. Without /proc (darwin) Signal(0) is the
// best available probe.
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if err := p.Signal(syscall.Signal(0)); err != nil {
		return false
	}
	if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
		if fields := strings.Fields(string(b)); len(fields) > 2 && fields[2] == "Z" {
			return false
		}
	}
	return true
}

func stopDaemon() {
	// The log may live under several conventional locations; try the current
	// dir r.log plus the cache dir.
	candidates := []string{zqfLogPath("")}
	if cache, err := os.UserCacheDir(); err == nil {
		candidates = append(candidates, filepath.Join(cache, "zcode-quick-web-forward", "remote.log"))
	}
	seen := map[string]bool{}
	stopped := false
	for _, l := range candidates {
		if seen[l] {
			continue
		}
		seen[l] = true
		if pid := daemonPID(l); pid != 0 {
			_ = syscall.Kill(pid, syscall.SIGTERM)
			fmt.Printf("zcode: daemon %d stopped (log %s)\n", pid, l)
			stopped = true
		}
	}
	if !stopped {
		fmt.Println("zcode: no running daemon found")
	}
}

// daemonMain re-execs this binary in the background (detached from the tty and
// the current ssh session), redirects output to the log, then tails the log.
func daemonMain(args []string, logHint string) {
	log := zqfLogPath(logHint)
	if pid := daemonPID(log); pid != 0 {
		fmt.Printf("zcode: daemon already running pid=%d (log %s)\n", pid, log)
		tailLog(log, true)
		return
	}
	_ = os.MkdirAll(filepath.Dir(filepath.Clean(log)), 0o755)
	// Child: re-exec self with a marker env so the child knows to run the
	// engine+relay in the foreground (it inherits a redirect to the log).
	exe, err := os.Executable()
	if err != nil {
		fatal("daemonize: %v", err)
	}
	childArgs := append([]string{"remote", "--foreground"}, args...)
	child := exec.Command(exe, childArgs...)
	child.Env = append(os.Environ(), "ZQF_DAEMON_CHILD=1", "ZQF_LOG="+log)
	// Detach: new session, stdin /dev/null, stdout+stderr -> log file.
	child.Stdin = nil
	f, err := os.OpenFile(log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fatal("daemonize log: %v", err)
	}
	defer f.Close()
	child.Stdout = f
	child.Stderr = f
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := child.Start(); err != nil {
		fatal("daemonize start: %v", err)
	}
	if err := os.WriteFile(pidPath(log), []byte(fmt.Sprintf("%d\n", child.Process.Pid)), 0o644); err != nil {
		fmt.Printf("zcode: warn: cannot write pid file: %v\n", err)
	}
	fmt.Printf("zcode: daemon started pid=%d (log %s)\n", child.Process.Pid, log)
	_ = child.Process.Release()
	tailLog(log, false)
}

// tailLog prints the pairing URL / log as it appears, like `tail -f`.
// fromEnd: when re-attaching to an already-running daemon, start at the tail so
// we don't replay the whole log from the beginning. The tail exits once the
// daemon behind the pid file is gone — the tailer is usually the daemon
// child's parent, so exiting also lets the kernel reap the child instead of
// wedging pid-file checks with a zombie.
func tailLog(path string, fromEnd bool) {
	fmt.Printf("zcode: tailing %s (ctrl-c to stop tailing; daemon keeps running)\n", path)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		fatal("tail log: %v", err)
	}
	defer f.Close()
	watched := daemonPID(path)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		fmt.Println("\nzcode: tail stopped (daemon still running; use --stop to end it)")
		os.Exit(0)
	}()
	st, _ := f.Stat()
	off := int64(0)
	if fromEnd {
		// Skip the last full-line boundary before the final 8KB so we still
		// show the most recent startup banner / activity without replaying
		// the entire history.
		off = st.Size() - 8192
		if off < 0 {
			off = 0
		}
	}
	printNew := func() {
		cur, err := f.Stat()
		if err != nil {
			return
		}
		if cur.Size() <= off {
			return
		}
		buf := make([]byte, cur.Size()-off)
		n, _ := f.ReadAt(buf, off)
		off += int64(n)
		os.Stdout.Write(buf)
	}
	printNew()
	for {
		time.Sleep(400 * time.Millisecond)
		printNew()
		// No daemon to wait for (no pid file): tail until ctrl-c like before.
		if watched != 0 && daemonPID(path) == 0 {
			fmt.Println("\nzcode: daemon exited, tail stopped")
			return
		}
	}
}

func hasFlag(args []string, names ...string) bool {
	for _, a := range args {
		for _, n := range names {
			if a == n {
				return true
			}
		}
	}
	return false
}

func flagValue(args []string, name, def string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(a, name+"=") {
			return strings.TrimPrefix(a, name+"=")
		}
	}
	return def
}

func stripFlags(args []string, names ...string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		skip := false
		for _, n := range names {
			if args[i] == n {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		// also skip value of --log
		if args[i] == "--log" {
			i++
			continue
		}
		out = append(out, args[i])
	}
	return out
}
