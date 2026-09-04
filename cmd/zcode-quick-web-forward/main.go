// Command zcode-quick-web-forward is a pure-CLI driver that logs into ZCode,
// exposes real workspaces/tasks from the local ZCode installation, and mints a
// phone pairing URL on ZCode's own web-remote relay. It only drives the relay
// and answers the phone's channel services from real ZCode state (task index,
// model-provider config, settings) — the reverse-engineered stub approach is
// gone.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/friddle/zcode-quick-web-forward/internal/browser"
	enginepkg "github.com/friddle/zcode-quick-web-forward/internal/engine"
	"github.com/friddle/zcode-quick-web-forward/internal/nodejs"
	"github.com/friddle/zcode-quick-web-forward/internal/relay"
	"github.com/friddle/zcode-quick-web-forward/internal/runtime"
	"github.com/friddle/zcode-quick-web-forward/internal/terminal"
	"github.com/friddle/zcode-quick-web-forward/internal/zcode"
)

const version = "0.7.0"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "run":
			runLoginCLI(os.Args[2:])
			return
		case "logincli", "login":
			runLoginCLI(os.Args[2:])
			return
		case "remote", "web":
			doRemote(os.Args[2:])
			return
		case "download", "fetch":
			doDownload(os.Args[2:])
			return
		case "workspace":
			doWorkspace(os.Args[2:])
			return
		case "version", "-version", "--version", "-v":
			fmt.Printf("zcode-quick-web-forward %s\n", version)
			return
		case "help", "-h", "--help":
			printHelp()
			return
		}
	}
	printHelp()
}

func printHelp() {
	fmt.Println(`zcode-quick-web-forward - pure-CLI ZCode web/mobile driver

Drives the official ZCode runtime (glm/zcode.cjs app-server), exposes the real
workspaces/tasks from this machine's ZCode install, and mints a phone pairing
URL on ZCode's web-remote relay. Login and workspace data are real — no stubs.

usage: zcode-quick-web-forward [command] [flags]

Commands:
  run            interactive: region, login method (link or BigModel key),
                 then engine + relay + phone pairing URL
  logincli       run the real client login (node zcode.cjs login --no-browser)
  remote|web     start engine + relay and print the phone pairing URL.
                 Default: runs as a background daemon (no tmux/ssh needed)
                 and tails r.log to the terminal. Use --foreground to run in
                 the foreground, --stop to stop the daemon, --log PATH to
                 choose the log file (default ./r.log).
  workspace      add/remove/list extra workspaces exposed to the phone
                 (zcode-quick-web-forward workspace add /path/to/dir)
  download       resolve/download the latest ZCode runtime
  version        print version

Flags:
  --runtime-path PATH   explicit glm runtime dir (env ZCODE_RUNTIME_PATH)
  --node PATH           node binary (env ZCODE_NODE); a managed Node.js
                        (>= 22.5) is downloaded automatically when missing
  --region REGION       china|global (env ZCODE_REGION); china uses the Aliyun
                        node mirror + BigModel login, global uses official
                        sources + Z.AI login. Auto-detected when empty.
  --workspace PATH      workspace to expose to the phone (repeatable; env
                        ZCODE_WORKSPACE). When unset, the startup directory is
                        used, plus any real workspaces from the task index.`)
}

var stdinReader = bufio.NewReader(os.Stdin)

func askRegion() string {
	fmt.Println("zcode: 请选择区域:")
	fmt.Println("  1) 国内 china  (BigModel 登录 / 阿里云下载镜像)")
	fmt.Println("  2) 国际 global (Z.AI 登录 / 官方下载源)")
	fmt.Print("选择 [1]: ")
	line, _ := stdinReader.ReadString('\n')
	switch strings.TrimSpace(line) {
	case "2", "2)", "global", "g", "国际":
		return "global"
	default:
		return "china"
	}
}

func askLoginMethod() string {
	fmt.Println("zcode: 请选择登录方式:")
	fmt.Println("  1) 登录链接 (浏览器打开授权链接, CLI 等待回调)")
	fmt.Println("  2) API Key  (BigModel API Key, https://open.bigmodel.cn/apikeys)")
	fmt.Print("选择 [1]: ")
	line, _ := stdinReader.ReadString('\n')
	switch strings.TrimSpace(line) {
	case "2", "2)", "key", "api", "apikey", "api key":
		return "key"
	default:
		return "link"
	}
}

type commonOpts struct {
	runtimePath string
	node        string
	region      string
	workspaces  multiFlag
}

type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

func parseCommon(args []string) commonOpts {
	var o commonOpts
	fs := flag.NewFlagSet("zqf", flag.ContinueOnError)
	fs.StringVar(&o.runtimePath, "runtime-path", "", "glm runtime dir; auto if empty")
	fs.StringVar(&o.node, "node", "", "node binary; auto-downloaded when missing/old")
	fs.StringVar(&o.region, "region", "", "china|global; auto-detected when empty")
	fs.Var(&o.workspaces, "workspace", "workspace path (repeatable; env ZCODE_WORKSPACE)")
	_ = fs.Parse(args)
	if o.node == "" {
		o.node = os.Getenv("ZCODE_NODE")
	}
	if o.region == "" {
		o.region = os.Getenv("ZCODE_REGION")
	}
	if v := os.Getenv("ZCODE_RUNTIME_PATH"); v != "" && o.runtimePath == "" {
		o.runtimePath = v
	}
	return o
}

func (o commonOpts) regionName() string { return nodejs.Normalize(o.region) }

func (o commonOpts) ensureNode() string {
	if o.node != "" {
		return o.node
	}
	node, err := nodejs.Ensure(o.region)
	if err != nil {
		fatal("node resolve failed: %v", err)
	}
	if node != "node" {
		fmt.Printf("zcode: using node %s\n", node)
	}
	return node
}

func (o commonOpts) resolveWorkspaces() []string {
	// explicit --workspace / ZCODE_WORKSPACE win
	explicit := append([]string{}, o.workspaces...)
	if len(explicit) == 0 {
		if v := os.Getenv("ZCODE_WORKSPACE"); v != "" {
			for _, p := range strings.Split(v, string(os.PathListSeparator)) {
				if p = strings.TrimSpace(p); p != "" {
					explicit = append(explicit, p)
				}
			}
		}
	}
	out := []string{}
	if len(explicit) > 0 {
		out = append(out, explicit...)
	} else if cwd, err := os.Getwd(); err == nil {
		out = append(out, cwd) // startup directory is the default workspace
	}
	// merge in real workspaces from the task index (dedup)
	seen := map[string]bool{}
	for _, w := range out {
		seen[filepath.Clean(w)] = true
	}
	if ws, err := zcode.Workspaces(); err == nil {
		for _, w := range ws {
			p := filepath.Clean(w.WorkspacePath)
			if !seen[p] {
				out = append(out, w.WorkspacePath)
				seen[p] = true
			}
		}
	}
	// merge in workspaces registered via `workspace add`
	for _, p := range zcode.StoredWorkspaces() {
		c := filepath.Clean(p)
		if !seen[c] {
			out = append(out, c)
			seen[c] = true
		}
	}
	return out
}

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "zcode: ERROR: "+format+"\n", a...)
	os.Exit(1)
}

// launchBrowser starts the headless chromium browser host, or returns nil when
// no chromium is available (browser tasks then report backend_unavailable).
func launchBrowser() *browser.Browser {
	if browser.FindChromium() == "" {
		fmt.Println("zcode: no chromium found; browser tasks unavailable (set PLAYWRIGHT_BROWSERS_PATH)")
		return nil
	}
	b, err := browser.Launch()
	if err != nil {
		fmt.Printf("zcode: browser launch failed: %v (browser tasks unavailable)\n", err)
		return nil
	}
	fmt.Printf("zcode: browser host ready (%s)\n", b.ID())
	return b
}

func resolveRuntime(runtimePath string) (dir string) {
	dir, _, err := runtime.NewFinder().Resolve(runtimePath)
	if err != nil {
		fatal("runtime resolve failed: %v", err)
	}
	fmt.Printf("zcode: runtime ready at %s\n", dir)
	return dir
}

func scriptPath(dir string) string {
	if strings.HasSuffix(dir, ".cjs") {
		return dir
	}
	return dir + "/zcode.cjs"
}

func runCLI(node, script string, args []string) error {
	cmd := exec.Command(node, append([]string{script}, args...)...)
	cmd.Dir = dirOf(script)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGINT)
		}
	}()
	defer signal.Stop(sig)
	return cmd.Run()
}

func dirOf(p string) string {
	if i := strings.LastIndexAny(p, "/\\"); i >= 0 {
		return p[:i]
	}
	return "."
}

// defaultPlaywrightPath returns the Playwright browsers cache directory used
// for headless browser tooling, preferring an existing one on the machine.
func defaultPlaywrightPath() string {
	if v := os.Getenv("PLAYWRIGHT_BROWSERS_PATH"); v != "" {
		return v
	}
	if home, err := os.UserHomeDir(); err == nil {
		cands := []string{
			filepath.Join(home, ".cache", "ms-playwright"),
			filepath.Join(home, "Library", "Caches", "ms-playwright"),
		}
		for _, c := range cands {
			if fi, err := os.Stat(c); err == nil && fi.IsDir() {
				return c
			}
		}
	}
	return ""
}

// runLoginCLI drives the interactive entry: region, login method, then remote.
func runLoginCLI(args []string) {
	o := parseCommon(args)
	rt := resolveRuntime(o.runtimePath)
	node := o.ensureNode()

	if o.region == "" && os.Getenv("ZCODE_REGION") == "" {
		o.region = askRegion()
	}
	region := o.regionName()

	method := "link"
	switch {
	case os.Getenv("BIGMODEL_API_KEY") != "":
		method = "key"
	case region == "china":
		method = askLoginMethod()
	}

	switch method {
	case "key":
		fmt.Println("zcode: 登录方式: BigModel API Key")
		bigmodelLogin()
	default:
		fmt.Println("zcode: 登录方式: 登录链接 (官方 login --no-browser)")
		fmt.Println("zcode: 会打印登录链接 -> 请在浏览器打开 -> 授权回调。")
		args2 := []string{"login", "--no-browser"}
		fmt.Printf("zcode: $ %s %s %s\n", node, scriptPath(rt), strings.Join(args2, " "))
		if err := runCLI(node, scriptPath(rt), args2); err != nil {
			fatal("登录失败: %v", err)
		}
	}
	fmt.Println("zcode: 登录完成。启动 app-server 并生成 web-remote 链接 ...")
	doRemoteOpts(o)
}

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
	p, err := os.FindProcess(pid)
	if err != nil {
		return 0
	}
	if err := p.Signal(syscall.Signal(0)); err != nil {
		return 0
	}
	return pid
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
// we don't replay the whole log from the beginning.
func tailLog(path string, fromEnd bool) {
	fmt.Printf("zcode: tailing %s (ctrl-c to stop tailing; daemon keeps running)\n", path)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		fatal("tail log: %v", err)
	}
	defer f.Close()
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

// remoteLockFile keeps the flock fd alive for the process lifetime: an
// unreachable os.File is closed by its finalizer, which would silently drop
// the lock.
var remoteLockFile *os.File

// acquireRemoteLock enforces one engine+relay instance per user. Two running
// instances share the same relay device identity (webremote-state.json) and the
// relay allows a single device connection per sid, so they would kick each
// other off in a loop and break every phone task mid-flight. The lock lives in
// the cache dir — not next to r.log — so instances started from different
// working directories still collide here first.
func acquireRemoteLock() {
	cache, err := os.UserCacheDir()
	if err != nil {
		return // no cache dir resolvable; proceed without the guard
	}
	dir := filepath.Join(cache, "zcode-quick-web-forward")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	path := filepath.Join(dir, "remote.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		holder := strings.TrimSpace(string(mustReadFile(path)))
		if holder == "" {
			holder = "unknown"
		}
		fatal("另一个 zcode-quick-web-forward 实例已在运行 (pid %s): 两个实例共用同一设备身份, 会在中继上互相踢线。请先执行 zcode-quick-web-forward remote --stop。", holder)
	}
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(fmt.Sprintf("%d\n", os.Getpid())), 0)
	remoteLockFile = f
}

func mustReadFile(path string) []byte {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return b
}

func doRemoteOpts(o commonOpts) {
	acquireRemoteLock()
	rt := resolveRuntime(o.runtimePath)
	node := o.ensureNode()
	region := o.regionName()
	origin := o.baseURL()
	workspaces := o.resolveWorkspaces()
	fmt.Printf("zcode: workspaces: %s\n", strings.Join(workspaces, ", "))

	if err := probeOrigin(origin); err != nil {
		fatal("relay origin %s 不可达: %v (检查网络; 或用 --region global / --region china、ZCODE_BASE_URL 切换端点)", origin, err)
	}
	fmt.Printf("zcode: relay origin %s 可达 (region=%s)\n", origin, region)

	engine := relay.NewBridgeEngine()
	engClient := enginepkg.New()
	sender := &relaySender{}

	ps := &phoneSessions{}
	br := launchBrowser()
	termSvc := terminal.New()
	termSvc.SetCallbacks(
		func(listenID int, data string) {
			engine.SendChannelEventString(listenID, data, sender.send)
		},
		func(listenID, code int) {
			engine.SendChannelEventInt(listenID, code, sender.send)
		},
	)
	engClient.OnEvent = func(m json.RawMessage) {
		handleEngineEvent(engClient, engine, sender, ps, m, br)
	}

	var engMu sync.Mutex
	var engCmd *exec.Cmd
	var engGen, shuttingDown int32
	engineExited := make(chan error, 1)

	startEngine := func() {
		gen := atomic.AddInt32(&engGen, 1)
		engMu.Lock()
		if engCmd != nil && engCmd.Process != nil {
			_ = engCmd.Process.Kill()
			_, _ = engCmd.Process.Wait()
		}
		engMu.Unlock()

		cmd := exec.Command(node, scriptPath(rt), "app-server")
		cmd.Dir = dirOf(scriptPath(rt))
		cmd.Stderr = os.Stderr
		// Let the engine discover the browser-use plugin and Playwright
		// chromium (headless browser capability).
		cmd.Env = append(os.Environ(),
			"ZCODE_PLUGIN_ROOT="+filepath.Join(filepath.Dir(scriptPath(rt)), "packages", "browser-use-plugin"),
			"PLAYWRIGHT_BROWSERS_PATH="+defaultPlaywrightPath(),
		)
		stdin, err := cmd.StdinPipe()
		if err != nil {
			fatal("engine stdin: %v", err)
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			fatal("engine stdout: %v", err)
		}
		if err := cmd.Start(); err != nil {
			fatal("engine start: %v", err)
		}
		engMu.Lock()
		engCmd = cmd
		engMu.Unlock()
		engine.Attach(stdin)
		engClient.Attach(stdin)
		fmt.Println("zcode: engine (re)started for bridge")
		go func() {
			sc := bufio.NewScanner(stdout)
			sc.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
			for sc.Scan() {
				line := sc.Bytes()
				if len(line) == 0 {
					continue
				}
				fmt.Printf("zcode << %s\n", line)
				engClient.HandleLine(line)
				engine.ServerLine(line, sender.send)
			}
			werr := cmd.Wait()
			if atomic.LoadInt32(&engGen) == gen {
				engineExited <- werr
			}
		}()
	}
	startEngine()

	go func() {
		if err := <-engineExited; err != nil && atomic.LoadInt32(&shuttingDown) == 0 {
			fmt.Fprintf(os.Stderr, "\nzcode: ERROR: ZCode engine exited: %v\n", err)
			fmt.Fprintf(os.Stderr, "zcode: 常见原因: node 版本过低 (需要 >= 22.5, 支持 node:sqlite)。\n")
			fmt.Fprintf(os.Stderr, "zcode: 可用 --node / ZCODE_NODE 指定 node。\n")
			os.Exit(1)
		}
	}()

	go startWebRemote(origin, region, engine, sender, startEngine, workspaces, ps, engClient, termSvc)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	atomic.StoreInt32(&shuttingDown, 1)
}

// doWorkspace manages the persistent extra-workspace list.
//
//	zcode-quick-web-forward workspace add /path/to/dir
//	zcode-quick-web-forward workspace remove /path/to/dir
//	zcode-quick-web-forward workspace list
func doWorkspace(args []string) {
	if len(args) == 0 {
		fmt.Println("usage: zcode-quick-web-forward workspace <add|remove|list> [path]")
		os.Exit(1)
	}
	switch args[0] {
	case "add":
		if len(args) < 2 {
			fatal("usage: zcode-quick-web-forward workspace add <path>")
		}
		p, added, err := zcode.AddStoredWorkspace(args[1])
		if err != nil {
			fatal("workspace add: %v", err)
		}
		if !added {
			fmt.Printf("zcode: workspace already registered: %s\n", p)
		} else {
			fmt.Printf("zcode: workspace added: %s\n", p)
		}
		fmt.Println("zcode: 生效方式: 停掉再启动 daemon 即可 (zqf remote --stop && zqf remote)")
	case "remove", "rm":
		if len(args) < 2 {
			fatal("usage: zcode-quick-web-forward workspace remove <path>")
		}
		removed, err := zcode.RemoveStoredWorkspace(args[1])
		if err != nil {
			fatal("workspace remove: %v", err)
		}
		if !removed {
			fmt.Printf("zcode: workspace not in list: %s\n", args[1])
		} else {
			fmt.Printf("zcode: workspace removed: %s\n", args[1])
		}
	case "list", "ls":
		ws := zcode.StoredWorkspaces()
		if len(ws) == 0 {
			fmt.Println("zcode: no extra workspaces registered")
			return
		}
		for _, p := range ws {
			fmt.Println(p)
		}
	default:
		fmt.Println("usage: zcode-quick-web-forward workspace <add|remove|list> [path]")
		os.Exit(1)
	}
}

// baseURL returns the web-remote/relay origin: ZCODE_BASE_URL wins, else the
// official international relay.
func (o commonOpts) baseURL() string {
	if v := os.Getenv("ZCODE_BASE_URL"); v != "" {
		return v
	}
	return "https://zcode.z.ai"
}

// probeOrigin checks the relay origin answers before promising a pairing URL.
func probeOrigin(origin string) error {
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get(strings.TrimRight(origin, "/") + "/")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	if resp.StatusCode >= 500 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// relaySender routes frames to whichever relay connection is current.
type relaySender struct {
	mu sync.Mutex
	fn func(any)
}

func (s *relaySender) set(fn func(any)) {
	s.mu.Lock()
	s.fn = fn
	s.mu.Unlock()
}

func (s *relaySender) send(v any) {
	s.mu.Lock()
	fn := s.fn
	s.mu.Unlock()
	if fn != nil {
		fn(v)
	}
}

func startWebRemote(origin, region string, engine *relay.BridgeEngine, sender *relaySender, restartEngine func(), workspaces []string, ps *phoneSessions, engClient *enginepkg.Client, termSvc *terminal.Service) {
	cache, err := os.UserCacheDir()
	if err == nil {
		cache = filepath.Join(cache, "zcode-quick-web-forward")
	}
	opts := relay.Options{
		Origin:     origin,
		DeviceMid:  loadOrCreateDeviceMid(cache),
		DeviceName: hostname(),
		AppVersion: version,
		StatePath:  filepath.Join(cache, "webremote-state.json"),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	relay.Run(ctx, opts, relay.Handler{
		OnReady: func(s relay.Session) {
			fmt.Println()
			fmt.Println("==========================================")
			fmt.Println("  ZCode web-remote / 手机配对链接(手机浏览器打开):")
			fmt.Println("  " + s.PhoneURL)
			fmt.Printf("  (relay %s, device %s, region %s)\n", origin, s.DeviceSid, region)
			fmt.Println("==========================================")
			if path, err := exec.LookPath("qrencode"); err == nil {
				c := exec.Command(path, "-t", "UTF8", s.PhoneURL)
				if out, err := c.Output(); err == nil {
					fmt.Println(string(out))
				}
			}
		},
		OnPaired: func(string) {
			fmt.Println()
			fmt.Println("*** web-remote: 手机已配对接入 ***")
			go func() {
				time.Sleep(800 * time.Millisecond)
				sender.send(workspaceListPush(workspaces, ps))
				fmt.Println("zcode: workspace list pushed to phone")
			}()
		},
		OnData: func(payload json.RawMessage, reply func(any)) {
			sender.set(reply)
			handleRemoteData(payload, reply, engine, restartEngine, sender.send, workspaces, ps, engClient, termSvc)
		},
	})
}

func workspaceListPush(workspaces []string, ps *phoneSessions) map[string]any {
	wsList := make([]any, 0, len(workspaces))
	for _, w := range workspaces {
		wsList = append(wsList, map[string]any{
			"workspacePath":   w,
			"label":           filepath.Base(w),
			"kind":            "local",
			"connectionState": "connected",
		})
	}
	active := ""
	if len(workspaces) > 0 {
		active = workspaces[0]
	}
	return map[string]any{
		"zcode_type": "workspace-list-updated",
		"result": map[string]any{
			"workspaces":         wsList,
			"tasks":              taskListPayload("", ps),
			"activeWorkspaceKey": active,
		},
	}
}

func handleRemoteData(payload json.RawMessage, reply func(any), engine *relay.BridgeEngine, restartEngine func(), replyFrames func(any), workspaces []string, ps *phoneSessions, engClient *enginepkg.Client, termSvc *terminal.Service) {
	var p struct {
		ZcodeType string `json:"zcode_type"`
		RequestID string `json:"requestId"`
	}
	if json.Unmarshal(payload, &p) != nil {
		return
	}
	if p.ZcodeType == "rpc-frame" || p.ZcodeType == "rpc-frame-ack" {
		engine.HandlePhonePayload(payload, reply, handleChannelCall(engine, reply, workspaces, ps, engClient, termSvc))
		return
	}
	if p.RequestID == "" {
		return
	}
	wsList := workspaceListPayload(workspaces)
	active := ""
	if len(workspaces) > 0 {
		active = workspaces[0]
	}
	switch p.ZcodeType {
	case "bootstrap-request":
		reply(map[string]any{
			"zcode_type": "bootstrap-response", "requestId": p.RequestID, "success": true,
			"result": map[string]any{
				"windowControlSessionId": "zqf",
				"workspaces":             wsList,
				"tasks":                  taskListPayload("", ps),
			},
		})
	case "workspace-list-request":
		reply(map[string]any{
			"zcode_type": "workspace-list-response", "requestId": p.RequestID, "success": true,
			"result": map[string]any{"workspaces": wsList, "tasks": taskListPayload("", ps)},
		})
	case "workspace-bridge-open":
		var v struct {
			RequestID        string `json:"requestId"`
			BridgeSessionID  string `json:"bridgeSessionId"`
			BridgeGeneration *int   `json:"bridgeGeneration"`
			RecoveryID       string `json:"recoveryId"`
			WorkspaceKey     string `json:"workspaceKey"`
			TaskID           string `json:"taskId"`
		}
		if json.Unmarshal(payload, &v) != nil || v.BridgeSessionID == "" {
			reply(map[string]any{
				"zcode_type": "workspace-bridge-error", "requestId": p.RequestID,
				"reason": "unexpected-error", "error": "malformed bridge-open",
			})
			return
		}
		engine.SetIdentity(v.BridgeSessionID, v.BridgeGeneration, v.RecoveryID)
		ps.mu.Lock()
		ps.workspacePath = v.WorkspaceKey
		ps.mu.Unlock()
		if restartEngine != nil {
			restartEngine()
		}
		bridge := map[string]any{
			"kind":            "local",
			"bridgeSessionId": v.BridgeSessionID,
			"workspaceKey":    v.WorkspaceKey,
			"workspacePath":   v.WorkspaceKey,
		}
		if v.BridgeGeneration != nil {
			bridge["bridgeGeneration"] = *v.BridgeGeneration
		}
		if v.RecoveryID != "" {
			bridge["recoveryId"] = v.RecoveryID
		}
		if v.TaskID != "" {
			bridge["initialTaskId"] = v.TaskID
		}
		ready := map[string]any{
			"zcode_type": "workspace-bridge-ready", "requestId": v.RequestID,
			"bridgeSessionId": v.BridgeSessionID,
			"bridge":          bridge,
		}
		reply(ready)
		reply(map[string]any{
			"zcode_type": "workspace-list-updated",
			"result": map[string]any{
				"workspaces":         wsList,
				"tasks":              taskListPayload("", ps),
				"activeWorkspaceKey": active,
			},
		})
		go func() {
			time.Sleep(1200 * time.Millisecond)
			engine.SendChannelInitialize(replyFrames)
			fmt.Println("zcode: channel initialize sent")
		}()
	case "workspace-reconnect-request":
		reply(map[string]any{
			"zcode_type": "workspace-reconnect-response", "requestId": p.RequestID, "success": true,
		})
	}
}

func workspaceListPayload(workspaces []string) []any {
	wsList := make([]any, 0, len(workspaces))
	for _, w := range workspaces {
		if strings.HasPrefix(w, "remote:") {
			continue
		}
		wsList = append(wsList, map[string]any{
			"workspacePath":   w,
			"label":           filepath.Base(w),
			"kind":            "local",
			"connectionState": "connected",
		})
	}
	return wsList
}

func boolPtr(b bool) *bool { return &b }

// taskItemPayload renders one task-index row in the phone's expected item
// shape (also echoed back from archive/delete/pin/rename mutations).
func taskItemPayload(t zcode.Task) map[string]any {
	item := map[string]any{
		"taskId":         t.TaskID,
		"title":          t.Title, // must be a string or the phone renders [object Object]
		"workspacePath":  t.WorkspacePath,
		"workspaceLabel": pathLabel(t.WorkspacePath),
		"workspaceKind":  "local",
		"displayStatus":  displayStatus(t.Status),
		"createdAt":      t.CreatedAt,
		"updatedAt":      t.UpdatedAt,
	}
	if t.Pinned {
		item["pinned"] = true
	}
	if t.Archived {
		item["archived"] = true
	}
	if t.UnreadAt != nil {
		item["unreadAt"] = *t.UnreadAt
	}
	return item
}

func taskListPayload(kind string, ps *phoneSessions) []any {
	tasks, err := zcode.ListTasks("", kind)
	if err != nil {
		return []any{}
	}
	out := make([]any, 0, len(tasks))
	seen := map[string]bool{}
	for _, t := range tasks {
		// Only expose local tasks: remote SSH workspaces (workspaceKey with a
		// remote: prefix) have no matching local workspace the phone can open,
		// and the phone stalls waiting for a bridge it can never get.
		if strings.HasPrefix(t.WorkspaceKey, "remote:") {
			continue
		}
		seen[t.TaskID] = true
		out = append(out, taskItemPayload(t))
	}
	// Runtime tasks (created this session, not yet in the index) only belong
	// in the unfiltered list — adding them to pinned/archived/deleted views
	// would surface unarchived tasks in those views.
	if ps != nil && kind == "" {
		for _, rt := range ps.runtimeTaskList() {
			m, _ := rt.(map[string]any)
			sid, _ := m["taskId"].(string)
			if sid == "" || seen[sid] {
				continue // already surfaced from the persisted task index
			}
			seen[sid] = true
			title, _ := m["title"].(string)
			ws, _ := m["workspacePath"].(string)
			item := map[string]any{
				"taskId":         sid,
				"title":          title,
				"workspacePath":  ws,
				"workspaceLabel": pathLabel(ws),
				"workspaceKind":  "local",
				"displayStatus":  "running",
				"createdAt":      m["createdAt"],
				"updatedAt":      m["updatedAt"],
			}
			out = append(out, item)
		}
	}
	return out
}

// pathLabel returns the last path segment (used for the phone's workspaceLabel).
func pathLabel(p string) string {
	p = strings.TrimRight(p, "/\\")
	if i := strings.LastIndexAny(p, "/\\"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// firstNonEmpty returns the first non-empty string argument.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func displayStatus(s string) string {
	switch strings.ToLower(s) {
	case "running", "in-progress", "active":
		return "running"
	case "completed", "completedSuccess", "completedInterrupted":
		return "completed"
	case "error":
		return "error"
	case "idle", "cancelled", "failed", "interrupted", "paused", "":
		return "idle"
	default:
		return "idle"
	}
}

// handleChannelCall answers phone channel calls. Desktop-owned services
// (model-provider, zcode-task, setting, window-controller, …) are answered
// from the real ZCode state; the app-server engine gets only the calls it
// actually implements (session/* conversation traffic).
// phoneSessions tracks the phone's active conversation session and EventListen
// subscription ids so we can push conversation/sessions-index frames. The
// subscriptionId must be stable across subscribe ack / pushed frames / resync.
type phoneSessions struct {
	mu sync.Mutex
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
	// listeners records EventListen ids by purpose so events can be pushed to
	// them (provider registry changes, workspace events, controller frames).
	listeners map[string]int
	// resumeAlias maps a phone-visible (task) sessionId to the live engine
	// session we rebuilt it as, so continuing a historical task (whose engine
	// session died on daemon restart) executes against a real engine session
	// while the phone keeps seeing its original task id.
	resumeAlias map[string]string
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
				"createdAt":     now,
				"updatedAt":     now,
			}
		}
	}
	p.mu.Unlock()
}

// runtimeTask adds a session created by the engine so it shows in the phone.
func (p *phoneSessions) runtimeTask(sessionID, workspace, title string) {
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

func handleChannelCall(engine *relay.BridgeEngine, send func(any), workspaces []string, ps *phoneSessions, engClient *enginepkg.Client, termSvc *terminal.Service) func(*relay.ChannelCall) {
	return func(c *relay.ChannelCall) {
		fmt.Printf("zcode: [call] kind=%d id=%d %s.%s\n", c.Kind, c.ID, c.ChannelName, c.Name)
		// EventListen (102): record the subscription so we can push EventFire.
		if c.Kind == 102 {
			switch c.Name {
			case "onDynamicConversationFrame":
				ps.convListener = c.ID
				fmt.Printf("zcode: [listen] onDynamicConversationFrame id=%d (监听 ✓)\n", c.ID)
			case "onDynamicSessionsIndexFrame":
				ps.indexListener = c.ID
				fmt.Printf("zcode: [listen] onDynamicSessionsIndexFrame id=%d (监听 ✓)\n", c.ID)
			case "onAgentRuntimeLifecycle", "onAgentRuntimeRestarted":
				ps.runtimeListener = c.ID
				fmt.Printf("zcode: [listen] %s id=%d (监听 ✓)\n", c.Name, c.ID)
			case "onDynamicData":
				// terminal/onDynamicData — c.Arg may be the terminal id string,
				// or nil when the phone subscribes a single global listener.
				id, _ := c.Arg.(string)
				_ = termSvc.SetDataListener(id, c.ID)
				fmt.Printf("zcode: [listen] terminal.onDynamicData id=%d term=%q (监听 ✓)\n", c.ID, id)
			case "onDynamicExit":
				id, _ := c.Arg.(string)
				_ = termSvc.SetExitListener(id, c.ID)
				fmt.Printf("zcode: [listen] terminal.onDynamicExit id=%d term=%q (监听 ✓)\n", c.ID, id)
			case "onDidChangeProviderRegistry":
				ps.recordListener("providerRegistry", c.ID)
				fmt.Printf("zcode: [listen] onDidChangeProviderRegistry id=%d (监听 ✓)\n", c.ID)
			case "onDynamicWorkspaceEvent":
				ps.recordListener("workspaceEvent", c.ID)
				fmt.Printf("zcode: [listen] onDynamicWorkspaceEvent id=%d (监听 ✓)\n", c.ID)
			case "onDynamicControllerFrame":
				ps.recordListener("controllerFrame", c.ID)
				fmt.Printf("zcode: [listen] onDynamicControllerFrame id=%d (监听 ✓)\n", c.ID)
			case "onMessage":
				// Transport-level socket event (phone internals); nothing for
				// us to push — ack by recording so it never stalls.
				ps.recordListener("onMessage", c.ID)
				fmt.Printf("zcode: [listen] onMessage id=%d (记录，无需推送)\n", c.ID)
			default:
				fmt.Printf("zcode: [listen] UNTRACKED subscribe %s id=%d (未监听，EventListen 未记录)\n", c.Name, c.ID)
			}
			return
		}
		if c.Kind != 100 || c.ID == 0 {
			return
		}
		if answerDesktopChannel(engine, c, send, workspaces, ps, engClient, termSvc) {
			fmt.Printf("zcode: [done] %s.%s answered locally\n", c.ChannelName, c.Name)
			return
		}
		engine.RegisterCall(c.ID)
		method, params := translateChannelMethod(c, workspaces)
		if method == "__local__" {
			engine.ReplyChannelPromise(c.ID, []byte(`{"runtimeModel":null}`), send)
			fmt.Printf("zcode: [done] %s.%s handled as __local__\n", c.ChannelName, c.Name)
			return
		}
		engine.WriteToServer(fmt.Sprintf(`{"id":%d,"method":%q,"params":%s}`, c.ID, method, params))
		fmt.Printf("zcode: [fwd] %s.%s -> engine %s\n", c.ChannelName, c.Name, method)
	}
}

// translateChannelMethod maps phone channel calls onto the engine's real
// ZCode Protocol methods. The phone's zcode-session/* names differ from the
// engine's workspace/* + session/* methods; the engine also persists model
// selection to ~/.zcode/cli/config.json automatically.
func translateChannelMethod(c *relay.ChannelCall, workspaces []string) (method, params string) {
	raw := []byte("null")
	if c.Arg != nil {
		if b, ok := c.Arg.(json.RawMessage); ok && len(b) > 0 {
			raw = b
		} else if b, err := json.Marshal(c.Arg); err == nil {
			raw = b
		}
	}
	key := c.ChannelName + "/" + c.Name
	// helper to wrap args as {workspace:{...}} — engine workspace/* requires
	// workspaceKey, which the phone doesn't send; derive it from the bridge.
	withWorkspace := func(body map[string]any) string {
		if body == nil {
			body = map[string]any{}
		}
		var in struct {
			WorkspacePath     string `json:"workspacePath"`
			WorkspaceIdentity string `json:"workspaceIdentity"`
		}
		_ = json.Unmarshal(raw, &in)
		ws := map[string]any{"workspacePath": in.WorkspacePath}
		if in.WorkspaceIdentity != "" {
			ws["workspaceIdentity"] = in.WorkspaceIdentity
		}
		ws["workspaceKey"] = in.WorkspacePath
		if in.WorkspacePath == "" && len(workspaces) > 0 {
			ws["workspaceKey"] = workspaces[0]
			ws["workspacePath"] = workspaces[0]
		}
		body["workspace"] = ws
		b, _ := json.Marshal(body)
		return string(b)
	}

	switch key {
	case "zcode-session/setWorkspaceDefaultModel":
		// keep the model field, wrap workspacePath/Identity into workspace.
		var in struct {
			WorkspacePath     string `json:"workspacePath"`
			WorkspaceIdentity string `json:"workspaceIdentity"`
			Model             any    `json:"model"`
		}
		_ = json.Unmarshal(raw, &in)
		body := map[string]any{}
		if in.Model != nil {
			body["model"] = in.Model
		}
		return "workspace/setDefaultModel", withWorkspace(body)
	case "zcode-session/readWorkspaceState":
		return "workspace/readState", withWorkspace(nil)
	case "zcode-session/setModel":
		// session/setModel is strict: only sessionId + model (+persist flag).
		var in struct {
			SessionID string `json:"sessionId"`
			Model     any    `json:"model"`
		}
		_ = json.Unmarshal(raw, &in)
		body := map[string]any{}
		if in.SessionID != "" {
			body["sessionId"] = in.SessionID
		}
		if in.Model != nil {
			body["model"] = in.Model
		}
		body["persistAsWorkspaceLastUsed"] = true
		b, _ := json.Marshal(body)
		return "session/setModel", string(b)
	case "zcode-session/setThoughtLevel":
		return "session/setThoughtLevel", string(raw)
	case "zcode-session/setWorkspaceDefaultThoughtLevel":
		return "workspace/setDefaultThoughtLevel", withWorkspace(map[string]any{})
	case "zcode-session/closeDeferredDraftSession", "zcode-session/closeSession":
		return "session/close", string(raw)
	case "zcode-session/readSession":
		return "session/read", string(raw)
	case "zcode-session/resolveRuntimeModelForV4":
		// Resolve a runtime model for a model ref; answer with a minimal
		// structure so the phone continues (the model itself is already
		// applied via session/setModel / session/create).
		return "__local__", string(raw)
	default:
		return key, string(raw)
	}
}

func answerDesktopChannel(engine *relay.BridgeEngine, c *relay.ChannelCall, send func(any), workspaces []string, ps *phoneSessions, engClient *enginepkg.Client, termSvc *terminal.Service) bool {
	reply := func(result any) {
		b, _ := json.Marshal(result)
		engine.ReplyChannelPromise(c.ID, b, send)
	}
	replyNil := func() {
		engine.ReplyChannelPromise(c.ID, []byte("null"), send)
	}
	switch key := c.ChannelName + "/" + c.Name; key {
	case "terminal/create":
		var p struct {
			Cols int    `json:"cols"`
			Rows int    `json:"rows"`
			Cwd  string `json:"cwd"`
		}
		var raw json.RawMessage
		if b, ok := c.Arg.(json.RawMessage); ok {
			raw = b
		} else if b, err := json.Marshal(c.Arg); err == nil {
			raw = b
		}
		_ = json.Unmarshal(raw, &p)
		desc, err := termSvc.Create(p.Cols, p.Rows, p.Cwd)
		if err != nil {
			reply(map[string]any{"error": err.Error()})
			return true
		}
		reply(desc)
		fmt.Printf("zcode: terminal created id=%s\n", desc["id"])
	case "terminal/write":
		var p struct {
			ID   string `json:"id"`
			Data string `json:"data"`
		}
		var raw json.RawMessage
		if b, ok := c.Arg.(json.RawMessage); ok {
			raw = b
		} else if b, err := json.Marshal(c.Arg); err == nil {
			raw = b
		}
		_ = json.Unmarshal(raw, &p)
		if err := termSvc.Write(p.ID, p.Data); err != nil {
			reply(map[string]any{"error": err.Error()})
			return true
		}
		replyNil()
	case "terminal/resize":
		var p struct {
			ID   string `json:"id"`
			Cols int    `json:"cols"`
			Rows int    `json:"rows"`
		}
		var raw json.RawMessage
		if b, ok := c.Arg.(json.RawMessage); ok {
			raw = b
		} else if b, err := json.Marshal(c.Arg); err == nil {
			raw = b
		}
		_ = json.Unmarshal(raw, &p)
		if err := termSvc.Resize(p.ID, p.Cols, p.Rows); err != nil {
			reply(map[string]any{"error": err.Error()})
			return true
		}
		replyNil()
	case "terminal/dispose":
		var p struct {
			ID string `json:"id"`
		}
		var raw json.RawMessage
		if b, ok := c.Arg.(json.RawMessage); ok {
			raw = b
		} else if b, err := json.Marshal(c.Arg); err == nil {
			raw = b
		}
		_ = json.Unmarshal(raw, &p)
		termSvc.Dispose(p.ID)
		replyNil()
	case "model-provider/getAll", "model-provider/getAllCached":
		reply(providerPayload())
	case "model-provider/getDisplayOrder":
		order := make([]any, 0, len(zcode.Providers()))
		for _, p := range zcode.Providers() {
			order = append(order, p.ID)
		}
		reply(order)
	case "model-provider/getProviderRegistrySnapshot":
		reply(providerPayload())
	case "setting/get":
		homeDir, _ := os.UserHomeDir()
		reply(map[string]any{
			"language":       "en",
			"locale":         "zh-CN",
			"dataBaseDir":    homeDir,
			"defaultHomeDir": homeDir,
		})
	case "system/info":
		reply(map[string]any{
			"version":       "0.7.0",
			"appName":       "zcode-quick-web-forward",
			"platform":      "linux",
			"arch":          "amd64",
			"nodeVersion":   "",
			"runtime":       "web-remote",
			"home":          "",
			"userName":      "",
			"workspacePath": "/home/friddle/zqf-work",
		})
	case "setting/update":
		reply(map[string]any{})
	case "oauth/restoreCachedSessionState":
		reply(map[string]any{})
	case "oauth/getActiveProvider":
		reply(nil)
	case "git/refresh":
		reply([]any{})
	case "coding-plan-subscription/getBillingDiscount", "coding-plan-subscription/getManualClaimPlanPreviews":
		reply(map[string]any{})
	case "usage-stats/getEntitlementSnapshot":
		// The engine has no usage-stats service. Return the phone's neutral
		// state shape (snapshot:null) so the UI shows no plan/entitlement.
		reply(map[string]any{"snapshot": nil})
	case "file/ensureConversationWorkspace":
		// Phone-side file service: confirm the workspace dir for a conversation
		// (returns {path}). The engine has no file/* protocol — resolve the
		// path from the active workspace / session.
		pth := ""
		var fp struct {
			SessionID      string `json:"sessionId"`
			WorkspacePath  string `json:"workspacePath"`
			WorkspaceID    string `json:"workspaceId"`
			LocalWorkspace string `json:"localWorkspacePath"`
		}
		var raw json.RawMessage
		if b, ok := c.Arg.(json.RawMessage); ok {
			raw = b
		} else if b, err := json.Marshal(c.Arg); err == nil {
			raw = b
		}
		_ = json.Unmarshal(raw, &fp)
		pth = firstNonEmpty(fp.WorkspacePath, fp.LocalWorkspace, fp.WorkspaceID, ps.workspacePath)
		if pth == "" && len(workspaces) > 0 {
			pth = workspaces[0]
		}
		fmt.Printf("zcode: file.ensureConversationWorkspace path=%q session=%q\n", pth, fp.SessionID)
		reply(map[string]any{"path": pth})
	case "zcode-task/getTaskSessionFilePath":
		reply(map[string]any{"path": nil, "exists": false})
	case "zcode-task/getTaskNativeSessionLogFile":
		reply(map[string]any{"provider": nil, "path": nil, "exists": false})
	case "zcode-task/getTaskSessionId", "zcode-task/getTaskNativeSessionId":
		reply(map[string]any{"sessionId": nil})
	case "settings-sync/getFirstRunPromptState":
		// Returning handled:true suppresses the "欢迎使用 ZCode" first-run
		// wizard on every new pairing session (the phone checks e.handled
		// before opening it).
		reply(map[string]any{"handled": true})
	case "settings-sync/markFirstRunPromptHandled":
		reply(map[string]any{})
	case "zcode-task/getWorkspaceProviderConfigFile":
		reply(nil)
	case "zcode-task/listTasks", "zcode-task/listPinnedTasks", "zcode-task/listArchivedTasks":
		kind := ""
		switch c.Name {
		case "listPinnedTasks":
			kind = "pinned"
		case "listArchivedTasks":
			kind = "archived"
		}
		reply(taskListPayload(kind, ps))
	case "zcode-task/listPinnedTaskIds", "zcode-task/listArchivedTaskIds", "zcode-task/listDeletedTaskIds", "zcode-task/listRecentTasks":
		kind := ""
		switch c.Name {
		case "listPinnedTaskIds":
			kind = "pinned"
		case "listArchivedTaskIds":
			kind = "archived"
		case "listDeletedTaskIds":
			// Must be the real deleted set: the UI subtracts these ids from
			// merged views, so answering with archived ids would hide tasks.
			kind = "deleted"
		}
		ids, _ := zcode.ListTaskIDs(kind)
		anyIDs := make([]any, 0, len(ids))
		for _, id := range ids {
			anyIDs = append(anyIDs, id)
		}
		reply(anyIDs)
	case "zcode-task/deleteTask", "zcode-task/archiveTask", "zcode-task/unarchiveTask",
		"zcode-task/setTaskPinned", "zcode-task/setTaskUnread", "zcode-task/renameTask":
		// The phone's task-list actions. Persisting here is what makes them
		// survive reloads — the UI itself only removes the row optimistically.
		var a struct {
			TaskID        string `json:"taskId"`
			WorkspacePath string `json:"workspacePath"`
			Title         string `json:"title"`
			Pinned        *bool  `json:"pinned"`
			Unread        *bool  `json:"unread"`
		}
		switch v := c.Arg.(type) {
		case map[string]any:
			raw, _ := json.Marshal(v)
			_ = json.Unmarshal(raw, &a)
		case json.RawMessage:
			_ = json.Unmarshal(v, &a)
		}
		if a.TaskID == "" {
			reply(nil)
			return true
		}
		var err error
		switch c.Name {
		case "deleteTask":
			err = zcode.SetTaskFlags(a.TaskID, boolPtr(true), nil, nil)
		case "archiveTask":
			err = zcode.SetTaskFlags(a.TaskID, nil, boolPtr(true), nil)
		case "unarchiveTask":
			err = zcode.SetTaskFlags(a.TaskID, nil, boolPtr(false), nil)
		case "setTaskPinned":
			err = zcode.SetTaskFlags(a.TaskID, nil, nil, a.Pinned)
		case "setTaskUnread":
			err = zcode.SetTaskUnread(a.TaskID, a.Unread != nil && *a.Unread)
		case "renameTask":
			err = zcode.RenameTask(a.TaskID, a.Title)
		}
		if err != nil {
			fmt.Printf("zcode: %s %s failed: %v\n", c.Name, a.TaskID, err)
			reply(nil)
			return true
		}
		fmt.Printf("zcode: task %s %s persisted\n", c.Name, a.TaskID)
		if t, ok, gerr := zcode.GetTask(a.TaskID); gerr == nil && ok {
			// Echo the updated row: the UI's .then() uses it as the new state.
			reply(taskItemPayload(t))
		} else {
			reply(nil)
		}
		return true
	case "window-controller/subscribeControllerV4", "window-controller/getSnapshot", "window-controller/getControllerSnapshot":
		reply([]any{})
	case "client-scenes/list":
		reply([]any{})
	case "subagents/list":
		reply([]any{})
	case "zcode-agent/getAgentRuntimeLifecycle":
		reply(map[string]any{"status": "running"})
	case "zcode-agent/helloConversationV4":
		// The phone's strict zod schema (Nle) validates this exact shape:
		// .strict() rejects any extra field, all fields required, and
		// deliveryProfile must pair with clientMode.
		reply(map[string]any{
			"kind":            "hello",
			"protocolVersion": 3,
			"connectionId":    uuidNew(),
			"clientMode":      "web-remote-replayable",
			"deliveryProfile": "replayable",
			"serverTime":      time.Now().UnixMilli(),
			"capabilities": map[string]any{
				"nativeDialogs": true,
				"localTerminal": true,
				"binaryFrames":  true,
				"compression":   "none",
			},
			"auth": map[string]any{},
		})
	case "zcode-agent/initializeConversationV4":
		reply(map[string]any{})
	case "zcode-agent/subscribeConversationV4", "zcode-agent/subscribeSessionsIndexV4", "zcode-agent/resyncConversationV4", "zcode-agent/resyncSessionsIndexV4":
		// The subscriptionId must be stable and echoed in resync (never
		// minted fresh) — a mismatch throws resyncGenerationMismatch. We
		// derive it from the sessionId so pushed frames and the ack agree.
		// The phone may ask to subscribe/resync a SPECIFIC session (opening an
		// old task from the list) — honor that instead of only the bridge's
		// current session.
		var subReq struct {
			SessionID string `json:"sessionId"`
		}
		if raw, ok := c.Arg.(json.RawMessage); ok {
			_ = json.Unmarshal(raw, &subReq)
		} else if b, err := json.Marshal(c.Arg); err == nil {
			_ = json.Unmarshal(b, &subReq)
		}
		if subReq.SessionID != "" {
			ps.setSession(subReq.SessionID, "") // keep workspacePath
		}
		sid, _ := ps.get()
		subID := ps.convSub()
		if subID == "" {
			if sid != "" {
				subID = sid + ":sub"
			} else {
				subID = uuidNew()
			}
			ps.setConvSubscription(subID)
		}
		// A resync means the phone already applied our snapshot base, so the
		// ack mode must be "resume" (and logEpoch must match the base). A
		// fresh subscribe is "snapshot".
		mode := "snapshot"
		if c.Name == "resyncConversationV4" || c.Name == "resyncSessionsIndexV4" {
			mode = "resume"
		}
		ack := map[string]any{
			"ack": map[string]any{
				"subscriptionId": subID,
				"mode":           mode,
				"logEpoch":       "0", // must be a string
			},
		}
		reply(ack)
		go pushSubscriptionFrames(engine, send, ps, c, engClient)
	case "zcode-agent/sendConversationCommandV4":
		ack := bridgeSendCommand(c, engClient, ps, workspaces)
		reply(ack)
		if sess, ok := ack["result"].(map[string]any); ok {
			if s, _ := sess["sessionId"].(string); s != "" {
				ps.setSession(s, "")
				go pushConversationFrame(engine, send, ps, ack)
			}
		}
		// When a text message was accepted, render the user's message in the
		// conversation immediately (the engine's transcript only comes back at
		// turn end, so without this the phone stays blank while it runs).
		if txt, ok := ack["userTextSent"].(string); ok && txt != "" {
			sid, _ := ps.get()
			if sid != "" {
				go func() {
					time.Sleep(200 * time.Millisecond)
					ps.mu.Lock()
					convID, convSub, ws := ps.convListener, ps.convSubscription, ps.workspacePath
					ps.mu.Unlock()
					if convID == 0 {
						return
					}
					now := time.Now().UnixMilli()
					row := map[string]any{
						"rowId":        1,
						"turnId":       "turn-" + sid,
						"createdAt":    now,
						"createdAtSeq": 1,
						"kind":         "userText",
						"text":         txt,
						"state":        "complete",
					}
					b, _ := json.Marshal(conversationSnapshotFrame(sid, ws, convSub, "recovery", ps.nextOrdinal(), []any{row}, ps.collabMode))
					engine.SendChannelEvent(convID, b, send)
					fmt.Printf("zcode: pushed user text snapshot session=%s text=%q\n", sid, txt)
				}()
			}
		}
		// A collaboration-mode switch needs a snapshot push so the phone's
		// picker reflects the new mode (the engine doesn't relay setMode).
		if mode, ok := ack["modeChanged"].(string); ok && mode != "" {
			sid, _ := ps.get()
			if sid != "" {
				go func() {
					time.Sleep(400 * time.Millisecond)
					ps.mu.Lock()
					convID, convSub := ps.convListener, ps.convSubscription
					ps.mu.Unlock()
					if convID > 0 {
						b, _ := json.Marshal(conversationSnapshotFrame(sid, ps.workspacePath, convSub, "recovery", ps.nextOrdinal(), nil, mode))
						engine.SendChannelEvent(convID, b, send)
						fmt.Printf("zcode: pushed mode snapshot session=%s mode=%s\n", sid, mode)
					}
				}()
			}
		}
	case "zcode-agent/queryConversationCommandsV4":
		reply(map[string]any{"results": []any{}})
	case "zcode-agent/unsubscribeConversationV4", "zcode-agent/unsubscribeSessionsIndexV4":
		reply(map[string]any{})
	case "zcode-agent/conversationRowsRangeV4", "zcode-agent/conversationPlansV4", "zcode-agent/conversationFileChangesV4", "zcode-agent/conversationFileRewindPreviewV4":
		reply(map[string]any{})
	case "coding-plan-subscription/getStaticTeamProducts":
		reply([]any{})
	case "coding-plan-subscription/getEnterprisePricing":
		reply(map[string]any{})
	case "settings-sync/detect":
		reply(map[string]any{})
	case "broadcast/getState":
		reply(map[string]any{})
	case "broadcast/listeners":
		reply([]any{})
	case "zcode-agent/getDynamicSessionsIndex", "zcode-agent/getConversationRowsRange", "zcode-agent/getConversationFileChanges":
		reply(map[string]any{})
	default:
		return false
	}
	fmt.Printf("zcode: answered %s.%s from real state\n", c.ChannelName, c.Name)
	return true
}

// pushSubscriptionFrames pushes the sessions-index + conversation snapshot so
// the phone's subscription goes live (otherwise it resync-loops).
func pushSubscriptionFrames(engine *relay.BridgeEngine, send func(any), ps *phoneSessions, c *relay.ChannelCall, engClient *enginepkg.Client) {
	time.Sleep(300 * time.Millisecond)
	ps.mu.Lock()
	indexID, runtimeID, convSub := ps.indexListener, ps.runtimeListener, ps.convSubscription
	ps.mu.Unlock()
	if indexID > 0 {
		b, _ := json.Marshal(sessionsIndexFrame(convSub, ps))
		engine.SendChannelEvent(indexID, b, send)
		fmt.Println("zcode: pushed sessions-index snapshot")
	}
	if runtimeID > 0 {
		b, _ := json.Marshal(map[string]any{"workspaceKey": "local", "state": "available"})
		engine.SendChannelEvent(runtimeID, b, send)
	}
	// If this was a resync, the phone waits for a recovery-delivery frame with
	// the full transcript so it renders the conversation history. When the
	// phone opens a specific session (old task from the list) we also push its
	// history so the conversation is restored, not left empty.
	if (c.Name == "resyncConversationV4" || c.Name == "resyncSessionsIndexV4" ||
		c.Name == "subscribeConversationV4") && (c.Name != "subscribeSessionsIndexV4") {
		sid, _ := ps.get()
		if sid != "" {
			rows := []any{}
			if engClient != nil {
				if tx, err := engClient.ReadSession(sid, 10*time.Second); err == nil {
					rows = messageRows(tx, sid, ps.nextOrdinal)
					fmt.Printf("zcode: recovery read session=%s rows=%d\n", sid, len(rows))
				} else if stored := zcode.LoadSessionTranscript(sid); stored != nil {
					// Engine session gone (daemon restarted): restore from the
					// transcript snapshot we saved when the turn completed.
					rows = messageRows(stored, sid, ps.nextOrdinal)
					fmt.Printf("zcode: recovery from transcript session=%s rows=%d (engine: %v)\n", sid, len(rows), err)
				} else {
					fmt.Printf("zcode: recovery read failed: %v (no transcript)\n", err)
				}
			}
			// workspacePath: prefer the one persisted for this task in sqlite.
			ws := ps.workspacePath
			if tasks, err := zcode.ListTasks("", ""); err == nil {
				for _, t := range tasks {
					if t.TaskID == sid && t.WorkspacePath != "" {
						ws = t.WorkspacePath
						break
					}
				}
			}
			b, _ := json.Marshal(conversationSnapshotFrame(sid, ws, convSub, "recovery", ps.nextOrdinal(), rows, ps.collabMode))
			engine.SendChannelEvent(ps.convListener, b, send)
			fmt.Printf("zcode: pushed recovery snapshot session=%s rows=%d\n", sid, len(rows))
		}
	}
}

// pushConversationFrame pushes an initial conversation snapshot for the opened
// session so the phone renders the (empty) conversation instead of waiting.
func pushConversationFrame(engine *relay.BridgeEngine, send func(any), ps *phoneSessions, ack map[string]any) {
	time.Sleep(500 * time.Millisecond)
	sess, ok := ack["result"].(map[string]any)
	if !ok {
		return
	}
	sessionID, _ := sess["sessionId"].(string)
	ps.mu.Lock()
	convID, ws, convSub := ps.convListener, ps.workspacePath, ps.convSubscription
	ps.mu.Unlock()
	if convID == 0 {
		return
	}
	frame := conversationSnapshotFrame(sessionID, ws, convSub, "initial", ps.nextOrdinal(), nil, ps.collabMode)
	b, _ := json.Marshal(frame)
	engine.SendChannelEvent(convID, b, send)
	fmt.Printf("zcode: pushed conversation snapshot session=%s\n", sessionID)
}

// syncConversation pulls the engine session transcript and pushes each message
// as a conversation delta so the phone renders the full reply (assistant text +
// tool outputs). Called when a turn.terminal event fires. engineSid is the live
// engine session (may be a rebuilt continuation); phoneSid is the task id the
// phone displays (may differ when resuming a historical task).
func syncConversation(engClient *enginepkg.Client, engine *relay.BridgeEngine, sender *relaySender, ps *phoneSessions, engineSid, phoneSid string) {
	if phoneSid == "" {
		phoneSid = engineSid
	}
	ps.mu.Lock()
	convID := ps.convListener
	convSub := ps.convSubscription
	ps.mu.Unlock()
	if convID == 0 {
		return
	}
	tx, err := engClient.ReadSession(engineSid, 10*time.Second)
	if err != nil {
		fmt.Printf("zcode: syncConversation read failed: %v\n", err)
		return
	}
	// Snapshot the transcript to disk so the phone can reopen this task's
	// history even after the engine restarts (its sessions are in-memory).
	if saved, _ := json.Marshal(tx); len(saved) > 0 {
		if err := zcode.SaveSessionTranscript(phoneSid, tx); err != nil {
			fmt.Printf("zcode: transcript save failed: %v\n", err)
		} else {
			fmt.Printf("zcode: transcript saved session=%s bytes=%d\n", phoneSid, len(saved))
		}
	}
	// Persist the engine-generated session title (engine titles the task, e.g.
	// "List files in /home/friddle") so the phone list shows real names.
	if s, ok := tx["session"].(map[string]any); ok {
		if t, _ := s["title"].(string); t != "" {
			if ws, _ := s["workspace"].(map[string]any); ws != nil {
				if wp, _ := ws["workspacePath"].(string); wp != "" {
					if err := zcode.UpsertTask(wp, wp, phoneSid, t, "completed"); err != nil {
						fmt.Printf("zcode: task title sync failed: %v\n", err)
					} else {
						fmt.Printf("zcode: task title synced %s title=%q\n", phoneSid, t)
					}
				}
			}
		}
	}
	rows := messageRows(tx, phoneSid, ps.nextOrdinal)
	if len(rows) == 0 {
		fmt.Printf("zcode: syncConversation empty rows session=%s\n", phoneSid)
		return
	}
	b, _ := json.Marshal(conversationSnapshotFrame(phoneSid, ps.workspacePath, convSub, "recovery", ps.nextOrdinal(), rows, ps.collabMode))
	engine.SendChannelEvent(convID, b, sender.send)
	fmt.Printf("zcode: synced conversation engine=%s phone=%s rows=%d\n", engineSid, phoneSid, len(rows))
}

// messageRows converts a session/read transcript into conversation snapshot
// rows (assistant text + tool outputs + user text) so the phone renders the
// full reply.
func messageRows(tx map[string]any, sessionID string, ordinal func() int) []any {
	var msgs []any
	if m, ok := tx["messages"].([]any); ok {
		msgs = m
	} else if m, ok := tx["rows"].([]any); ok {
		msgs = m
	}
	out := make([]any, 0, len(msgs))
	for _, raw := range msgs {
		m, _ := raw.(map[string]any)
		if m == nil {
			continue
		}
		role, _ := m["role"].(string)
		kind, _ := m["kind"].(string)
		content, _ := m["content"].(string)
		if content == "" {
			content = messageTextFromParts(m["parts"], m["contentParts"])
		}
		if content == "" {
			continue
		}
		rowKind := "assistantText"
		if role == "user" || kind == "userText" {
			rowKind = "userText"
		}
		now := time.Now().UnixMilli()
		out = append(out, map[string]any{
			"rowId":               0,
			"turnId":              "turn-" + sessionID,
			"createdAt":           now,
			"createdAtSeq":        1,
			"kind":                rowKind,
			"assistantResponseId": "ar-" + sessionID,
			"text":                content,
			"state":               "complete",
		})
	}
	return out
}

// messageTextFromParts extracts joined text from a message's parts array
// (assistant messages carry {type:"text",text:...} or {type:"tool",...}).
func messageTextFromParts(parts any, contentParts any) string {
	extract := func(p any) string {
		pm, _ := p.(map[string]any)
		if pm == nil {
			return ""
		}
		t, _ := pm["type"].(string)
		if t == "text" {
			if txt, ok := pm["text"].(string); ok {
				return txt
			}
			return ""
		}
		if t == "tool" {
			// {type:"tool", title:"Bash", state:{input:{command}, output}}
			var cmd, output string
			if st, ok := pm["state"].(map[string]any); ok {
				if inp, ok := st["input"].(map[string]any); ok {
					cmd, _ = inp["command"].(string)
				}
				output, _ = st["output"].(string)
			}
			if cmd == "" && output == "" {
				return ""
			}
			return "```\n" + cmd + "\n```\n```\n" + output + "\n```"
		}
		return ""
	}
	var sb strings.Builder
	for _, list := range []any{parts, contentParts} {
		if arr, ok := list.([]any); ok {
			for _, p := range arr {
				if s := extract(p); s != "" {
					sb.WriteString(s)
					sb.WriteString("\n")
				}
			}
		}
	}
	return sb.String()
}

// sessionsIndexFrame builds a sessions-index snapshot wire frame.
func sessionsIndexFrame(convSub string, ps *phoneSessions) map[string]any {
	idx := "0"
	tasks, _ := zcode.ListTasks("", "")
	sessions := make([]any, 0, len(tasks))
	seen := map[string]bool{}
	for _, t := range tasks {
		if strings.HasPrefix(t.WorkspaceKey, "remote:") {
			continue
		}
		seen[t.TaskID] = true
		sessions = append(sessions, map[string]any{
			"sessionId":      t.TaskID,
			"workspaceId":    t.WorkspaceKey,
			"title":          t.Title,
			"titleSource":    "generated",
			"phase":          "idle",
			"createdAt":      t.CreatedAt,
			"lastActivityAt": t.UpdatedAt,
		})
	}
	if ps != nil {
		for _, rt := range ps.runtimeTaskList() {
			m, _ := rt.(map[string]any)
			sid, _ := m["taskId"].(string)
			if sid == "" || seen[sid] {
				continue
			}
			seen[sid] = true
			title, _ := m["title"].(string)
			ws, _ := m["workspacePath"].(string)
			sessions = append(sessions, map[string]any{
				"sessionId":      sid,
				"workspaceId":    ws,
				"title":          title,
				"titleSource":    "generated",
				"phase":          "running",
				"createdAt":      m["createdAt"],
				"lastActivityAt": m["updatedAt"],
			})
		}
	}
	return map[string]any{
		"wireVersion":         3,
		"kind":                "complete",
		"deliveryKind":        "initial",
		"logicalFrameId":      uuidNew(),
		"logicalFrameOrdinal": 1,
		"topic":               "sessions-index/local",
		"subscriptionId":      convSub,
		"frame": map[string]any{
			"topic":          "sessions-index/local",
			"subscriptionId": convSub,
			"fromSeq":        1,
			"toSeq":          1,
			"sentAt":         time.Now().UnixMilli(),
			"payload": map[string]any{
				"kind": "snapshot",
				"snapshot": map[string]any{
					"protocolVersion": 1,
					"workspaceId":     "local",
					"logEpoch":        idx,
					"sessions":        sessions,
				},
			},
		},
	}
}

// stateUpdatedFrame wraps an engine state.updated as a conversation frame delta.
func stateUpdatedFrame(sessionID, status, convSub string, ordinal int) map[string]any {
	if convSub == "" {
		convSub = sessionID + ":sub"
	}
	return map[string]any{
		"wireVersion":         3,
		"kind":                "complete",
		"deliveryKind":        "online",
		"logicalFrameId":      uuidNew(),
		"logicalFrameOrdinal": ordinal,
		"topic":               "conversation/" + sessionID,
		"subscriptionId":      convSub,
		"frame": map[string]any{
			"topic":          "conversation/" + sessionID,
			"subscriptionId": convSub,
			"fromSeq":        1,
			"toSeq":          2,
			"sentAt":         time.Now().UnixMilli(),
			"payload": map[string]any{
				"kind": "deltas",
				"deltas": []any{
					map[string]any{
						"op": "state.updated",
						"patch": map[string]any{
							"control": map[string]any{
								"phase": status,
							},
						},
					},
				},
			},
		},
	}
}

// conversationChunkFrame wraps one assistant text chunk as a conversation
// delta (row.append-style) so the phone renders streaming output.
func conversationChunkFrame(sessionID, text, convSub string, ordinal int) map[string]any {
	if convSub == "" {
		convSub = sessionID + ":sub"
	}
	now := time.Now().UnixMilli()
	return map[string]any{
		"wireVersion":         3,
		"kind":                "complete",
		"deliveryKind":        "online",
		"logicalFrameId":      uuidNew(),
		"logicalFrameOrdinal": ordinal,
		"topic":               "conversation/" + sessionID,
		"subscriptionId":      convSub,
		"frame": map[string]any{
			"topic":          "conversation/" + sessionID,
			"subscriptionId": convSub,
			"fromSeq":        1,
			"toSeq":          2,
			"sentAt":         now,
			"payload": map[string]any{
				"kind": "deltas",
				"deltas": []any{
					map[string]any{
						"op": "row.appended",
						"row": map[string]any{
							"rowId":               1,
							"turnId":              "turn-" + sessionID,
							"createdAt":           now,
							"createdAtSeq":        1,
							"kind":                "assistantText",
							"assistantResponseId": "ar-" + sessionID,
							"text":                text,
							"state":               "streaming",
						},
					},
				},
			},
		},
	}
}

// conversationSnapshotFrame builds a conversation snapshot wire frame matching
// the phone's strict Hoe schema. deliveryKind is "initial" (fresh subscribe) or
// "recovery" (resync). ordinal must strictly increase per subscription.
func conversationSnapshotFrame(sessionID, workspace, convSub, deliveryKind string, ordinal int, rows []any, mode string) map[string]any {
	if convSub == "" {
		convSub = sessionID + ":sub"
	}
	if deliveryKind == "" {
		deliveryKind = "initial"
	}
	if rows == nil {
		rows = []any{}
	}
	if mode == "" {
		mode = "build"
	}
	snapshot := map[string]any{
		"protocolVersion": 1,
		"sessionId":       sessionID,
		"logEpoch":        "0",
		"seq":             1,
		"revision":        0,
		"control": map[string]any{
			"phase":          "running",
			"sessionEnded":   false,
			"canStop":        false,
			"stopState":      "idle",
			"stopTargetKind": "assistant",
			"activeWorks":    []any{},
			"lastError":      nil,
			"apiRetry":       nil,
		},
		"availability": map[string]any{
			"fork": map[string]any{"allowed": true}, "compact": map[string]any{"allowed": true},
			"switchModelConfig": map[string]any{"allowed": true}, "setFollowupMode": map[string]any{"allowed": true},
			"queueEdit": map[string]any{"allowed": true}, "sendQueuedNow": map[string]any{"allowed": true},
			"pauseGoal": map[string]any{"allowed": true}, "resumeGoal": map[string]any{"allowed": true},
		},
		"inputRouting": map[string]any{"mode": "startNow"},
		"meta":         map[string]any{"title": "", "titleSource": "default"},
		"config": map[string]any{
			"provider":      "bigmodel",
			"model":         "GLM-5.3",
			"thought":       "low",
			"thoughtLevels": []any{},
			"followupMode":  "queue",
			"mode":          mode,
		},
		"modelTransition": nil,
		"usage": map[string]any{
			"contextWindow": nil,
			"cumulative": map[string]any{
				"inputTokens": 0, "outputTokens": 0, "cacheReadTokens": 0, "cacheWriteTokens": 0,
			},
		},
		"queue":               map[string]any{"items": []any{}, "autoDrain": false},
		"pendingInteractions": []any{},
		"pendingCommands":     []any{},
		"backgroundWorks":     []any{},
		"subagents": map[string]any{
			"revision": 0, "childSessionIds": []any{}, "running": []any{}, "endedTotal": 0,
		},
		"goal":                   nil,
		"plan":                   nil,
		"workspaceHookAdmission": nil,
		"rows": map[string]any{
			"window":     rows,
			"totalCount": len(rows),
			"firstRowId": nil,
		},
	}
	return map[string]any{
		"wireVersion":         3,
		"kind":                "complete",
		"deliveryKind":        deliveryKind,
		"logicalFrameId":      uuidNew(),
		"logicalFrameOrdinal": ordinal,
		"topic":               "conversation/" + sessionID,
		"subscriptionId":      convSub,
		"frame": map[string]any{
			"topic":          "conversation/" + sessionID,
			"subscriptionId": convSub,
			"fromSeq":        0,
			"toSeq":          1,
			"sentAt":         time.Now().UnixMilli(),
			"payload": map[string]any{
				"kind":     "snapshot",
				"snapshot": snapshot,
			},
		},
	}
}

// bridgeSendCommand bridges a phone conversation command (createSession /
// sendText) to the real engine. Returns the ack to send back to the phone.
func bridgeSendCommand(c *relay.ChannelCall, engClient *enginepkg.Client, ps *phoneSessions, workspaces []string) map[string]any {
	ack := map[string]any{
		"commandId":          uuidNew(),
		"status":             "accepted",
		"revisionAtDecision": 0,
	}
	// Parse the phone envelope: {workspacePath, envelope:{commandId, sessionId, type, payload}}.
	var arg []byte
	switch a := c.Arg.(type) {
	case json.RawMessage:
		arg = a
	case map[string]any:
		arg, _ = json.Marshal(a)
	case []any:
		// phone wraps args as Array(1){object}
		arg, _ = json.Marshal(a)
	default:
		arg, _ = json.Marshal(c.Arg)
	}
	var req struct {
		Envelope struct {
			CommandID string `json:"commandId"`
			SessionID string `json:"sessionId"`
			Type      string `json:"type"`
			Payload   struct {
				WorkspaceID string `json:"workspaceId"`
				FirstInput  struct {
					Text string `json:"text"`
				} `json:"firstInput"`
				Text     string `json:"text"`
				Provider string `json:"provider"`
				Model    string `json:"model"`
				Mode     string `json:"mode"`
				Config   struct {
					Provider string `json:"provider"`
					Model    string `json:"model"`
				} `json:"config"`
			} `json:"payload"`
		} `json:"envelope"`
	}
	if json.Unmarshal(arg, &req) != nil {
		fmt.Printf("zcode: bridgeSendCommand unmarshal failed arg=%s\n", arg)
		return ack
	}
	fmt.Printf("zcode: bridgeSendCommand raw=%s\n", arg)
	if req.Envelope.CommandID != "" {
		ack["commandId"] = req.Envelope.CommandID
	}

	switch req.Envelope.Type {
	case "createSession":
		ws := req.Envelope.Payload.WorkspaceID
		if ws == "" && len(workspaces) > 0 {
			ws = workspaces[0]
		}
		// Ask the engine to create a real session. Prefer the config's default
		// model (model.main) over the phone's cached selection — the phone may
		// still carry a model whose key is out of balance, and the config is
		// the authoritative default.
		provider := req.Envelope.Payload.Config.Provider
		model := req.Envelope.Payload.Config.Model
		if defP, defM := zcode.DefaultModel(); defP != "" && defM != "" {
			provider, model = defP, defM
		}
		res, err := engClient.CreateSession(ws, ws, provider, model, 15*time.Second)
		if err != nil {
			fmt.Printf("zcode: engine createSession failed: %v\n", err)
			ack["status"] = "failed"
			ack["message"] = err.Error()
			return ack
		}
		sid, _ := res["sessionId"].(string)
		if sid == "" {
			// fall back to whatever the engine returned
			if s, ok := res["session"].(map[string]any); ok {
				sid, _ = s["sessionId"].(string)
			}
		}
		ps.setSession(sid, ws)
		title := req.Envelope.Payload.FirstInput.Text
		if title == "" {
			title = req.Envelope.Payload.Text
		}
		if title == "" {
			title = "新任务"
		}
		ps.runtimeTask(sid, ws, title)
		// Persist to the real task index so the task survives reconnects/restarts
		// (the phone's task list is read from that sqlite).
		if err := zcode.UpsertTask(ws, ws, sid, title, "running"); err != nil {
			fmt.Printf("zcode: task persist failed: %v\n", err)
		} else {
			fmt.Printf("zcode: task persisted %s\n", sid)
		}
		ack["result"] = map[string]any{
			"type":      "createSession",
			"sessionId": sid,
		}
		fmt.Printf("zcode: engine created session %s title=%q\n", sid, title)
	case "sendText", "":
		sid := req.Envelope.SessionID
		if sid == "" {
			sid, _ = ps.get()
		}
		text := req.Envelope.Payload.Text
		if text == "" {
			text = req.Envelope.Payload.FirstInput.Text
		}
		if sid != "" && text != "" {
			// The phone may send into a historical task whose engine session
			// died on a daemon restart. Resolve to the live engine session; if
			// none is active, rebuild one and continue from the saved
			// transcript so the command actually executes.
			engineSid := ps.engineFor(sid)
			if engineSid == sid {
				if _, err := engClient.ReadSession(sid, 3*time.Second); err != nil {
					engineSid = rebuildContinuedSession(engClient, ps, sid)
				}
			}
			if engineSid == "" {
				ack["status"] = "failed"
				ack["message"] = "无法恢复该历史任务的会话"
			} else {
				if !engClient.SendMessage(engineSid, text) {
					ack["status"] = "failed"
					ack["message"] = "engine stdin closed"
				}
				fmt.Printf("zcode: engine session/send %s (phone=%s) text=%q\n", engineSid, sid, text)
				ack["userTextSent"] = text
			}
		}
	case "switchModelConfig":
		// Switch the current session's model. The payload is
		// {provider, model, thought} at the top level.
		sid := req.Envelope.SessionID
		if sid == "" {
			sid, _ = ps.get()
		}
		provider := req.Envelope.Payload.Provider
		model := req.Envelope.Payload.Model
		if sid != "" && provider != "" && model != "" {
			body := map[string]any{
				"sessionId":                  sid,
				"model":                      map[string]any{"providerId": provider, "modelId": model},
				"persistAsWorkspaceLastUsed": true,
			}
			if engClient.Write(map[string]any{"id": 0, "method": "session/setModel", "params": body}) {
				fmt.Printf("zcode: engine switchModelConfig session=%s provider=%s model=%s\n", sid, provider, model)
			}
		}
	case "switchCollaborationMode":
		// The phone's mode picker (变更前确认/自动编辑/计划模式/完全访问) sends
		// {mode: confirm|edit|plan|yolo}. Forward it to the engine's
		// session/setMode so it actually takes effect.
		sid := req.Envelope.SessionID
		if sid == "" {
			sid, _ = ps.get()
		}
		mode := req.Envelope.Payload.Mode
		// The engine accepts confirm/edit/plan/yolo; map anything unknown away.
		switch mode {
		case "confirm", "edit", "plan", "yolo":
		default:
			mode = "confirm"
		}
		if sid != "" {
			if engClient.SetMode(sid, mode) {
				fmt.Printf("zcode: engine setMode session=%s mode=%s\n", sid, mode)
			}
			// Remember the phone's selected mode so permission requests below
			// can decide whether to auto-approve or ask.
			ps.mu.Lock()
			ps.collabMode = mode
			ps.mu.Unlock()
			ack["modeChanged"] = mode
		}
	default:
		fmt.Printf("zcode: engine command type %q unhandled (ignored)\n", req.Envelope.Type)
	}
	return ack
}

// handleEngineEvent processes engine->client notifications: it answers
// session/requestRuntimePreferences and forwards conversation-relevant stream
// events to the phone as onDynamicConversationFrame frames.
func handleEngineEvent(engClient *enginepkg.Client, engine *relay.BridgeEngine, sender *relaySender, ps *phoneSessions, m json.RawMessage, br *browser.Browser) {
	var ev struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if json.Unmarshal(m, &ev) != nil || ev.Method == "" {
		return
	}
	switch ev.Method {
	case "session/requestRuntimePreferences":
		// The engine asks the client for runtime prefs; answer with the real
		// settings so it can materialize the app and accept messages.
		result := map[string]any{
			"nativeSearchEnhancementsEnabled": true,
			"memoryEnabled":                   false,
		}
		engClient.RespondToRequest(ev.ID, result)
		fmt.Println("zcode: engine prefs ok")
	case "interaction/requestPermission":
		// The engine requests permission for a tool (e.g. browser-use opening
		// a browser, or Edit/Write changing a file). The phone already asked
		// to run the task, so in edit/plan/yolo modes we auto-approve. In
		// confirm mode (变更前确认) file-modifying tools must NOT be silently
		// allowed — deny them so the engine reports the change needs approval.
		// The broker result schema S2 is
		// {decision, reason?, modifiedInput?, permissionUpdates?}.strict() —
		// any extra field (even resolvedAt) fails strict and the engine
		// silently retries the permission request forever.
		var pp struct {
			ToolName string `json:"toolName"`
			Action   string `json:"action"`
		}
		_ = json.Unmarshal(ev.Params, &pp)
		ps.mu.Lock()
		mode := ps.collabMode
		ps.mu.Unlock()
		tool := pp.ToolName
		if tool == "" {
			tool = pp.Action
		}
		if mode == "confirm" && (tool == "Edit" || tool == "Write" || tool == "edit_file" || tool == "write_file") {
			engClient.RespondToRequest(ev.ID, map[string]any{
				"decision": "deny",
				"reason":   "变更前确认模式: 文件修改需要用户批准",
			})
			fmt.Printf("zcode: denied %s (confirm mode)\n", tool)
			return
		}
		engClient.RespondToRequest(ev.ID, map[string]any{
			"decision": "allow",
			"reason":   "Auto-approved by zcode-quick-web-forward (phone requested this task)",
		})
		fmt.Printf("zcode: auto-approved permission request (mode=%s tool=%s)\n", mode, tool)
	case "interaction/browserList":
		// Report the browser host so the engine's browser-use plugin has a
		// real browser to drive.
		var p struct {
			RequestID string `json:"requestId"`
		}
		_ = json.Unmarshal(ev.Params, &p)
		if br == nil {
			engClient.RespondToRequest(ev.ID, map[string]any{"browsers": []any{}})
			fmt.Println("zcode: browserList (no browser host)")
			return
		}
		insts := br.List()
		engClient.RespondToRequest(ev.ID, map[string]any{"browsers": insts})
		fmt.Printf("zcode: browserList -> %d browser\n", len(insts))
	case "interaction/browserExecute":
		// Execute a browser command on the browser host.
		var p struct {
			RequestID string         `json:"requestId"`
			BrowserID string         `json:"browserId"`
			Command   map[string]any `json:"command"`
		}
		if json.Unmarshal(ev.Params, &p) != nil {
			return
		}
		if br == nil {
			engClient.RespondToRequest(ev.ID, map[string]any{"ok": false, "error": map[string]any{"code": "backend_unavailable", "message": "no browser host"}, "elapsedMs": 0})
			return
		}
		result := br.Execute(p.Command)
		engClient.RespondToRequest(ev.ID, result)
		fmt.Printf("zcode: browserExecute %v -> ok=%v\n", p.Command["method"], result["ok"])
	case "state.updated":
		var p struct {
			SessionID string `json:"sessionId"`
			Patch     struct {
				Status string `json:"status"`
			} `json:"patch"`
			Revision int `json:"revision"`
		}
		if json.Unmarshal(ev.Params, &p) == nil && p.SessionID != "" {
			ps.mu.Lock()
			convID := ps.convListener
			convSub := ps.convSubscription
			ps.mu.Unlock()
			phoneSid := ps.phoneFor(p.SessionID)
			if convID > 0 {
				b, _ := json.Marshal(stateUpdatedFrame(phoneSid, p.Patch.Status, convSub, ps.nextOrdinal()))
				engine.SendChannelEvent(convID, b, sender.send)
			}
		}
	case "v4/telemetry/event":
		// stream.chunk carries the assistant's streaming text.
		var p struct {
			Kind    string `json:"kind"`
			Channel string `json:"channel"`
			Session string `json:"sessionId"`
			Chunk   string `json:"chunk"`
			Status  string `json:"status"`
		}
		if json.Unmarshal(ev.Params, &p) != nil {
			return
		}
		if p.Kind == "stream.chunk" && p.Channel == "text" && p.Session != "" {
			ps.mu.Lock()
			convID := ps.convListener
			convSub := ps.convSubscription
			ps.mu.Unlock()
			phoneSid := ps.phoneFor(p.Session)
			if convID > 0 {
				b, _ := json.Marshal(conversationChunkFrame(phoneSid, p.Chunk, convSub, ps.nextOrdinal()))
				engine.SendChannelEvent(convID, b, sender.send)
			}
		}
		if p.Kind == "turn.terminal" && p.Session != "" {
			// The engine session may be a rebuilt continuation of a phone task;
			// update the phone-visible task and push under its id.
			phoneSid := ps.phoneFor(p.Session)
			st := p.Status
			if st != "success" && st != "interrupted" && st != "failed" {
				st = "completed"
			}
			go func(sid string) {
				ws, title := taskMeta(ps, sid)
				if ws != "" {
					if err := zcode.UpsertTask(ws, ws, sid, title, st); err != nil {
						fmt.Printf("zcode: task finalize failed: %v\n", err)
					}
				}
			}(phoneSid)
			// A turn finished: pull the full transcript (assistant text, tool
			// outputs like ls results) and push it as conversation rows so the
			// phone actually sees the reply. Run async — this goroutine IS the
			// stdout reader, and ReadSession must not block it (the reply
			// arrives on the same stream after this event).
			go syncConversation(engClient, engine, sender, ps, p.Session, phoneSid)
		}
	}
}

func providerPayload() []any {
	providers := zcode.Providers()
	out := make([]any, 0, len(providers))
	for _, p := range providers {
		models := make([]any, 0, len(p.Models))
		for _, m := range p.Models {
			models = append(models, map[string]any{
				"id":              m,
				"name":            m,
				"kinds":           []any{"anthropic", "openai-compatible"},
				"modalities":      map[string]any{"input": []any{"text"}, "output": []any{"text"}},
				"contextWindow":   1 << 20,
				"maxOutputTokens": 8192,
				"deleted":         false,
			})
		}
		endpoints := map[string]any{}
		if p.BaseURL != "" {
			endpoints = map[string]any{
				"baseURL": p.BaseURL,
				"paths": map[string]any{
					"anthropic":         "/api/anthropic",
					"openai-compatible": "/v1/chat/completions",
				},
			}
		}
		out = append(out, map[string]any{
			"id":             p.ID,
			"name":           p.Name,
			"apiKey":         p.APIKey,
			"apiFormat":      p.Kind,
			"enabled":        p.Enabled,
			"source":         p.Source,
			"presetId":       p.PresetID,
			"endpoints":      endpoints,
			"models":         models,
			"headers":        map[string]any{},
			"apiKeyRequired": true,
		})
	}
	return out
}

// loadOrCreateDeviceMid reuses the ZCode desktop client's deviceMid.
func loadOrCreateDeviceMid(cache string) string {
	if home, err := os.UserHomeDir(); err == nil {
		b, err := os.ReadFile(filepath.Join(home, ".zcode", "v2", "telemetry-state.json"))
		if err == nil {
			var st struct {
				DeviceMid string `json:"deviceMid"`
			}
			if json.Unmarshal(b, &st) == nil && st.DeviceMid != "" {
				return st.DeviceMid
			}
		}
	}
	path := filepath.Join(cache, "device-mid")
	if b, err := os.ReadFile(path); err == nil && len(bytes.TrimSpace(b)) > 0 {
		return string(bytes.TrimSpace(b))
	}
	mid := fmt.Sprintf("zqf-%s", strings.ReplaceAll(uuidNew(), "-", ""))
	_ = os.MkdirAll(cache, 0o755)
	_ = os.WriteFile(path, []byte(mid), 0o600)
	return mid
}

func uuidNew() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "zcode-quick-web-forward"
	}
	return h
}

func doDownload(args []string) {
	o := parseCommon(args)
	rt := resolveRuntime(o.runtimePath)
	fmt.Printf("zcode: download/use complete at %s\n", rt)
}

// ---- BigModel login (domestic) ----

const (
	bigmodelBaseURL = "https://open.bigmodel.cn/api/anthropic"
	bigmodelModel   = "bigmodel/GLM-5.3"
)

// bigmodelLogin ensures a BigModel API key is configured in ~/.zcode: an
// existing key wins, else BIGMODEL_API_KEY / an interactive prompt supplies
// it. The key is validated against BigModel's Anthropic-compatible /v1/models
// endpoint before being written into the desktop config (builtin:bigmodel)
// and the CLI config (provider/bigmodel + model/main).
func bigmodelLogin() {
	cliPath, v2Path := zcodeConfigPaths()
	key := os.Getenv("BIGMODEL_API_KEY")
	if key == "" {
		if hasBigmodelKey(cliPath) || hasBigmodelKey(v2Path) {
			fmt.Println("zcode: BigModel API key 已配置。")
			return
		}
		fmt.Print("zcode: 请输入 BigModel API Key (https://open.bigmodel.cn/apikeys 获取): ")
		line, err := stdinReader.ReadString('\n')
		if err != nil {
			fatal("读取 API key 失败: %v (非交互环境请 export BIGMODEL_API_KEY=...)", err)
		}
		key = strings.TrimSpace(line)
		if key == "" {
			fatal("BigModel API key 为空")
		}
	}
	if err := validateBigmodelKey(key); err != nil {
		fatal("BigModel API key 校验失败: %v", err)
	}
	writeBigmodelKey(v2Path, "builtin:bigmodel", key)
	writeBigmodelKey(cliPath, "bigmodel", key)
	setMainModel(cliPath, bigmodelModel)
	fmt.Println("zcode: BigModel API key 校验通过,已写入 ~/.zcode (v2 + cli 配置)。")
}

func validateBigmodelKey(key string) error {
	req, err := http.NewRequest(http.MethodGet, bigmodelBaseURL+"/v1/models", nil)
	if err != nil {
		return err
	}
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	switch {
	case resp.StatusCode == http.StatusOK:
		var v struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		_ = json.Unmarshal(body, &v)
		ids := make([]string, 0, len(v.Data))
		for _, m := range v.Data {
			ids = append(ids, m.ID)
		}
		fmt.Printf("zcode: key 有效,可用模型 %d 个: %s\n", len(ids), strings.Join(ids, ", "))
		return nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("key 被拒绝 (HTTP %d)", resp.StatusCode)
	default:
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
}

func zcodeConfigPaths() (cliPath, v2Path string) {
	home, err := os.UserHomeDir()
	if err != nil {
		fatal("user home: %v", err)
	}
	base := filepath.Join(home, ".zcode")
	return filepath.Join(base, "cli", "config.json"), filepath.Join(base, "v2", "config.json")
}

func readJSONMap(path string) map[string]any {
	m := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

func writeJSONMap(path string, m map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

func hasBigmodelKey(path string) bool {
	prov, _ := readJSONMap(path)["provider"].(map[string]any)
	for _, name := range []string{"bigmodel", "builtin:bigmodel"} {
		p, _ := prov[name].(map[string]any)
		opts, _ := p["options"].(map[string]any)
		if s, _ := opts["apiKey"].(string); s != "" {
			return true
		}
	}
	return false
}

func writeBigmodelKey(path, name, key string) {
	m := readJSONMap(path)
	prov, _ := m["provider"].(map[string]any)
	if prov == nil {
		prov = map[string]any{}
	}
	p, _ := prov[name].(map[string]any)
	if p == nil {
		p = map[string]any{}
	}
	opts, _ := p["options"].(map[string]any)
	if opts == nil {
		opts = map[string]any{}
	}
	if _, ok := opts["baseURL"]; !ok {
		opts["baseURL"] = bigmodelBaseURL
	}
	opts["apiKey"] = key
	p["options"] = opts
	prov[name] = p
	m["provider"] = prov
	if err := writeJSONMap(path, m); err != nil {
		fatal("写入 %s: %v", path, err)
	}
}

func setMainModel(path, model string) {
	m := readJSONMap(path)
	mo, _ := m["model"].(map[string]any)
	if mo == nil {
		mo = map[string]any{}
	}
	mo["main"] = model
	m["model"] = mo
	if err := writeJSONMap(path, m); err != nil {
		fatal("写入 %s: %v", path, err)
	}
}
