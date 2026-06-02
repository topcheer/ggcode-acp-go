//go:build !unix

package acp

func isPIDRunning(pid int) bool {
	return pid > 0
}

func terminatePID(pid int) error {
	return nil
}
