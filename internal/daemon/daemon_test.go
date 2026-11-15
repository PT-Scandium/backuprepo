package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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

// TestRunningPID covers the liveness probe `bb service install` uses to detect a
// daemon started outside systemd: missing, corrupt and stale PID files are all
// "not running"; a PID file naming a live process is "running".
func TestRunningPID(t *testing.T) {
	dir := t.TempDir()
	if _, ok := RunningPID(dir); ok {
		t.Fatal("no pid file: want not running")
	}
	if err := os.WriteFile(pidPath(dir), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := RunningPID(dir); ok {
		t.Fatal("corrupt pid file: want not running")
	}
	if err := os.WriteFile(pidPath(dir), []byte("0"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := RunningPID(dir); ok {
		t.Fatal("pid 0: want not running")
	}
	if err := writePID(dir); err != nil {
		t.Fatal(err)
	}
	defer removePID(dir)
	if pid, ok := RunningPID(dir); !ok || pid != os.Getpid() {
		t.Fatalf("live pid file: got (%d, %v), want (%d, true)", pid, ok, os.Getpid())
	}
}

func TestParseStatStartTime(t *testing.T) {
	// 52 fields as in /proc/<pid>/stat; field 22 (start time) = "424242".
	rest := make([]string, 50) // fields 3..52
	for i := range rest {
		rest[i] = strconv.Itoa(i + 3)
	}
	rest[22-3] = "424242"
	tail := strings.Join(rest, " ")
	for _, comm := range []string{"(bb)", "(my prog)", "(a) (b)) x)"} {
		got, ok := parseStatStartTime("1234 " + comm + " " + tail)
		if !ok || got != "424242" {
			t.Errorf("comm %s: got %q, %v", comm, got, ok)
		}
	}
	for _, bad := range []string{"", "1234 bb S 1", "1234 (bb) S 1 2 3"} {
		if _, ok := parseStatStartTime(bad); ok {
			t.Errorf("parseStatStartTime(%q) should fail", bad)
		}
	}
}

// ownStart returns this process's start time, skipping where it's unavailable.
func ownStart(t *testing.T) string {
	t.Helper()
	s, ok := procStartTime(os.Getpid())
	if !ok {
		t.Skip("process start time unavailable on this OS")
	}
	return s
}

func TestWritePIDRecordsStartTime(t *testing.T) {
	start := ownStart(t)
	dir := t.TempDir()
	if err := writePID(dir); err != nil {
		t.Fatal(err)
	}
	defer removePID(dir)
	rec, err := readPIDFile(dir)
	if err != nil || rec.pid != os.Getpid() || rec.start != start {
		t.Fatalf("record = %+v, %v; want pid %d start %s", rec, err, os.Getpid(), start)
	}
	// daemon.pid must stay the bare PID that every older bb parses
	// (strconv.Atoi(strings.TrimSpace(...))), so mixed versions interoperate.
	data, _ := os.ReadFile(pidPath(dir))
	if old, err := strconv.Atoi(strings.TrimSpace(string(data))); err != nil || old != os.Getpid() {
		t.Errorf("daemon.pid = %q is not readable by older bb versions", data)
	}
	removePID(dir)
	if _, err := os.Stat(startPath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Error("removePID must also remove daemon.start")
	}
}

// writeReusedPID writes a PID file naming this (live) process but with a start
// time it doesn't have — exactly what a stale file looks like after the PID is
// reused by another process.
func writeReusedPID(t *testing.T, dir string) {
	t.Helper()
	start := ownStart(t)
	pid := strconv.Itoa(os.Getpid())
	if err := os.WriteFile(pidPath(dir), []byte(pid), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(startPath(dir), []byte(fmt.Sprintf("%s %s1\n", pid, start)), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestStartFileForOtherPIDIgnored: an older bb rewrote daemon.pid but left a
// daemon.start for a different PID; it must not be paired with the new PID
// (which would wrongly look stale), so liveness falls back to the PID alone.
func TestStartFileForOtherPIDIgnored(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(pidPath(dir), []byte(strconv.Itoa(os.Getpid())), 0o600)
	_ = os.WriteFile(startPath(dir), []byte("999999 12345\n"), 0o600)
	rec, err := readPIDFile(dir)
	if err != nil || rec.start != "" {
		t.Fatalf("record = %+v, %v; start from another PID must be ignored", rec, err)
	}
	if _, ok := RunningPID(dir); !ok {
		t.Error("live PID with an unrelated start file should still count as running")
	}
}

// TestStopSignalsMatchingProcess covers Stop's success path: with a PID and
// matching start time it sends SIGTERM to that process (a child `sleep` here).
func TestStopSignalsMatchingProcess(t *testing.T) {
	ownStart(t) // skip where start times are unavailable
	sleepBin, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep not available")
	}
	cmd := exec.Command(sleepBin, "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	pid := cmd.Process.Pid
	var start string
	for i := 0; i < 100; i++ { // the child's /proc entry appears asynchronously
		if s, ok := procStartTime(pid); ok {
			start = s
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if start == "" {
		t.Fatal("could not read child start time")
	}
	dir := t.TempDir()
	_ = os.WriteFile(pidPath(dir), []byte(strconv.Itoa(pid)), 0o600)
	_ = os.WriteFile(startPath(dir), []byte(fmt.Sprintf("%d %s\n", pid, start)), 0o600)

	var out bytes.Buffer
	if err := Stop(dir, &out); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !strings.Contains(out.String(), fmt.Sprintf("Stopping daemon (pid %d)", pid)) {
		t.Errorf("output = %q", out.String())
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("child exited cleanly; expected termination by SIGTERM")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("child still running 5s after Stop")
	}
}

// TestReusedPIDIsNotTheDaemon is the regression for PID reuse: a live process
// with a different start time must not count as the running daemon.
func TestReusedPIDIsNotTheDaemon(t *testing.T) {
	dir := t.TempDir()
	writeReusedPID(t, dir)
	if pid, ok := RunningPID(dir); ok {
		t.Fatalf("RunningPID = %d, true; a reused PID is not the daemon", pid)
	}
	if err := writePID(dir); err != nil {
		t.Fatalf("writePID must overwrite a reused-PID file: %v", err)
	}
	removePID(dir)
}

// TestStopNeverSignalsReusedPID: Stop must not signal a process that merely
// reuses the recorded PID. The PID here is this test process, so a regression
// would SIGTERM the test binary itself and fail loudly.
func TestStopNeverSignalsReusedPID(t *testing.T) {
	dir := t.TempDir()
	writeReusedPID(t, dir)
	var out bytes.Buffer
	err := Stop(dir, &out)
	if !errors.Is(err, apperr.ErrDaemon) || !strings.Contains(err.Error(), "stale pid file") {
		t.Fatalf("want ErrDaemon about a stale pid file, got %v", err)
	}
	if _, statErr := os.Stat(pidPath(dir)); !errors.Is(statErr, os.ErrNotExist) {
		t.Error("stale pid file should be removed")
	}
	if _, statErr := os.Stat(startPath(dir)); !errors.Is(statErr, os.ErrNotExist) {
		t.Error("stale start file should be removed")
	}
	if strings.Contains(out.String(), "Stopping") {
		t.Errorf("must not report stopping: %q", out.String())
	}
}

func TestLegacyPIDFileStillRecognised(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(pidPath(dir), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	if pid, ok := RunningPID(dir); !ok || pid != os.Getpid() {
		t.Fatalf("legacy pid-only file: got (%d, %v)", pid, ok)
	}
}

func TestStopNoOrCorruptPIDFile(t *testing.T) {
	dir := t.TempDir()
	if err := Stop(dir, &bytes.Buffer{}); !errors.Is(err, apperr.ErrDaemon) || !strings.Contains(err.Error(), "no pid file") {
		t.Errorf("missing file: %v", err)
	}
	_ = os.WriteFile(pidPath(dir), []byte("garbage"), 0o600)
	if err := Stop(dir, &bytes.Buffer{}); !errors.Is(err, apperr.ErrDaemon) || !strings.Contains(err.Error(), "corrupt") {
		t.Errorf("corrupt file: %v", err)
	}
}
