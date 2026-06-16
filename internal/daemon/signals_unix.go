//go:build !windows

package daemon

import (
	"os"
	"syscall"
)

// shutdownSignals are the signals that trigger a graceful daemon shutdown on
// Unix: Ctrl-C (SIGINT) and the conventional `kill` signal (SIGTERM).
func shutdownSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM}
}

// signalStop asks the daemon to shut down gracefully via SIGTERM, which Run
// catches through signal.NotifyContext so its deferred cleanup (PID file,
// in-flight upload) runs.
func signalStop(proc *os.Process) error {
	return proc.Signal(syscall.SIGTERM)
}

// processAlive reports whether pid refers to a running process. On Unix
// os.FindProcess always succeeds, so liveness is probed with signal 0.
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}
