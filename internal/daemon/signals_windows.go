//go:build windows

package daemon

import (
	"context"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// shutdownSignals: Windows has no SIGTERM. A daemon in the foreground shuts down
// gracefully on Ctrl-C (os.Interrupt); a cross-process `bb stop` uses the named
// stop event (see signalStop / installStopWatcher).
func shutdownSignals() []os.Signal {
	return []os.Signal{os.Interrupt}
}

// eventName is the per-PID named event a running daemon waits on for a graceful
// stop request. Local namespace: stop and daemon run in the same user session.
func eventName(pid int) string {
	return fmt.Sprintf(`Local\backuprepo-daemon-stop-%d`, pid)
}

// signalStop asks the daemon to stop. It first tries the graceful path —
// signalling the daemon's named stop event, so Run returns through its normal
// deferred cleanup (PID file removed, watcher closed) — and falls back to a
// forceful TerminateProcess only if the event can't be opened (e.g. a daemon
// that predates this mechanism).
func signalStop(proc *os.Process) error {
	if name, err := windows.UTF16PtrFromString(eventName(proc.Pid)); err == nil {
		if h, err := windows.OpenEvent(windows.EVENT_MODIFY_STATE, false, name); err == nil {
			defer windows.CloseHandle(h)
			return windows.SetEvent(h)
		}
	}
	return proc.Kill()
}

// installStopWatcher creates this process's named stop event and returns a
// channel that closes when another process (bb stop) signals it. cleanup wakes
// the waiting goroutine and releases the handle.
func installStopWatcher(_ context.Context) (<-chan struct{}, func(), error) {
	name, err := windows.UTF16PtrFromString(eventName(os.Getpid()))
	if err != nil {
		return nil, func() {}, err
	}
	// Manual-reset, initially non-signaled. CreateEvent returns a usable handle
	// even when the named object already exists (err == ERROR_ALREADY_EXISTS), so
	// validity is judged by the handle, not the error.
	h, _ := windows.CreateEvent(nil, 1, 0, name)
	if h == 0 {
		return nil, func() {}, fmt.Errorf("create stop event")
	}
	ch := make(chan struct{})
	go func() {
		_, _ = windows.WaitForSingleObject(h, windows.INFINITE)
		close(ch)
	}()
	cleanup := func() {
		_ = windows.SetEvent(h) // wake the waiter so the goroutine exits
		_ = windows.CloseHandle(h)
	}
	return ch, cleanup, nil
}

// processAlive reports whether pid refers to a running process. On Windows
// os.FindProcess opens the process handle and returns an error if it does not
// exist, so a successful lookup is a sufficient liveness signal.
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	_ = proc.Release()
	return true
}
