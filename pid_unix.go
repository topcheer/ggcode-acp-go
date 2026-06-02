//go:build unix

package acp

import (
	"fmt"
	"syscall"
	"time"
)

func isPIDRunning(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

func terminatePID(pid int) error {
	if pid <= 0 {
		return nil
	}
	if !isPIDRunning(pid) {
		return nil
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
		return fmt.Errorf("sending SIGTERM to %d: %w", pid, err)
	}
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if !isPIDRunning(pid) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		return fmt.Errorf("sending SIGKILL to %d: %w", pid, err)
	}
	return nil
}
