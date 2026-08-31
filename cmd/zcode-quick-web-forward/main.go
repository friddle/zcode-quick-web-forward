// Command zcode-quick-web-forward is a thin, pure-CLI driver around the
// official ZCode runtime (glm/zcode.cjs). It mimics exactly what the ZCode
// desktop/client does by invoking the runtime's own subcommands:
//
//  1. resolves the newest ZCode runtime (downloads glm/zcode.cjs if needed)
//  2. login  -> calls `node zcode.cjs login --oauth --no-browser` (real client
//     login), prints the authorize URL, waits for the browser callback / Enter
//  3. app-server -> calls `node zcode.cjs app-server` (the ZCode engine)
//  4. remote -> surfaces ZCode's own web-remote URL (https://zcode.z.ai/remote/…)
//     which is the mobile access link
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
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/friddle/zcode-quick-web-forward/internal/runtime"
	"github.com/friddle/zcode-quick-web-forward/internal/webremote"
)

const version = "0.4.1"

var urlRe = regexp.MustCompile(`https?://[A-Za-z0-9._/\-?&=:%#~+{}$]+`)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "run":
			runLoginCLI(os.Args[2:])
			return
		case "logincli", "login":
			runLoginCLI(os.Args[2:])
			return
		case "app", "app-server":
			runAppServer(os.Args[2:])
			return
		case "download", "fetch":
			doDownload(os.Args[2:])
			return
		case "remote", "web":
			doRemote(os.Args[2:])
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

Invokes the official ZCode runtime (glm/zcode.cjs) subcommands directly,
mimicking the desktop client's own calls.

usage: zcode-quick-web-forward [command] [flags]

Commands:
  run            full flow used by the installer: login, then app-server
                 plus the web-remote/mobile link (same as logincli)
  logincli       run the real client login: node zcode.cjs login --oauth
                 --no-browser ; prints authorize URL, waits for callback/Enter
  app-server     run the ZCode engine: node zcode.cjs app-server
  remote|web     start app-server and print ZCode's own web-remote/mobile URL
  download       resolve/download the latest ZCode runtime
  version        print version

Flags:
  --runtime-path PATH   explicit glm runtime dir (env ZCODE_RUNTIME_PATH)
  --node PATH           node binary (env ZCODE_NODE), needs >=22.5`)
}

func parseCommon(args []string) (runtimePath, node string) {
	fs := flag.NewFlagSet("zqf", flag.ContinueOnError)
	fs.StringVar(&runtimePath, "runtime-path", "", "glm runtime dir; auto if empty")
	fs.StringVar(&node, "node", "", "node binary")
	_ = fs.Parse(args)
	if node == "" {
		node = os.Getenv("ZCODE_NODE")
	}
	if node == "" {
		node = "node"
	}
	if v := os.Getenv("ZCODE_RUNTIME_PATH"); v != "" && runtimePath == "" {
		runtimePath = v
	}
	return runtimePath, node
}

func resolveRuntime(runtimePath string) (dir string) {
	dir, _, err := runtime.NewFinder().Resolve(runtimePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "zcode: runtime resolve failed: %v\n", err)
		os.Exit(1)
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

// runCLI spawns node <script> <args...>, forwarding stdout/stderr to the
// console (pure-CLI, mimics the client) and capping how far it waits.
func runCLI(node, script string, args []string) {
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
	_ = cmd.Run()
}

// runLoginCLI prints the authorize URL, runs the real client login command,
// waits for Enter, then prints ZCode's web-remote hint.
func runLoginCLI(args []string) {
	runtimePath, node := parseCommon(args)
	rt := resolveRuntime(runtimePath)
	fmt.Println("zcode: 启动官方登录命令: login --no-browser")
	fmt.Println("zcode: 会打印登录链接 -> 请在浏览器打开 -> 授权回调 -> 回车确认。")
	args2 := []string{"login", "--no-browser"}
	fmt.Printf("zcode: $ %s %s %s\n", node, scriptPath(rt), strings.Join(args2, " "))
	runCLI(node, scriptPath(rt), args2)
	fmt.Println("zcode: 登录命令结束。按 Enter 继续或再次登录。")
	waitEnter()
	fmt.Println("zcode: 登录完成确认。启动 app-server 并获取 web-remote 链接。")
	doRemote(args)
}

func doRemote(args []string) {
	runtimePath, node := parseCommon(args)
	rt := resolveRuntime(runtimePath)
	_ = rt
	origin := os.Getenv("ZCODE_BASE_URL")
	if origin == "" {
		origin = "https://zcode.z.ai"
	}

	// Register on the official web-remote relay and mint a real pairing URL
	// (same QR the desktop's "continue on your phone" uses). Runs alongside
	// the engine; a relay failure never blocks the local app-server.
	go startWebRemote(origin)

	fmt.Println("zcode: 启动 ZCode engine (app-server)...")
	runCLI(node, scriptPath(rt), []string{"app-server"})
}

// startWebRemote registers this machine as a web-remote relay device and
// prints the phone pairing URL (plus a terminal QR code when qrencode is
// installed).
func startWebRemote(origin string) {
	cache, err := os.UserCacheDir()
	if err == nil {
		cache = filepath.Join(cache, "zcode-quick-web-forward")
	}
	opts := webremote.Options{
		Origin:     origin,
		DeviceMid:  loadOrCreateDeviceMid(cache),
		DeviceName: hostname(),
		AppVersion: version,
		StatePath:  filepath.Join(cache, "webremote-state.json"),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	webremote.Run(ctx, opts, webremote.Handler{
		OnReady: func(s webremote.Session) {
			fmt.Println()
			fmt.Println("==========================================")
			fmt.Println("  ZCode web-remote / 手机配对链接(手机浏览器打开):")
			fmt.Println("  " + s.PhoneURL)
			fmt.Printf("  (relay %s,device %s)\n", origin, s.DeviceSid)
			fmt.Println("  国内网络可 export ZCODE_BASE_URL=https://zcode.chatglm.site")
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
		},
		OnData: handleRemoteData,
	})
}

// handleRemoteData answers the phone's bridge requests. bootstrap/workspace
// listing is implemented so the phone gets past its splash; opening a
// workspace (workspace-bridge-open + rpc-frame stream to the engine) is not
// bridged yet in the CLI.
func handleRemoteData(payload json.RawMessage, reply func(any)) {
	var p struct {
		ZcodeType string `json:"zcode_type"`
		RequestID string `json:"requestId"`
	}
	if json.Unmarshal(payload, &p) != nil || p.RequestID == "" {
		return
	}
	cwd, _ := os.Getwd()
	ws := map[string]any{
		"workspacePath":   cwd,
		"label":           filepath.Base(cwd),
		"kind":            "local",
		"connectionState": "connected",
	}
	switch p.ZcodeType {
	case "bootstrap-request":
		reply(map[string]any{
			"zcode_type": "bootstrap-response", "requestId": p.RequestID, "success": true,
			"result": map[string]any{
				"windowControlSessionId": "zqf",
				"workspaces":             []any{ws},
				"tasks":                  []any{},
			},
		})
	case "workspace-list-request":
		reply(map[string]any{
			"zcode_type": "workspace-list-response", "requestId": p.RequestID, "success": true,
			"result": map[string]any{"workspaces": []any{ws}, "tasks": []any{}},
		})
	case "workspace-bridge-open":
		reply(map[string]any{
			"zcode_type": "workspace-bridge-error", "requestId": p.RequestID,
			"reason": "unsupported", "error": "CLI bridge does not relay engine sessions yet",
		})
	}
}

// loadOrCreateDeviceMid reuses the telemetry deviceMid the ZCode desktop
// client / runtime already generated, so the relay sees a stable machine id.
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

func runAppServer(args []string) {
	runtimePath, node := parseCommon(args)
	rt := resolveRuntime(runtimePath)
	runCLI(node, scriptPath(rt), []string{"app-server"})
}

func doDownload(args []string) {
	runtimePath, _ := parseCommon(args)
	rt := resolveRuntime(runtimePath)
	fmt.Printf("zcode: download/use complete at %s\n", rt)
}

func dirOf(p string) string {
	if i := strings.LastIndexAny(p, "/\\"); i >= 0 {
		return p[:i]
	}
	return "."
}

func waitEnter() {
	br := bufio.NewScanner(os.Stdin)
	_ = br.Scan()
}

var _ io.Reader
var _ = strconv.Itoa
