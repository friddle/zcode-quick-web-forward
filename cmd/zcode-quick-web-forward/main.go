// Command zcode-quick-web-forward is a single static binary that, in one shot:
//
//  1. resolves the newest ZCode runtime (downloads glm/zcode.cjs if needed)
//  2. launches the ZCode app-server (node zcode.cjs app-server)
//  3. prints the OAuth login link and waits for confirmation (Enter / event)
//  4. starts a local web hub
//  5. forwards the hub out to mobile via a tunnel and prints the link
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/friddle/zcode-quick-web-forward/internal/appserver"
	"github.com/friddle/zcode-quick-web-forward/internal/bridge"
	"github.com/friddle/zcode-quick-web-forward/internal/login"
	"github.com/friddle/zcode-quick-web-forward/internal/runtime"
	"github.com/friddle/zcode-quick-web-forward/internal/tunnel"
)

const version = "0.1.0"

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
  run|up       full flow: resolve runtime → spawn app-server → login link →
               confirm → local hub → mobile tunnel → print link
  download     only resolve/download the latest ZCode runtime
  serve        spawn app-server + local web hub (no tunnel)
  login        spawn app-server, show & wait for the login link
  version      print version

Flags:
  --runtime-path PATH   explicit glm runtime dir (ZCODE_RUNTIME_PATH)
  --port PORT           local hub port (default: ephemeral)
  --host HOST           local hub bind host (default 127.0.0.1)
  --tunnel MODE         none|local|piko|ssh|auto (default auto)
  --tunnel-cmd \"CMD\"    tunnel command override
  --node PATH           node binary (ZCODE_NODE)`)
}

func parseFlags(args []string) (runtimePath, host, tunnelMode string, port int, tunnelCmd []string) {
	fs := flag.NewFlagSet("zqf", flag.ContinueOnError)
	fs.StringVar(&runtimePath, "runtime-path", "", "glm runtime dir (zcode.cjs parent); auto if empty")
	fs.IntVar(&port, "port", 0, "local hub port")
	fs.StringVar(&host, "host", "127.0.0.1", "local hub bind host")
	fs.StringVar(&tunnelMode, "tunnel", "auto", "none|local|piko|ssh|auto")
	tunnelCmdStr := fs.String("tunnel-cmd", "", "tunnel command override")
	_ = fs.Parse(args)
	if *tunnelCmdStr != "" {
		tunnelCmd = strings.Fields(*tunnelCmdStr)
	}
	if v := os.Getenv("ZCODE_RUNTIME_PATH"); v != "" && runtimePath == "" {
		runtimePath = v
	}
	return runtimePath, host, tunnelMode, port, tunnelCmd
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

func isLoginURL(u string) bool {
	lower := strings.ToLower(u)
	for _, w := range []string{"login", "oauth", "authorize", "device", "signin", "sign-in", "auth", "callback"} {
		if strings.Contains(lower, w) {
			return true
		}
	}
	return false
}

func run(args []string) {
	runtimePath, host, tunnelMode, port, tunnelCmd := parseFlags(args)

	rtDir, rtVer := resolveRuntime(runtimePath)

	// step 2: spawn the ZCode app-server
	srv, err := appserver.New(appserver.Options{RuntimeDir: rtDir})
	if err != nil {
		fmt.Fprintf(os.Stderr, "zcode: app-server spawn failed: %v\n", err)
		os.Exit(1)
	}
	defer srv.Close()
	fmt.Println("zcode: ZCode app-server (glm/zcode.cjs app-server) started; spawning login flow…")

	// local hub
	hub, err := bridge.NewHub(host, port)
	if err != nil {
		fmt.Fprintf(os.Stderr, "zcode: hub bind failed: %v\n", err)
		os.Exit(1)
	}
	defer hub.Stop()
	hub.Set(func(s *bridge.State) { s.Version = rtVer; s.Runtime = rtDir })
	actualPort := portOf(hub)
	localURL := fmt.Sprintf("http://%s:%d", host, actualPort)
	hub.Set(func(s *bridge.State) { s.LocalURL = localURL })

	// login handler wired into the app-server stdout stream
	lh := login.NewHandler(srv)
	srv.OnLine = func(line string) {
		lh.Line(line)
		for _, u := range appserver.FindURLs(line) {
			hub.Set(func(s *bridge.State) {
				if s.LoginURL == "" && isLoginURL(u) {
					s.LoginURL = u
				}
			})
		}
		hub.Set(func(s *bridge.State) {
			s.Confirmed = lh.Confirmed
			if s.Confirmed {
				s.Message = "已登录，正在准备远程链接…"
			}
		})
	}

	// step 3: wait briefly for the login link, then print the flow
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) && lh.LoginURL == "" {
		if !srv.Started {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	hub.Set(func(s *bridge.State) { s.Confirmed = lh.Confirmed })

	fmt.Println()
	fmt.Println("==========================================")
	fmt.Println("  ZCode 登录")
	fmt.Println()
	if lh.LoginURL == "" {
		fmt.Println("  (尚未检测到登录链接，登录页会稍后更新；按 Enter 刷新)")
	} else {
		fmt.Println("  请用浏览器打开：")
		fmt.Println()
		fmt.Println("  " + lh.LoginURL)
	}
	fmt.Println()
	fmt.Println("  登录完成后按 Enter 确认（或等待自动检测到登录成功）。")
	fmt.Println("==========================================")
	fmt.Println()
	fmt.Printf("  本机 Web 界面: %s\n\n", localURL)

	go hub.Serve()

	// step 4: confirm login (auto-success event or Enter)
	confirmed := lh.Wait()
	hub.Set(func(s *bridge.State) { s.Confirmed = confirmed })

	// step 5: mobile / remote tunnel, print the link
	mobileURL, mode, err := tunnel.Start(tunnel.Options{
		LocalURL: localURL,
		Mode:     tunnelMode,
		Command:  tunnelCmd,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "zcode: tunnel start failed: %v\n", err)
		os.Exit(1)
	}
	hub.Set(func(s *bridge.State) { s.MobileURL = mobileURL; s.Message = "远程链接已就绪" })
	tunnel.Print("手机/远程", mobileURL)
	fmt.Printf("zcode: tunnel mode=%s (Ctrl+C 退出)\n", mode)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	fmt.Println("zcode: 运行中。按 Ctrl+C 结束。")
	<-sig
	fmt.Println("zcode: 已退出。")
}

func doDownload(args []string) {
	runtimePath, _, _, _, _ := parseFlags(args)
	_, ver := resolveRuntime(runtimePath)
	fmt.Printf("zcode: download/use complete (version %s)\n", ver)
}

func portOf(h *bridge.Hub) int {
	_, pStr, _ := strings.Cut(h.Addr(), ":")
	p, _ := strconv.Atoi(pStr)
	return p
}

func doServe(args []string) {
	runtimePath, host, _, port, _ := parseFlags(args)
	rtDir, ver := resolveRuntime(runtimePath)
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
	hub.Set(func(s *bridge.State) {
		s.Version = ver
		s.Runtime = rtDir
		s.LocalURL = fmt.Sprintf("http://%s:%d", host, portOf(hub))
	})
	lh := login.NewHandler(srv)
	srv.OnLine = func(line string) {
		lh.Line(line)
		hub.Set(func(s *bridge.State) {
			s.Confirmed = lh.Confirmed
			for _, u := range appserver.FindURLs(line) {
				if s.LoginURL == "" && isLoginURL(u) {
					s.LoginURL = u
				}
			}
		})
	}
	fmt.Printf("zcode: web hub at http://%s:%d (login link will appear here)\n", host, portOf(hub))
	go hub.Serve()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) && lh.LoginURL == "" {
		time.Sleep(150 * time.Millisecond)
	}
	if lh.LoginURL != "" {
		fmt.Println("zcode: 登录链接: " + lh.LoginURL)
	}
	confirmed := lh.Wait()
	_ = confirmed
}

func doLogin(args []string) {
	runtimePath, _, _, _, _ := parseFlags(args)
	rtDir, _ := resolveRuntime(runtimePath)
	srv, err := appserver.New(appserver.Options{RuntimeDir: rtDir})
	if err != nil {
		fmt.Fprintf(os.Stderr, "zcode: %v\n", err)
		os.Exit(1)
	}
	defer srv.Close()
	lh := login.NewHandler(srv)
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) && lh.LoginURL == "" {
		time.Sleep(150 * time.Millisecond)
	}
	if lh.LoginURL != "" {
		fmt.Println("登录链接：" + lh.LoginURL)
	} else {
		fmt.Println("尚未检测到登录链接。")
	}
	if lh.Wait() {
		fmt.Println("登录成功。")
	} else {
		fmt.Println("未确认登录。")
	}
}
