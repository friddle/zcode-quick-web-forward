//go:build !linux

package main

// findEnginePIDs for non-Linux hosts: match the engine by its argv signature
// (`app-server --stdio` is unique to the zcode agent server). No per-workspace
// refinement outside /proc — callers must treat the result as best effort.

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
)

func findEnginePIDs(workspace string) []int {
	out, err := exec.Command("pgrep", "-f", "app-server --stdio").Output()
	if err != nil {
		return nil
	}
	pids := []int{}
	for _, f := range strings.Fields(string(out)) {
		if pid, err := strconv.Atoi(f); err == nil && pid != os.Getpid() {
			pids = append(pids, pid)
		}
	}
	return pids
}
