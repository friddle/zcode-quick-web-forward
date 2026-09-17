//go:build windows

package main

import (
	"os"
	"os/exec"
)

// killPID terminates a daemon recorded in a pid file (hard kill on Windows;
// SIGTERM does not exist there).
func killPID(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}

// setSysProcAttr: the Unix detached-session concept has no Windows
// equivalent on this spawn path; the child detaches via its own daemonize
// marker env.
func setSysProcAttr(child *exec.Cmd) {}

// lockExclusive: advisory file locking has no drop-in syscall here, and the
// pid-file content check above already guards against double daemons, so
// this stays permissive on Windows.
func lockExclusive(f *os.File) bool {
	_ = f
	return true
}

var _ = os.Getpid
