//go:build !windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// killPID terminates a daemon recorded in a pid file (SIGTERM on Unix).
func killPID(pid int) error {
	return syscall.Kill(pid, syscall.SIGTERM)
}

// setSysProcAttr detaches the daemon child into its own session (Unix).
func setSysProcAttr(child *exec.Cmd) {
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// lockExclusive takes a non-blocking exclusive lock on the instance-lock
// file; false means another daemon already holds it.
func lockExclusive(f *os.File) bool {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil
}
