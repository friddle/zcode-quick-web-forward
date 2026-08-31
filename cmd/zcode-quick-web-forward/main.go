// Command zcode-quick-web-forward is a single static binary that, in one shot:
//
//  1. resolves the newest ZCode runtime (downloads glm/zcode.cjs if needed)
//  2. launches the ZCode app-server (node zcode.cjs app-server)
//  3. runs the Z.AI OAuth login: prints the authorize/login link, hosts a local
//     callback server, waits for the browser callback (or Enter), then hands the
//     code to the runtime to complete the login
//  4. starts a local web hub
//  5. forwards the hub out to mobile via a tunnel and prints the link
package main

import (
	"bufio"
	"flag"
	"fmt"
	"github.com/friddle/zcode-quick-web-forward/internal/appserver"
	"github.com/friddle/zcode-quick-web-forward/internal/bridge"
	"github.com/friddle/zcode-quick-web-forward/internal/oauth"
	"github.com/friddle/zcode-quick-web-forward/internal/runtime"
	"github.com/friddle/zcode-quick-web-forward/internal/tunnel"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const version = "0.2.0"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "run", "up":
			run(os.Args[2:])
			return
		case "download", "fetch":
			doDownload(os.Args[2:])
			return
		case "serve":
			doServe(os.Args[2:])
			return
		case "login":
			doLogin(os.Args[2:])
			return
		case "version", "-version", "--version", "-v":
			fmt.Printf("zcode-quick-web-forward %s\n", version)
			return
		case "help", "-h", "--help":
			printHelp()
			return
		}
	}
	run(os.Args[1:])
}

func printHelp() {
	fmt.Println(`zcode-quick-web-forward - one-shot ZCode web/mobile forward

usage: zcode-quick-web-forward [command] [flags]

Commands (default: run):
  run|up       full flow: resolve runtime -> spawn app-server -> OAuth login ->
               confirm -> local hub -> mobile tunnel -> print link
  download     only resolve/download the latest ZCode runtime
  serve        spawn app-server + local web hub (no OAuth / tunnel)
  login        OAuth login only (build link, host callback, confirm)
  version      print version

Flags:
  --runtime-path PATH   explicit glm runtime dir (env ZCODE_RUNTIME_PATH)
  --port PORT           local hub port           (default: ephemeral)
  --callback-port PORT  local OAuth callback port (default: ephemeral)
  --host HOST           local bind host          (default 127.0.0.1)
  --tunnel MODE         none|local|piko|ssh|auto (default auto)
  --tunnel-cmd "CMD"    tunnel command override
  --node PATH           node binary              (env ZCODE_NODE)`)
}

func parseFlags(args []string) (runtimePath, host, tunnelMode string, port, cbPort int, tunnelCmd []string) {
	fs := flag.NewFlagSet("zqf", flag.ContinueOnError)
	fs.StringVar(&runtimePath, "runtime-path", "", "glm runtime dir (zcode.cjs parent); auto if empty")
	fs.IntVar(&port, "port", 0, "local hub port")
	fs.IntVar(&cbPort, "callback-port", 0, "local OAuth callback port")
	fs.StringVar(&host, "host", "127.0.0.1", "local bind host")
	fs.StringVar(&tunnelMode, "tunnel", "auto", "none|local|piko|ssh|auto")
	tunnelCmdStr := fs.String("tunnel-cmd", "", "tunnel command override")
	_ = fs.Parse(args)
	if *tunnelCmdStr != "" {
		tunnelCmd = strings.Fields(*tunnelCmdStr)
	}
	if v := os.Getenv("ZCODE_RUNTIME_PATH"); v != "" && runtimePath == "" {
		runtimePath = v
	}
	return runtimePath, host, tunnelMode, port, cbPort, tunnelCmd
}

func resolveRuntime(runtimePath string) (dir, ver string) {
	dir, ver, err := runtime.NewFinder().Resolve(runtimePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "zcode: runtime resolve failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("zcode: runtime ready at %s (%s)\n", dir, ver)
	return dir, ver
}

// startOAuth runs the Z.AI OAuth login: builds the authorize link, hosts the
// callback server, prints the login link + login page, waits for the browser
// callback (or Enter), then hands the code to the runtime to complete login.
// It returns true on confirmed login.
func startOAuth(node, rtDir, host string, cbPort int) bool {
	cb, err := oauth.StartCallback(host, cbPort)
	if err != nil {
		fmt.Fprintf(os.Stderr, "zcode: oauth callback bind failed: %v\n", err)
		os.Exit(1)
	}
	defer cb.Stop()
	state := oauth.State()
	cb.SetState(state)

	authorizeURL := oauth.AuthorizeURL(state, cb.CallbackURI())
	loginURL := cb.LoginURI()

	go cb.Serve()

	fmt.Println()
	fmt.Println("==========================================")
	fmt.Println("  ZCode OAuth 登录")
	fmt.Println()
	fmt.Println("  authorize / login 链接:")
	fmt.Println("  " + authorizeURL)
	fmt.Println()
	fmt.Println("  本机登录页(有点击登录按钮):")
	fmt.Println("  " + loginURL)
	fmt.Println()
	fmt.Println("  请在浏览器打开任一链接并点击登录/授权。")
	fmt.Println("  授权完成后会自动回调确认（或在此按 Enter 确认）。")
	fmt.Println("==========================================")
	fmt.Println()

	select {
	case res := <-cb.Done():
		if !res.Confirmed {
			fmt.Println("zcode: 登录失败。")
			return false
		}
		fmt.Println("zcode: 收到授权回调，正在完成登录…")
		ok, err := oauth.Complete(node, rtDir, map[string]string{
			"callbackUrl": res.CallbackURL,
			"state":       res.State,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "zcode: complete login: %v\n", err)
		}
		fmt.Printf("zcode: 登录完成: %v\n", ok)
		return ok
	case <-waitEnter():
		fmt.Println("zcode: 等待时长结束，可按 Enter 后重跑，或先手动完成浏览器授权。")
		return false
	}
}

// waitEnter returns a channel closed when the user presses Enter on stdin.
func waitEnter() <-chan struct{} {
	c := make(chan struct{})
	go func() {
		_ = scanEnter(os.Stdin)
		close(c)
	}()
	return c
}

// scanEnter reads one line (or until EOF) from an io.Reader via bufio.
func scanEnter(r io.Reader) error {
	br := bufio.NewScanner(r)
	_ = br.Scan()
	return br.Err()
}

func run(args []string) {
	runtimePath, host, tunnelMode, port, cbPort, tunnelCmd := parseFlags(args)
	node := runtime.Node()
	rtDir, _ := resolveRuntime(runtimePath)

	// step 2: spawn the ZCode app-server
	srv, err := appserver.New(appserver.Options{RuntimeDir: rtDir})
	if err != nil {
		fmt.Fprintf(os.Stderr, "zcode: app-server spawn failed: %v\n", err)
		os.Exit(1)
	}
	defer srv.Close()
	fmt.Println("zcode: ZCode app-server (glm/zcode.cjs app-server) started.")

	// local web hub
	hub, err := bridge.NewHub(host, port)
	if err != nil {
		fmt.Fprintf(os.Stderr, "zcode: hub bind failed: %v\n", err)
		os.Exit(1)
	}
	defer hub.Stop()
	hub.Set(func(s *bridge.State) { s.Runtime = rtDir })
	actualPort := portOf(hub)
	localURL := fmt.Sprintf("http://%s:%d", host, actualPort)
	hub.Set(func(s *bridge.State) { s.LocalURL = localURL })
	go hub.Serve()

	// step 3: OAuth login
	fmt.Println("zcode: 本机 Web 界面: " + localURL)
	confirmed := startOAuth(node, rtDir, host, cbPort)
	hub.Set(func(s *bridge.State) { s.Confirmed = confirmed })

	// step 5: mobile / remote tunnel + print link
	mobileURL, mode, err := tunnel.Start(tunnel.Options{
		LocalURL: localURL,
		Mode:     tunnelMode,
		Command:  tunnelCmd,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "zcode: tunnel start failed: %v\n", err)
		os.Exit(1)
	}
	hub.Set(func(s *bridge.State) { s.MobileURL = mobileURL })
	tunnel.Print("手机/远程", mobileURL)
	fmt.Printf("zcode: tunnel mode=%s  (Ctrl+C 退出)\n", mode)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	fmt.Println("zcode: 运行中。按 Ctrl+C 结束。")
	<-sig
	fmt.Println("zcode: 已退出。")
}

func portOf(h *bridge.Hub) int {
	_, pStr, _ := strings.Cut(h.Addr(), ":")
	p, _ := strconv.Atoi(pStr)
	return p
}

func doDownload(args []string) {
	runtimePath, _, _, _, _, _ := parseFlags(args)
	_, ver := resolveRuntime(runtimePath)
	fmt.Printf("zcode: download/use complete (version %s)\n", ver)
}

func doServe(args []string) {
	runtimePath, host, _, port, _, _ := parseFlags(args)
	rtDir, _ := resolveRuntime(runtimePath)
	srv, err := appserver.New(appserver.Options{RuntimeDir: rtDir})
	if err != nil {
		fmt.Fprintf(os.Stderr, "zcode: %v\n", err)
		os.Exit(1)
	}
	defer srv.Close()
	hub, err := bridge.NewHub(host, port)
	if err != nil {
		fmt.Fprintf(os.Stderr, "zcode: %v\n", err)
		os.Exit(1)
	}
	defer hub.Stop()
	hub.Set(func(s *bridge.State) { s.Runtime = rtDir })
	p := portOf(hub)
	hub.Set(func(s *bridge.State) { s.LocalURL = fmt.Sprintf("http://%s:%d", host, p) })
	go hub.Serve()
	fmt.Printf("zcode: serving web hub on http://%s:%d\n", host, p)
	go func() {
		time.Sleep(1 * time.Hour)
	}()
	select {}
}

func doLogin(args []string) {
	runtimePath, host, _, _, cbPort, _ := parseFlags(args)
	node := runtime.Node()
	rtDir, _ := resolveRuntime(runtimePath)
	confirmed := startOAuth(node, rtDir, host, cbPort)
	fmt.Printf("zcode: login confirmed: %v\n", confirmed)
}
