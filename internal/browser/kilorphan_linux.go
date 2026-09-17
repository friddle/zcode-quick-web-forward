//go:build linux

package browser

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

// killOrphanedChromium reaps chromium processes left by earlier daemon runs
// (temp profiles named zqf-chromium-*). Only our own orphaned instances are
// matched, never user-facing browsers. /proc is Linux-only, so this is a
// no-op on every other platform.
func killOrphanedChromium() {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	for _, e := range entries {
		if !isPID(e.Name()) {
			continue
		}
		cmdline, err := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		if err != nil {
			continue
		}
		if strings.Contains(string(cmdline), "zqf-chromium-") &&
			strings.Contains(string(cmdline), "remote-debugging-port") {
			if p, err := strconv.Atoi(e.Name()); err == nil {
				_ = syscall.Kill(p, syscall.SIGKILL)
			}
		}
	}
}
