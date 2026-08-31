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
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/friddle/zcode-quick-web-forward/internal/runtime"
)

const version = "0.3.0"

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
	fmt.Println("zcode: 启动 ZCode engine (app-server)...")
	fmt.Println()
	fmt.Println("==========================================")
	fmt.Println("  ZCode web-remote / 手机远程访问：")
	fmt.Printf("  %s/remote/v4?id=<session>\n", origin)
	fmt.Println("  (会话由 app-server 经 relay 建立；国内网络可")
	fmt.Println("   export ZCODE_BASE_URL=https://zcode.chatglm.site)")
	fmt.Println("==========================================")
	runCLI(node, scriptPath(rt), []string{"app-server"})
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
