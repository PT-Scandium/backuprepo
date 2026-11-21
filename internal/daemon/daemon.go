// Package daemon runs backuprepo's background file watcher.
//
// It combines two change-detection paths that both funnel into the SAME engine,
// backup.Service.UploadChanged, so "what counts as changed" has a single
// definition (see internal/backup):
//
//   - Real-time events via fsnotify — low latency, but best-effort: the kernel
//     event queue can overflow, new subdirectories race their watch being added,
//     and events that occur while the daemon is down are simply missed.
//   - A periodic full scan (FallbackInterval) — the safety net that guarantees
//     eventual consistency for anything the event path dropped.
//
// The watcher is the fast path; the ticker is the floor. Neither alone suffices.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"

	"backuprepo/internal/apperr"
	"backuprepo/internal/b2"
	"backuprepo/internal/backup"
	"backuprepo/internal/store"
)

// FallbackInterval is how often the daemon runs a full scan regardless of
// events, catching anything fsnotify missed (queue overflow, races, downtime).
const FallbackInterval = 5 * time.Minute

// Daemon watches configured folders and backs up changed files.
type Daemon struct {
	store       *store.Store
	svc         *backup.Service
	interval    time.Duration // full-scan fallback period
	quietWindow time.Duration // flush after events stay quiet this long
	maxDelay    time.Duration // upper bound on coalescing from the first event of a burst
	deletions   bool          // deletion propagation on: Remove events trigger a flush too
}

// New builds a Daemon from a store and an uploader, with the default debounce
// timing (1s quiet window, 5s max delay) and 5-minute fallback scan.
func New(st *store.Store, up b2.Uploader) *Daemon {
	return &Daemon{
		store:       st,
		svc:         backup.New(st, up),
		interval:    FallbackInterval,
		quietWindow: 1 * time.Second,
		maxDelay:    5 * time.Second,
	}
}

// EnableDeletions turns on deletion propagation: the daemon will remove remote
// objects whose local files have been deleted (opt-in; destructive).
func (d *Daemon) EnableDeletions(del b2.Deleter) {
	d.svc = d.svc.WithDeleter(del)
	d.deletions = true
}

// Run watches every configured folder and blocks until the context is cancelled
// or a stop signal (SIGINT/SIGTERM) arrives. dir is ~/backup_repo, used for the
// PID file that `bb stop` targets.
//
// Note: the watched-folder set is read once at startup. Running `watch`/`unwatch`
// while the daemon is up does not take effect until it is restarted — the
// periodic scan will still cover folders already watched at start.
func (d *Daemon) Run(ctx context.Context, dir string, out io.Writer) error {
	ctx, stop := signal.NotifyContext(ctx, shutdownSignals()...)
	defer stop()

	if err := writePID(dir); err != nil {
		return err
	}
	defer removePID(dir)

	// An external `stop` request closes extStop, letting the daemon shut down
	// through its normal deferred cleanup. On Unix this is nil (the SIGTERM path
	// above handles stop); on Windows it's backed by a named event.
	extStop, cleanupStop, err := installStopWatcher(ctx)
	if err != nil {
		fmt.Fprintf(out, "warning: stop watcher unavailable: %v\n", err)
	}
	defer cleanupStop()

	folders, err := d.store.ListFolders(ctx)
	if err != nil {
		return err
	}
	if len(folders) == 0 {
		return fmt.Errorf("%w: no folders are being watched (use `bb watch`)", apperr.ErrDaemon)
	}

	w, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("%w: new watcher: %v", apperr.ErrDaemon, err)
	}
	defer w.Close()

	for _, f := range folders {
		if err := addRecursive(w, f); err != nil {
			fmt.Fprintf(out, "warning: watch %s: %v\n", f, err)
		}
	}
	fmt.Fprintf(out, "Watching %d folder(s); full scan every %s. Press Ctrl-C to stop.\n",
		len(folders), d.interval)

	// Initial scan so the first backup doesn't wait up to a full interval.
	d.flush(ctx, out)

	// Changed paths flow from the event loop into the debouncer goroutine, which
	// decides when to actually trigger a backup (see debounce).
	events := make(chan string, 1024)
	go d.debounce(ctx, events, func() { d.flush(ctx, out) })

	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			fmt.Fprintln(out, "Shutting down.")
			return nil

		case <-extStop:
			fmt.Fprintln(out, "Stop requested; shutting down.")
			return nil

		case ev, ok := <-w.Events:
			if !ok {
				return nil
			}
			// fsnotify watches are not recursive: a newly created directory
			// needs its own watch, or changes inside it would go unnoticed
			// until the next scan.
			if ev.Op&fsnotify.Create != 0 {
				if info, statErr := os.Stat(ev.Name); statErr == nil && info.IsDir() {
					_ = addRecursive(w, ev.Name)
				}
			}
			if d.flushOn(ev.Op) {
				select {
				case events <- ev.Name:
				default:
					// Debounce buffer full during a burst; drop the path. The
					// fallback scan is the safety net that will still catch it.
				}
			}

		case err, ok := <-w.Errors:
			if !ok {
				return nil
			}
			fmt.Fprintf(out, "watch error: %v\n", err)

		case <-ticker.C:
			d.flush(ctx, out)
		}
	}
}

// flushOn reports whether this daemon should schedule a flush for op (see
// triggersFlush), with Remove counting once EnableDeletions has run.
func (d *Daemon) flushOn(op fsnotify.Op) bool { return triggersFlush(op, d.deletions) }

// triggersFlush reports whether an fsnotify op should schedule a flush. Create,
// Write and Rename change content (Rename matters because many editors save
// atomically: write a temp file, rename it over the target). Chmod is
// metadata-only. Remove counts only with deletion propagation on (--delete):
// the flush's reconciliation scan then removes the remote copy promptly
// instead of at the next fallback scan. The scan, not the event, decides
// what is deleted (ADR-013).
func triggersFlush(op fsnotify.Op, deletions bool) bool {
	if op&(fsnotify.Create|fsnotify.Write|fsnotify.Rename) != 0 {
		return true
	}
	return deletions && op&fsnotify.Remove != 0
}

// debounce coalesces a stream of changed paths into as few flush() calls as
// possible. A single editor save emits several events and a bulk copy emits
// hundreds; without coalescing each one would trigger a full re-scan.
//
// Strategy (tunable via the Daemon's quietWindow and maxDelay fields, set in New):
//   - quietWindow (1s): flush once events have stayed quiet this long. Reset on
//     every event, so a flush lands shortly after writes settle — long enough to
//     avoid uploading a file mid-save, short enough to feel prompt.
//   - maxDelay (5s): an upper bound measured from the first event of a burst.
//     Under a steady trickle the quiet window would keep resetting and never fire
//     (starvation); this cap forces a flush anyway.
//   - Granularity: a flush re-scans ALL watched folders rather than the specific
//     changed paths. That is cheap because change detection skips unchanged files
//     on a size+mtime check before hashing (see internal/backup), and it keeps
//     this function pure timing logic — event paths are consumed only as a signal.
//
// flush() runs synchronously, so no two backups overlap; events arriving during a
// flush queue in `events` and start the next burst.
func (d *Daemon) debounce(ctx context.Context, events <-chan string, flush func()) {
	// Timers start stopped and are Reset when a burst begins. Go 1.23+ timer
	// semantics guarantee a stopped timer delivers no stale tick, so the channels
	// never need manual draining (and the long initial durations mean neither can
	// fire before Stop even on older toolchains).
	quiet := time.NewTimer(d.quietWindow)
	quiet.Stop()
	maxWait := time.NewTimer(d.maxDelay)
	maxWait.Stop()
	defer quiet.Stop()
	defer maxWait.Stop()

	pending := false
	fire := func() {
		quiet.Stop()
		maxWait.Stop()
		pending = false
		flush() // synchronous; cancels timers first so neither fires during upload
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-events:
			quiet.Reset(d.quietWindow) // restart the quiet window on every event
			if !pending {
				pending = true
				maxWait.Reset(d.maxDelay) // cap measured from the first event of the burst
			}
		case <-quiet.C:
			if pending {
				fire()
			}
		case <-maxWait.C:
			if pending {
				fire()
			}
		}
	}
}

// flush runs one full change-detection pass over all watched folders. It is safe
// to call frequently: unchanged files are skipped cheaply (size+mtime) before any
// hashing or upload. Errors are reported, not fatal — the daemon keeps running.
func (d *Daemon) flush(ctx context.Context, out io.Writer) {
	res, err := d.svc.UploadChanged(ctx)
	if err != nil {
		fmt.Fprintf(out, "scan: uploaded %d, skipped %d, failed %d: %v\n",
			res.Uploaded, res.Skipped, res.Failed, err)
		return
	}
	if res.Uploaded > 0 || res.Deleted > 0 {
		fmt.Fprintf(out, "scan: uploaded %d, skipped %d, deleted %d\n",
			res.Uploaded, res.Skipped, res.Deleted)
	}
}

// addRecursive adds a watch on root and every subdirectory beneath it. fsnotify
// watches a single directory per Add on every platform: inotify cannot watch a
// subtree, and although ReadDirectoryChangesW can, fsnotify v1.10 keeps its
// recursive mode disabled. So each dir must be added explicitly, and newly
// created dirs added at runtime (the Create handler in Run does that).
func addRecursive(w *fsnotify.Watcher, root string) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries, keep walking
		}
		if d.IsDir() {
			if addErr := w.Add(path); addErr != nil {
				return fmt.Errorf("add %s: %w", path, addErr)
			}
		}
		return nil
	})
}

// Stop signals a running daemon (identified by its PID file in dir) to shut down.
// It never signals a process that isn't the daemon: if the recorded PID is dead
// or now belongs to another process (PID reuse after an unclean shutdown), the
// stale PID file is removed and Stop reports that no daemon is running.
func Stop(dir string, out io.Writer) error {
	rec, err := readPIDFile(dir)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: not running (no pid file)", apperr.ErrDaemon)
	}
	if errors.Is(err, apperr.ErrDaemon) {
		return err
	}
	if err != nil {
		return fmt.Errorf("%w: read pid file: %v", apperr.ErrDaemon, err)
	}
	if !rec.alive() {
		removePID(dir)
		return fmt.Errorf("%w: not running (removed stale pid file for pid %d)", apperr.ErrDaemon, rec.pid)
	}
	pid := rec.pid
	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("%w: find process %d: %v", apperr.ErrDaemon, pid, err)
	}
	if err := signalStop(proc); err != nil {
		return fmt.Errorf("%w: stop pid %d: %v", apperr.ErrDaemon, pid, err)
	}
	fmt.Fprintf(out, "Stopping daemon (pid %d).\n", pid)
	return nil
}

// RunningPID reports the PID of a live daemon recorded in dir's PID file.
// A missing, corrupt, or stale (dead or reused PID) PID file reports false.
func RunningPID(dir string) (int, bool) {
	rec, err := readPIDFile(dir)
	if err != nil || !rec.alive() {
		return 0, false
	}
	return rec.pid, true
}

// pidRecord identifies the running daemon: its PID (daemon.pid) and, where the
// OS exposes it (Linux and Windows), its start time (daemon.start).
type pidRecord struct {
	pid   int
	start string // "" if unknown
}

// readPIDFile reads the daemon's PID from daemon.pid (just the number — the
// format every bb version reads, so older binaries still interoperate) and,
// from daemon.start ("<pid> <starttime>"), its start time — used only when the
// PIDs agree, so a daemon.pid rewritten by an older bb is never paired with a
// stale start time. A missing daemon.pid returns an error matching
// os.ErrNotExist; an unparsable one returns apperr.ErrDaemon.
func readPIDFile(dir string) (pidRecord, error) {
	data, err := os.ReadFile(pidPath(dir))
	if err != nil {
		return pidRecord{}, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	// PIDs are positive and fit in 32 bits on every supported OS; a larger value
	// would wrap to an unrelated PID in Windows' uint32 process APIs.
	if err != nil || pid <= 0 || pid > math.MaxUint32 {
		return pidRecord{}, fmt.Errorf("%w: corrupt pid file", apperr.ErrDaemon)
	}
	rec := pidRecord{pid: pid}
	if sdata, err := os.ReadFile(startPath(dir)); err == nil {
		if f := strings.Fields(string(sdata)); len(f) == 2 && f[0] == strconv.Itoa(pid) {
			rec.start = f[1]
		}
	}
	return rec, nil
}

// alive reports whether r still names the daemon that wrote it: the PID must
// be running and, when a start time was recorded and can be read, the start
// times must match — so a PID reused by another process is not mistaken for
// the daemon (and never signalled by Stop). Without a recorded or readable
// start time it falls back to processAlive alone.
func (r pidRecord) alive() bool {
	if r.pid <= 0 || !processAlive(r.pid) {
		return false
	}
	if r.start == "" {
		return true
	}
	cur, ok := procStartTime(r.pid)
	if !ok {
		return true
	}
	return cur == r.start
}

// parseStatStartTime extracts the start time (field 22) from the contents of
// /proc/<pid>/stat. Field 2 is the command name in parentheses and may itself
// contain spaces and ")", so fields are counted from the LAST ")".
func parseStatStartTime(stat string) (string, bool) {
	i := strings.LastIndexByte(stat, ')')
	if i < 0 {
		return "", false
	}
	fields := strings.Fields(stat[i+1:]) // fields[0] is field 3 (state)
	const startIdx = 22 - 3
	if len(fields) <= startIdx {
		return "", false
	}
	return fields[startIdx], true
}

// pidPath returns the daemon PID file path within dir.
func pidPath(dir string) string { return filepath.Join(dir, "daemon.pid") }

// startPath returns the file recording the daemon's "<pid> <starttime>".
func startPath(dir string) string { return filepath.Join(dir, "daemon.start") }

// writePID records the current PID (and, on Linux and Windows, its start time
// in a separate file — see readPIDFile), refusing to start if a live daemon is
// found. A stale file — dead process, or a PID
// since reused by another process — is overwritten.
func writePID(dir string) error {
	if rec, err := readPIDFile(dir); err == nil && rec.alive() {
		return fmt.Errorf("%w: already running (pid %d); use `bb stop`", apperr.ErrDaemon, rec.pid)
	}
	pid := strconv.Itoa(os.Getpid())
	// Start file first, so a reader that sees the new daemon.pid also sees its
	// start time; it is ignored anyway unless its PID matches daemon.pid.
	if start, ok := procStartTime(os.Getpid()); ok {
		if err := os.WriteFile(startPath(dir), []byte(pid+" "+start+"\n"), 0o600); err != nil {
			return err
		}
	} else {
		_ = os.Remove(startPath(dir))
	}
	return os.WriteFile(pidPath(dir), []byte(pid), 0o600)
}

// removePID deletes the daemon PID and start files, ignoring any error.
func removePID(dir string) {
	_ = os.Remove(pidPath(dir))
	_ = os.Remove(startPath(dir))
}
