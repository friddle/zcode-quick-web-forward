// Engine + relay bring-up for `remote`: single-instance flock, runtime/
// node/browser/terminal services, engine (re)start supervision, the
// persistent `workspace` subcommand and relay origin probing.

package main

import (
	"bufio"
	"encoding/json"
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

	enginepkg "github.com/friddle/zcode-quick-web-forward/internal/engine"
	"github.com/friddle/zcode-quick-web-forward/internal/relay"
	"github.com/friddle/zcode-quick-web-forward/internal/terminal"
	"github.com/friddle/zcode-quick-web-forward/internal/zcode"
)

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

	ps := &phoneSessions{workspaces: workspaces}
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
				// The dead engine's in-flight turns never emit turn.terminal;
				// lingering running flags would ghost-block queue dispatch.
				ps.clearAllTurnRunning()
				// A fresh engine can pick up globally queued tasks again.
				go func() {
					time.Sleep(3 * time.Second)
					dispatchGlobalQueue(engClient, engine, sender.send, ps)
				}()
			}()
	}
	// Official-host mode: the host spawns and owns the engine itself
	// (ZCODE_AGENT_SERVER_COMMAND); our engine supervision stays idle.
	restartEngineFn := startEngine
	var officialStarted bool
	if os.Getenv("ZCODE_OFFICIAL_HOST") != "" {
		cache, _ := os.UserCacheDir()
		mid := loadOrCreateDeviceMid(filepath.Join(cache, "zcode-quick-web-forward"))
		defWS := ""
		if len(workspaces) > 0 {
			defWS = workspaces[0]
		}
		officialStarted = maybeStartOfficialHost(engine, sender, node, scriptPath(rt), defWS, mid)
		if officialStarted {
			// The host owns the engine — a workspace bridge-open must NOT
			// restart it (the old restart-on-open semantics killed the host
			// mid-pairing and stalled the sync).
			restartEngineFn = func() {}
		}
	}
	if !officialStarted {
		startEngine()
	}

	go func() {
		if err := <-engineExited; err != nil && atomic.LoadInt32(&shuttingDown) == 0 {
			fmt.Fprintf(os.Stderr, "\nzcode: ERROR: ZCode engine exited: %v\n", err)
			fmt.Fprintf(os.Stderr, "zcode: 常见原因: node 版本过低 (需要 >= 22.5, 支持 node:sqlite)。\n")
			fmt.Fprintf(os.Stderr, "zcode: 可用 --node / ZCODE_NODE 指定 node。\n")
			os.Exit(1)
		}
	}()

	go startWebRemote(origin, region, engine, sender, restartEngineFn, workspaces, ps, engClient, termSvc)
	go watchStoredWorkspaces(o, ps, func(ws []string) {
		sender.send(workspaceListPush(ws, ps))
	})

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	atomic.StoreInt32(&shuttingDown, 1)
	// Tear down the engine (node app-server) and the browser host so a stopped
	// daemon doesn't leave orphan children behind.
	engMu.Lock()
	if engCmd != nil && engCmd.Process != nil {
		_ = engCmd.Process.Kill()
	}
	engMu.Unlock()
	if br != nil {
		br.Close()
	}
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
// Degraded networks regularly push TLS setup past 5s, so tolerate 15s per
// attempt and retry — a slow origin beats refusing to start.
func probeOrigin(origin string) error {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
		}
		c := &http.Client{Timeout: 15 * time.Second}
		resp, err := c.Get(strings.TrimRight(origin, "/") + "/")
		if err != nil {
			lastErr = err
			continue
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
		if resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			continue
		}
		return nil
	}
	return lastErr
}

// watchStoredWorkspaces polls the stored-workspace file (written by
// `workspace add`) and hot-reloads the running remote when it changes: the
// phone session picks up the new list and a workspace-list push goes out, so
// no daemon restart (and no phone kick) is needed. Polling rather than
// inotify keeps it portable; the file is tiny, and re-resolving only happens
// on an actual content change.
func watchStoredWorkspaces(o commonOpts, ps *phoneSessions, push func([]string)) {
	path := zcode.StoredWorkspacesPath()
	last := ""
	if b, err := os.ReadFile(path); err == nil {
		last = string(b)
	}
	for {
		time.Sleep(2 * time.Second)
		b, err := os.ReadFile(path)
		if err != nil || string(b) == last {
			continue
		}
		last = string(b)
		ws := o.resolveWorkspaces()
		ps.setWorkspaces(ws)
		fmt.Printf("zcode: workspaces updated (hot reload): %s\n", strings.Join(ws, ", "))
		if push != nil {
			push(ws)
		}
	}
}
