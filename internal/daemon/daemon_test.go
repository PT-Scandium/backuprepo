package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"backuprepo/internal/apperr"
)

// TestDebounceCoalescesBurst verifies a rapid burst of events triggers exactly
// one flush, once the quiet window elapses.
func TestDebounceCoalescesBurst(t *testing.T) {
	d := &Daemon{quietWindow: 20 * time.Millisecond, maxDelay: 500 * time.Millisecond}
	var flushes atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events := make(chan string)
	go d.debounce(ctx, events, func() { flushes.Add(1) })

	// Five events, each well within the quiet window, so the window keeps
	// resetting and only one flush should fire after they settle.
	for i := 0; i < 5; i++ {
		events <- "f"
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(80 * time.Millisecond) // > quietWindow, let the flush land

	if got := flushes.Load(); got != 1 {
		t.Fatalf("expected exactly 1 coalesced flush, got %d", got)
	}
}

// TestDebounceMaxDelayForcesFlush verifies that under a steady trickle of events
// (faster than the quiet window), the max-delay cap still forces a flush — the
// quiet window alone would keep resetting forever (starvation).
func TestDebounceMaxDelayForcesFlush(t *testing.T) {
	d := &Daemon{quietWindow: 50 * time.Millisecond, maxDelay: 80 * time.Millisecond}
	var flushes atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events := make(chan string)
	go d.debounce(ctx, events, func() { flushes.Add(1) })

	// Trickle for ~200ms at 20ms intervals (< quietWindow), so the quiet timer
	// never fires on its own and the cap must.
	stop := time.After(200 * time.Millisecond)
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
loop:
	for {
		select {
		case <-stop:
			break loop
		case <-tick.C:
			select {
			case events <- "f":
			default:
			}
		}
	}
	if got := flushes.Load(); got < 1 {
		t.Fatalf("expected the max-delay cap to force >=1 flush during the trickle, got %d", got)
	}
}

// TestDebounceStopsOnCancel verifies the goroutine exits when the context is
// cancelled and performs no further flushes.
func TestDebounceStopsOnCancel(t *testing.T) {
	d := &Daemon{quietWindow: 10 * time.Millisecond, maxDelay: 50 * time.Millisecond}
	var flushes atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())

	events := make(chan string, 1)
	done := make(chan struct{})
	go func() { d.debounce(ctx, events, func() { flushes.Add(1) }); close(done) }()

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("debounce did not return after context cancel")
	}
}

// TestAddRecursiveWatchesSubdirs verifies addRecursive registers a watch for the
// root and each nested subdirectory (inotify is not recursive).
func TestAddRecursiveWatchesSubdirs(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	w, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	if err := addRecursive(w, root); err != nil {
		t.Fatalf("addRecursive: %v", err)
	}
	// root, root/a, root/a/b → 3 directories watched.
	if got := len(w.WatchList()); got != 3 {
		t.Fatalf("expected 3 watched dirs, got %d: %v", got, w.WatchList())
	}
}

// TestWritePIDRefusesWhenRunning verifies a second start is refused while a live
// PID file (this test process) is present, and accepted once it is removed.
func TestWritePIDRefusesWhenRunning(t *testing.T) {
	dir := t.TempDir()
	if err := writePID(dir); err != nil {
		t.Fatalf("first writePID: %v", err)
	}
	// The PID file now names this live process, so a second start must refuse.
	if err := writePID(dir); !errors.Is(err, apperr.ErrDaemon) {
		t.Fatalf("expected ErrDaemon on second writePID, got %v", err)
	}
	removePID(dir)
	if err := writePID(dir); err != nil {
		t.Fatalf("writePID after removePID: %v", err)
	}
	removePID(dir)
}
