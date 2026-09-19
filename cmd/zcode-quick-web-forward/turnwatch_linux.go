//go:build linux

package main

// findEnginePIDs locates the zcode app-server engine child process(es) for a
// workspace by scanning /proc environ for ZCODE_AGENT_SERVER_CWD — the exact
// variable officialhost.Env hands the engine at spawn. Killing the child is
// safe recovery-wise: the host's engine process manager owns the lifecycle
// and respawns a fresh engine (child.killed → getClient → start).
//
// The host shim must NEVER match: officialhost.Env seeds the same variable in
// the HOST's environment (the host passes it down to the engine it spawns),
// so a plain environ match once killed the host alongside the wedged engine —
// and with the host gone nothing respawned it (every forwarded call dropped,
// every page connect hung on the pairing splash).

import (
	"os"
	"strconv"
	"strings"
)

func findEnginePIDs(workspace string) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	want := "ZCODE_AGENT_SERVER_CWD=" + workspace
	out := []int{}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		cb, err := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		if err == nil && engineKillExcluded(string(cb)) {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/environ")
		if err != nil {
			continue
		}
		for _, kv := range strings.Split(string(b), "\x00") {
			if kv == want {
				out = append(out, pid)
				break
			}
		}
	}
	return out
}

// engineKillExcluded reports a cmdline belonging to a process the watchdog
// must NEVER kill even though it carries the engine's ZCODE_AGENT_SERVER_CWD
// in its environ. The official host shim is the critical one: officialhost.Env
// seeds the variable in the HOST's environment, and on Linux the shim's
// process.title assignment ("zcode-host-<label>") REPLACES /proc cmdline — a
// "shim.mjs" substring match alone therefore missed it and the 12:01 kill
// took the whole service pipe down (every forwarded call dropped, the page
// wedged on the pairing splash until the ensure-alive respawn landed).
func engineKillExcluded(cmdline string) bool {
	return strings.Contains(cmdline, "shim.mjs") || strings.Contains(cmdline, "zcode-host")
}
