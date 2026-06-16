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
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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
}

// Run watches every configured folder and blocks until the context is cancelled
// or a stop signal (SIGINT/SIGTERM) arrives. dir is ~/backup_repo, used for the
// PID file that `backuprepo stop` targets.
//
// Note: the watched-folder set is read once at startup. Running `watch`/`unwatch`
// while the daemon is up does not take effect until it is restarted — the
// periodic scan will still cover folders already watched at start.
func (d *Daemon) Run(ctx context.Context, dir string, out io.Writer) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := writePID(dir); err != nil {
		return err
	}
	defer removePID(dir)

	folders, err := d.store.ListFolders(ctx)
	if err != nil {
		return err
	}
	if len(folders) == 0 {
		return fmt.Errorf("%w: no folders are being watched (use `backuprepo watch`)", apperr.ErrDaemon)
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
	// decides when to actually trigger a backup (see debounce's TODO).
	events := make(chan string, 1024)
	go d.debounce(ctx, events, func() { d.flush(ctx, out) })

	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			fmt.Fprintln(out, "Shutting down.")
			return nil

		case ev, ok := <-w.Events:
			if !ok {
				return nil
			}
			// inotify is not recursive: a newly created directory needs its own
			// watch, or changes inside it would go unnoticed until the next scan.
			if ev.Op&fsnotify.Create != 0 {
				if info, statErr := os.Stat(ev.Name); statErr == nil && info.IsDir() {
					_ = addRecursive(w, ev.Name)
				}
			}
			// React to content-affecting ops. Chmod is metadata-only; Remove is
			// ignored because the backup flow is upload-only (it does not delete
			// remote objects when a local file disappears). Rename matters because
			// many editors save atomically (write temp, rename over the target).
			if ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Rename) != 0 {
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

// debounce coalesces a stream of changed paths into as few flush() calls as
// possible. A single editor save emits several events and a bulk copy emits
// hundreds; without coalescing each one would trigger a full re-scan.
//
// Strategy (tunable via the constants below):
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

// addRecursive adds a watch on root and every subdirectory beneath it. The Linux
// fsnotify backend (inotify) watches a single directory, not a subtree, so each
// dir must be added explicitly — and newly created dirs re-added at runtime (the
// Create handler in Run does that). On Windows, ReadDirectoryChangesW is natively
// recursive, so a future platform-specific build could simplify this.
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
func Stop(dir string, out io.Writer) error {
	data, err := os.ReadFile(pidPath(dir))
	if err != nil {
		return fmt.Errorf("%w: not running (no pid file)", apperr.ErrDaemon)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return fmt.Errorf("%w: corrupt pid file", apperr.ErrDaemon)
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("%w: find process %d: %v", apperr.ErrDaemon, pid, err)
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("%w: signal pid %d: %v", apperr.ErrDaemon, pid, err)
	}
	fmt.Fprintf(out, "Sent stop signal to daemon (pid %d).\n", pid)
	return nil
}

func pidPath(dir string) string { return filepath.Join(dir, "daemon.pid") }

// writePID records the current PID, refusing to start if a live daemon is found.
func writePID(dir string) error {
	if data, err := os.ReadFile(pidPath(dir)); err == nil {
		if pid, perr := strconv.Atoi(strings.TrimSpace(string(data))); perr == nil && processAlive(pid) {
			return fmt.Errorf("%w: already running (pid %d); use `backuprepo stop`", apperr.ErrDaemon, pid)
		}
		// Stale PID file (process gone); fall through and overwrite it.
	}
	return os.WriteFile(pidPath(dir), []byte(strconv.Itoa(os.Getpid())), 0o600)
}

func removePID(dir string) { _ = os.Remove(pidPath(dir)) }

// processAlive reports whether pid refers to a running process. On Unix,
// os.FindProcess always succeeds, so liveness is probed with signal 0.
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}
