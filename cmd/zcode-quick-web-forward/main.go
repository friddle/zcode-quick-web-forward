// Command zcode-quick-web-forward is a pure-CLI driver that logs into ZCode,
// exposes real workspaces/tasks from the local ZCode installation, and mints a
// phone pairing URL on ZCode's own web-remote relay. It only drives the relay
// and answers the phone's channel services from real ZCode state (task index,
// model-provider config, settings) — the reverse-engineered stub approach is
// gone.
package main

import (
	"fmt"
	"os"
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
