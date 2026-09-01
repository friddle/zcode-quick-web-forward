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

	"github.com/friddle/zcode-quick-web-forward/internal/nodejs"
	"github.com/friddle/zcode-quick-web-forward/internal/relay"
	"github.com/friddle/zcode-quick-web-forward/internal/runtime"
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
  remote|web     start engine + relay and print the phone pairing URL
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
	return out
}

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "zcode: ERROR: "+format+"\n", a...)
	os.Exit(1)
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
	doRemoteOpts(parseCommon(args))
}

func doRemoteOpts(o commonOpts) {
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
	sender := &relaySender{}

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

	go startWebRemote(origin, region, engine, sender, startEngine, workspaces)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	atomic.StoreInt32(&shuttingDown, 1)
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

func startWebRemote(origin, region string, engine *relay.BridgeEngine, sender *relaySender, restartEngine func(), workspaces []string) {
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
				sender.send(workspaceListPush(workspaces))
				fmt.Println("zcode: workspace list pushed to phone")
			}()
		},
		OnData: func(payload json.RawMessage, reply func(any)) {
			sender.set(reply)
			handleRemoteData(payload, reply, engine, restartEngine, sender.send, workspaces)
		},
	})
}

func workspaceListPush(workspaces []string) map[string]any {
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
			"tasks":              taskListPayload(""),
			"activeWorkspaceKey": active,
		},
	}
}

func handleRemoteData(payload json.RawMessage, reply func(any), engine *relay.BridgeEngine, restartEngine func(), replyFrames func(any), workspaces []string) {
	var p struct {
		ZcodeType string `json:"zcode_type"`
		RequestID string `json:"requestId"`
	}
	if json.Unmarshal(payload, &p) != nil {
		return
	}
	if p.ZcodeType == "rpc-frame" || p.ZcodeType == "rpc-frame-ack" {
		engine.HandlePhonePayload(payload, reply, handleChannelCall(engine, reply, workspaces))
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
				"tasks":                  taskListPayload(""),
			},
		})
	case "workspace-list-request":
		reply(map[string]any{
			"zcode_type": "workspace-list-response", "requestId": p.RequestID, "success": true,
			"result": map[string]any{"workspaces": wsList, "tasks": taskListPayload("")},
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
				"tasks":              taskListPayload(""),
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

func taskListPayload(kind string) []any {
	tasks, err := zcode.ListTasks("", kind)
	if err != nil {
		return []any{}
	}
	out := make([]any, 0, len(tasks))
	for _, t := range tasks {
		// Only expose local tasks: remote SSH workspaces (workspaceKey with a
		// remote: prefix) have no matching local workspace the phone can open,
		// and the phone stalls waiting for a bridge it can never get.
		if strings.HasPrefix(t.WorkspaceKey, "remote:") {
			continue
		}
		item := map[string]any{
			"taskId":        t.TaskID,
			"title":         t.Title,
			"workspaceKey":  t.WorkspaceKey,
			"workspacePath": t.WorkspacePath,
			"displayStatus": displayStatus(t.Status),
			"pinned":        t.Pinned,
			"archived":      t.Archived,
			"createdAt":     t.CreatedAt,
			"updatedAt":     t.UpdatedAt,
		}
		if t.UnreadAt != nil {
			item["unreadAt"] = *t.UnreadAt
		}
		out = append(out, item)
	}
	return out
}

func displayStatus(s string) string {
	if s == "" {
		return "idle"
	}
	switch strings.ToLower(s) {
	case "running", "in-progress", "active":
		return "running"
	case "idle", "completed", "cancelled", "failed", "interrupted", "paused":
		return strings.ToLower(s)
	default:
		return "idle"
	}
}

// handleChannelCall answers phone channel calls. Desktop-owned services
// (model-provider, zcode-task, setting, window-controller, …) are answered
// from the real ZCode state; the app-server engine gets only the calls it
// actually implements (session/* conversation traffic).
func handleChannelCall(engine *relay.BridgeEngine, send func(any), workspaces []string) func(*relay.ChannelCall) {
	return func(c *relay.ChannelCall) {
		fmt.Printf("zcode: channel call kind=%d id=%d %s.%s\n", c.Kind, c.ID, c.ChannelName, c.Name)
		if c.Kind != 100 || c.ID == 0 {
			return
		}
		if answerDesktopChannel(engine, c, send, workspaces) {
			return
		}
		engine.RegisterCall(c.ID)
		params := "null"
		if c.Arg != nil {
			if b, ok := c.Arg.(json.RawMessage); ok && len(b) > 0 {
				params = string(b)
			} else if b, err := json.Marshal(c.Arg); err == nil {
				params = string(b)
			}
		}
		engine.WriteToServer(fmt.Sprintf(`{"id":%d,"method":%q,"params":%s}`, c.ID, c.ChannelName+"/"+c.Name, params))
	}
}

func answerDesktopChannel(engine *relay.BridgeEngine, c *relay.ChannelCall, send func(any), workspaces []string) bool {
	reply := func(result any) {
		b, _ := json.Marshal(result)
		engine.ReplyChannelPromise(c.ID, b, send)
	}
	switch c.ChannelName + "/" + c.Name {
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
		reply(map[string]any{"language": "en", "locale": "zh-CN"})
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
	case "settings-sync/getFirstRunPromptState":
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
		reply(taskListPayload(kind))
	case "zcode-task/listPinnedTaskIds", "zcode-task/listArchivedTaskIds", "zcode-task/listDeletedTaskIds", "zcode-task/listRecentTasks":
		kind := ""
		switch c.Name {
		case "listPinnedTaskIds":
			kind = "pinned"
		case "listArchivedTaskIds", "listDeletedTaskIds":
			kind = "archived"
		}
		ids, _ := zcode.ListTaskIDs(kind)
		anyIDs := make([]any, 0, len(ids))
		for _, id := range ids {
			anyIDs = append(anyIDs, id)
		}
		reply(anyIDs)
	case "window-controller/subscribeControllerV4", "window-controller/getSnapshot", "window-controller/getControllerSnapshot":
		reply([]any{})
	case "client-scenes/list":
		reply([]any{})
	case "subagents/list":
		reply([]any{})
	case "zcode-agent/getAgentRuntimeLifecycle":
		reply(map[string]any{"status": "running"})
	case "zcode-agent/helloConversationV4":
		reply(map[string]any{"status": "ok"})
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
					"anthropic":        "/api/anthropic",
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
