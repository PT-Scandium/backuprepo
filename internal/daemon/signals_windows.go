//go:build windows

package daemon

import "os"

// shutdownSignals: Windows has no SIGTERM. A daemon running in the foreground
// still shuts down gracefully on Ctrl-C (delivered as os.Interrupt); a
// cross-process `bb stop` is forceful — see signalStop.
func shutdownSignals() []os.Signal {
	return []os.Signal{os.Interrupt}
}

// signalStop terminates the daemon process. Windows has no SIGTERM and no
// reliable cross-process graceful signal without extra OS APIs, so this is a
// forceful TerminateProcess (proc.Kill). The daemon tolerates it by design: the
// stale PID file is overwritten on the next start (writePID re-checks liveness),
// and an interrupted upload is simply retried (uploads are idempotent). For a
// graceful stop, run the daemon in the foreground and press Ctrl-C.
func signalStop(proc *os.Process) error {
	return proc.Kill()
}

// processAlive reports whether pid refers to a running process. On Windows
// os.FindProcess opens the process handle and returns an error if the process
// does not exist, so a successful lookup is a sufficient liveness signal for the
// PID-file guard.
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	_ = proc.Release()
	return true
}
