//go:build linux

package main

// findEnginePIDs locates the zcode app-server engine child process(es) for a
// workspace by scanning /proc environ for ZCODE_AGENT_SERVER_CWD — the exact
// variable officialhost.Env hands the engine at spawn. Killing the child is
// safe recovery-wise: the host's engine process manager owns the lifecycle
// and respawns a fresh engine (child.killed → getClient → start).

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
