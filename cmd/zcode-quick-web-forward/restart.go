// The `restart` subcommand: GLOBAL kill of every zcode process on this
// machine (daemon, official host, engine) followed by a clean start. For when
// the phone wedges at Paired. Loading workspace — a full process sweep clears
// stale relay sessions and half-disposed hosts that a plain `systemctl
// restart` may paper over (systemd only restarts the daemon, not strays).

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

func doRestart(args []string) {
	fmt.Println("zcode: global kill zcode-quick-web / zcode-host / zcode-cli ...")
	// comm is truncated to 15 chars by the kernel; match both forms.
	victims := []string{
		"zcode-quick-web", "zcode-quick-web-forward",
		"zcode-host-zcode", "zcode-host-web-remote-host",
		"zcode-cli", "zcode-node-repl-m",
	}
	deadline := time.Now().Add(10 * time.Second)
	self := os.Getpid()
	ppid := os.Getppid()
	for {
		out, _ := exec.Command("pgrep", "-f", strings.Join(victims, "|")).Output()
		live := []string{}
		for _, pid := range strings.Fields(string(out)) {
			// never kill ourselves or our parent shell — pgrep -f matches
			// this very command line ("... zcode-quick-web-forward restart")
			if pid == fmt.Sprint(self) || pid == fmt.Sprint(ppid) {
				continue
			}
			if cl, err := os.ReadFile("/proc/" + pid + "/cmdline"); err == nil &&
				strings.Contains(string(cl), "forward restart") {
				continue
			}
			live = append(live, pid)
		}
		if len(live) == 0 {
			break
		}
		_ = exec.Command("pkill", "-9", "-f", strings.Join(victims, "|")).Run()
		time.Sleep(500 * time.Millisecond)
		if time.Now().After(deadline) {
			fmt.Println("zcode: WARNING: some processes resisted the kill:", strings.Join(live, ","))
			break
		}
	}
	fmt.Println("zcode: all zcode processes gone")

	if _, err := exec.LookPath("systemctl"); err == nil {
		if out, err := exec.Command("systemctl", "is-active", "--quiet", "zqf").Output(); err == nil || len(out) > 0 {
			// unit exists — restart under systemd (keeps supervision)
			if err := exec.Command("systemctl", "restart", "zqf").Run(); err != nil {
				fatal("systemctl restart zqf: %v", err)
			}
			fmt.Println("zcode: systemctl restart zqf 完成")
			watchStartup()
			return
		}
	}
	// no systemd unit: start a bare daemon like install.sh does
	cmd := exec.Command("/root/.local/bin/zcode-quick-web-forward", "run", "--region", "china")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		fatal("daemon start: %v", err)
	}
	fmt.Printf("zcode: daemon started pid %d\n", cmd.Process.Pid)
	watchStartup()
}

// watchStartup prints the pairing URL once the new daemon registers.
func watchStartup() {
	fmt.Println("zcode: waiting for pairing URL ...")
	for i := 0; i < 40; i++ {
		b, err := os.ReadFile("/root/zcode-forward.log")
		if err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				if strings.Contains(line, "https://zcode.z.ai/remote/v4?") {
					fmt.Println(strings.TrimSpace(line))
					return
				}
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	fmt.Println("zcode: URL 未在 20s 内出现 — 查看 /root/zcode-forward.log")
}
