// CLI handlers for mapped folders and two-way sync (ADR-028): `bb mapto`,
// `bb unmap`, `bb maps` and `bb sync`, plus SyncMapped, the sync step that
// `bb upload`, the web UI's Upload button and the daemon run after the one-way
// backup.
package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"backuprepo/internal/apperr"
	"backuprepo/internal/b2"
	"backuprepo/internal/daemon"
	"backuprepo/internal/store"
	"backuprepo/internal/syncer"
)

// SyncMode selects what `bb sync` syncs.
type SyncMode int

const (
	// ModeMapped syncs every mapped folder (`bb sync`).
	ModeMapped SyncMode = iota
	// ModeWholeBucket maps Dir to the whole bucket and syncs it (`bb sync --all`).
	ModeWholeBucket
	// ModeFolder maps Dir to bucket folder Folder and syncs it (`bb sync --folder`).
	ModeFolder
)

// SyncOptions are the inputs of Sync.
type SyncOptions struct {
	Mode          SyncMode
	Folder        string // bucket folder for ModeFolder
	Dir           string // the local side for ModeWholeBucket/ModeFolder (usually the current directory)
	DryRun        bool
	Refill        bool   // allow refilling a folder whose synced files are all missing (`--refill`)
	Backend       string // effective backend kind; sync needs "b2"
	StateDir      string // ~/backup_repo: holds the sync lock and conflicts/ (losing local versions)
	DaemonRunning bool
}

// syncOpts builds the syncer options for state folder dir.
func syncOpts(dir string, dryRun, refill bool) syncer.Options {
	return syncer.Options{DryRun: dryRun, AllowRefill: refill, ConflictDir: filepath.Join(dir, "conflicts")}
}

// errNeedsB2 explains why sync refuses the S3 backend: only the native B2 API
// lists each version's id and SHA-1, which sync's change detection relies on.
var errNeedsB2 = fmt.Errorf("%w: two-way sync needs the native B2 backend (run `bb backend b2`)", apperr.ErrSync)

// errDaemonMaps explains why mapto refuses while the daemon runs.
var errDaemonMaps = fmt.Errorf("%w: the daemon is running and would sync a new mapping on its next pass, before you could preview it; "+
	"stop it first (`systemctl --user stop backuprepo` or `bb stop`), map the folder, preview with `bb sync --dry-run`, "+
	"run `bb sync`, then start the daemon again", apperr.ErrSync)

// MapTo maps the directory dir (normally the current one) to the bucket folder
// remote, so its files are kept in two-way sync with remote/ (ADR-028). An
// empty bucket folder is made visible with a zero-byte remote/.bzEmpty, as the
// Backblaze web UI does. Mapping rules are WatchRemote's plus planMapping's
// (no second mapping next to a whole-bucket mirror, no remapping a mapped
// folder, never the folder holding bb's state).
//
// It refuses while a daemon runs (its PID file is in stateDir): the daemon
// re-reads the mapped folders on every pass, so it would upload the new
// folder's files, permanently under Object Lock, before the user could preview
// the first sync with `bb sync --dry-run`.
func MapTo(ctx context.Context, st *store.Store, be b2.Backend, backendKind, stateDir, dir, remote string, out io.Writer) error {
	if backendKind != "b2" {
		return errNeedsB2
	}
	if stateDir != "" {
		if _, running := daemon.RunningPID(stateDir); running {
			return errDaemonMaps
		}
	}
	prefix, err := normalizeRemotePrefix(remote)
	if err != nil {
		return err
	}
	if _, _, err := planMapping(ctx, st, SyncOptions{Mode: ModeFolder, Folder: remote, Dir: dir, StateDir: stateDir}); err != nil {
		return err
	}
	// List before storing the mapping, so a bucket error leaves nothing behind.
	l, err := be.List(ctx, prefix, false)
	if err != nil {
		return fmt.Errorf("%w: check bucket folder %s: %v", apperr.ErrSync, prefix, err)
	}
	if err := WatchRemote(ctx, st, dir, remote, out); err != nil {
		return err
	}
	if len(l.Objects) == 0 && len(l.Prefixes) == 0 {
		if err := be.Upload(ctx, prefix+syncer.FolderMarker, bytes.NewReader(nil), 0); err != nil {
			return fmt.Errorf("%w: create bucket folder %s: %v", apperr.ErrSync, prefix, err)
		}
		fmt.Fprintf(out, "Created bucket folder %s\n", prefix)
	}
	fmt.Fprintln(out, "Next: preview the first sync with `bb sync --dry-run`, then run `bb sync`;")
	fmt.Fprintln(out, "start the daemon afterwards (it syncs mapped folders on every pass).")
	return nil
}

// Unmap stops syncing the mapped folder dir. Its local files and the bucket
// folder are left as they are.
func Unmap(ctx context.Context, st *store.Store, dir string, out io.Writer) error {
	abs, err := normalizeFolder(dir)
	if err != nil {
		return err
	}
	watched, err := st.ListWatched(ctx)
	if err != nil {
		return err
	}
	for _, f := range watched {
		if sameFolder(filepath.Clean(f.Path), abs, runtime.GOOS) {
			if !f.Synced() {
				return fmt.Errorf("%w: %s is backed up one way, not mapped (use `bb unwatch`)", apperr.ErrFolderNotWatched, f.Path)
			}
			return Unwatch(ctx, st, f.Path, out)
		}
	}
	return fmt.Errorf("%w: %s is not mapped", apperr.ErrFolderNotWatched, abs)
}

// Maps lists the mapped folders and their bucket folders.
func Maps(ctx context.Context, st *store.Store, out io.Writer) error {
	watched, err := st.ListWatched(ctx)
	if err != nil {
		return err
	}
	n := 0
	for _, f := range watched {
		if f.Synced() {
			fmt.Fprintf(out, "%s <-> %s\n", f.Path, remoteLabel(f.RemotePrefix))
			n++
		}
	}
	if n == 0 {
		fmt.Fprintln(out, "No folders are mapped. Map the current folder with `bb mapto <bucket-folder>`.")
	}
	return nil
}

// remoteLabel names a bucket prefix for output.
func remoteLabel(prefix string) string {
	if prefix == store.RootPrefix {
		return "(whole bucket)"
	}
	return prefix
}

// Sync runs `bb sync`: for ModeWholeBucket/ModeFolder it maps opt.Dir first
// (unless dry-running, which only previews) and syncs it; for SyncMapped it
// syncs every mapped folder.
func Sync(ctx context.Context, st *store.Store, be b2.Backend, opt SyncOptions, out io.Writer) error {
	if opt.Backend != "b2" {
		return errNeedsB2
	}
	if opt.DaemonRunning && !opt.DryRun {
		return fmt.Errorf("%w: the daemon is running and already syncs mapped folders every 5 minutes; "+
			"stop it first (`systemctl --user stop backuprepo` or `bb stop`), or preview with --dry-run", apperr.ErrSync)
	}
	unlock, err := syncer.Lock(opt.StateDir)
	if err != nil {
		return err
	}
	defer unlock()
	sopt := syncOpts(opt.StateDir, opt.DryRun, opt.Refill)
	if opt.Mode == ModeMapped {
		reports, err := syncer.All(ctx, st, be, sopt)
		failed := 0
		for _, r := range reports {
			printReport(out, r, opt.DryRun)
			failed += r.Failed
		}
		if len(reports) == 0 && err == nil {
			fmt.Fprintln(out, "No folders are mapped. Run `bb sync --folder <bucket-folder>` or `bb sync --all` in the folder to sync.")
		}
		if err == nil && failed > 0 {
			err = fmt.Errorf("%w: %d file(s) failed", apperr.ErrSync, failed)
		}
		return err
	}

	target, watched, err := planMapping(ctx, st, opt)
	if err != nil {
		return err
	}
	if !opt.DryRun {
		if err := persistMapping(ctx, st, target, out); err != nil {
			return err
		}
		if watched, err = st.ListWatched(ctx); err != nil {
			return err
		}
	}
	rep, err := syncer.Folder(ctx, st, be, watched, target, sopt)
	printReport(out, rep, opt.DryRun)
	if err == nil && rep.Failed > 0 {
		err = fmt.Errorf("%w: %d file(s) failed", apperr.ErrSync, rep.Failed)
	}
	return err
}

// planMapping returns the folder mapping `bb sync --all/--folder` (or `bb
// mapto`) asks for and the watch list as it would be with it, refusing
// mappings that would make two local folders sync the same bucket objects, a
// folder already mapped elsewhere or backed up one way (re-keying it would
// upload everything again — unmap/unwatch it first), and a folder holding bb's
// own state (its database and conflicts folder must never be synced).
func planMapping(ctx context.Context, st *store.Store, opt SyncOptions) (store.Folder, []store.Folder, error) {
	abs, err := normalizeFolder(opt.Dir)
	if err != nil {
		return store.Folder{}, nil, err
	}
	if opt.StateDir != "" {
		if r, err := filepath.Rel(abs, filepath.Clean(opt.StateDir)); err == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator)) {
			return store.Folder{}, nil, fmt.Errorf("%w: %s contains bb's own folder %s; map a folder inside it instead",
				apperr.ErrInvalidPrefix, abs, opt.StateDir)
		}
	}
	if info, err := os.Stat(abs); err != nil || !info.IsDir() {
		return store.Folder{}, nil, fmt.Errorf("%w: %s", apperr.ErrFolderNotFound, abs)
	}
	prefix := store.RootPrefix
	if opt.Mode == ModeFolder {
		if prefix, err = normalizeRemotePrefix(opt.Folder); err != nil {
			return store.Folder{}, nil, err
		}
	}
	watched, err := st.ListWatched(ctx)
	if err != nil {
		return store.Folder{}, nil, err
	}
	target := store.Folder{Path: abs, RemotePrefix: prefix}
	var others []store.Folder
	for _, f := range watched {
		if sameFolder(filepath.Clean(f.Path), abs, runtime.GOOS) {
			if f.RemotePrefix != prefix {
				what := "backed up one way (`bb unwatch` it first)"
				if f.Synced() {
					what = "mapped to " + remoteLabel(f.RemotePrefix) + " (`bb unmap` it first)"
				}
				return store.Folder{}, nil, fmt.Errorf("%w: %s is already %s", apperr.ErrInvalidPrefix, f.Path, what)
			}
			target.Path = f.Path // keep the stored spelling
			continue
		}
		others = append(others, f)
	}
	for _, f := range others {
		if err := checkSyncedOverlap(target, f); err != nil {
			return store.Folder{}, nil, err
		}
	}
	return target, append(others, target), nil
}

// checkSyncedOverlap refuses a mapping target next to folder f when both would
// sync the same bucket objects: two mapped folders whose bucket folders nest,
// or a whole-bucket mirror next to any other mapped folder. One-way folders
// coexist with a whole-bucket mirror: their objects are excluded from it.
func checkSyncedOverlap(target, f store.Folder) error {
	if !f.Synced() {
		if target.RemotePrefix == store.RootPrefix {
			return nil
		}
		return checkOverlap(target.RemotePrefix, f)
	}
	if target.RemotePrefix == store.RootPrefix || f.RemotePrefix == store.RootPrefix {
		return fmt.Errorf("%w: %s is already mapped to %s; a whole-bucket mirror can't share the bucket with another mapped folder (`bb unmap` it first)",
			apperr.ErrInvalidPrefix, f.Path, remoteLabel(f.RemotePrefix))
	}
	return checkOverlap(target.RemotePrefix, f)
}

// persistMapping stores target unless it is already on the watch list.
func persistMapping(ctx context.Context, st *store.Store, target store.Folder, out io.Writer) error {
	watched, err := st.ListWatched(ctx)
	if err != nil {
		return err
	}
	for _, f := range watched {
		if f.Path == target.Path {
			return nil
		}
	}
	if err := st.AddFolderWithPrefix(ctx, target.Path, target.RemotePrefix); err != nil {
		return err
	}
	fmt.Fprintf(out, "Mapped %s <-> %s\n", target.Path, remoteLabel(target.RemotePrefix))
	return nil
}

// SyncMapped syncs every mapped folder after a one-way backup run (`bb
// upload`, the web UI's Upload button, each daemon scan). With no mapped
// folder it does nothing; on the S3 backend it only warns, since sync needs
// native B2.
//
// stateDir is ~/backup_repo (sync lock, conflicts folder). When another bb
// process is syncing it skips quietly: that process covers the same folders.
func SyncMapped(ctx context.Context, st *store.Store, be b2.Backend, backendKind, stateDir string, out io.Writer) error {
	watched, err := st.ListWatched(ctx)
	if err != nil {
		return err
	}
	mapped := false
	for _, f := range watched {
		mapped = mapped || f.Synced()
	}
	if !mapped {
		return nil
	}
	if backendKind != "b2" {
		fmt.Fprintf(out, "warning: mapped folders are NOT backed up or synced: %v\n", errNeedsB2)
		return nil
	}
	unlock, err := syncer.Lock(stateDir)
	if errors.Is(err, syncer.ErrBusy) {
		fmt.Fprintln(out, "Mapped folders: another bb process is syncing them right now; skipped.")
		return nil
	}
	if err != nil {
		return err
	}
	defer unlock()
	reports, err := syncer.All(ctx, st, be, syncOpts(stateDir, false, false))
	for _, r := range reports {
		if len(r.Items) > 0 {
			printReport(out, r, false)
		}
	}
	return err
}

// printReport writes one folder's sync result: each file's action, then totals.
func printReport(out io.Writer, r syncer.Report, dryRun bool) {
	head := "Sync"
	if dryRun {
		head = "Sync preview (dry run, nothing changed)"
	}
	fmt.Fprintf(out, "%s: %s <-> %s\n", head, r.Folder.Path, remoteLabel(r.Folder.RemotePrefix))
	counts := map[syncer.Action]int{}
	for _, it := range r.Items {
		if it.Err != nil {
			fmt.Fprintf(out, "  FAILED %s %s: %v\n", it.Action, it.Rel, it.Err)
			continue
		}
		counts[it.Action]++
		fmt.Fprintf(out, "  %-50s %s\n", it.Action, it.Rel)
	}
	if dryRun {
		var parts []string
		for _, a := range []syncer.Action{syncer.Download, syncer.Upload, syncer.KeepRemote, syncer.KeepLocal, syncer.Record, syncer.Forget} {
			if counts[a] > 0 {
				parts = append(parts, fmt.Sprintf("%s: %d", a, counts[a]))
			}
		}
		if len(parts) == 0 {
			parts = []string{"nothing to do"}
		}
		fmt.Fprintf(out, "Would do: %s; failures: %d\n", strings.Join(parts, ", "), r.Failed)
		return
	}
	fmt.Fprintf(out, "Downloaded: %d, Uploaded: %d, Same on both sides: %d, Conflicts: %d, Failed: %d\n",
		r.Downloaded, r.Uploaded, r.Recorded, r.Conflicts, r.Failed)
	if r.Conflicts > 0 {
		fmt.Fprintln(out, "Local versions that lost a conflict were moved to ~/backup_repo/conflicts/.")
	}
}
