// CLI handlers for `bb service` — running the watcher daemon as a systemd user
// service. internal/service owns the unit file and systemctl; this layer adds
// the bb-specific pre-flight checks and makes sure the folder to back up is on
// the watch list *before* the unit (re)starts, because the daemon reads the
// folder list only once at startup.
package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"backuprepo/internal/apperr"
	"backuprepo/internal/daemon"
	"backuprepo/internal/store"
)

// DefaultServiceFolder is the folder `bb service install` watches when no
// --folder is given, relative to the user's home directory.
const DefaultServiceFolder = "Documents"

// ServiceManager is the systemd side of `bb service` (satisfied by
// *service.Manager; faked in tests).
type ServiceManager interface {
	Supported() error
	Preflight(ctx context.Context, force bool) error
	Active(ctx context.Context) bool
	Install(ctx context.Context, deleteRemoved, force bool, out io.Writer) error
	EnableLinger(ctx context.Context, out io.Writer)
	LingerHint(ctx context.Context, out io.Writer)
	Uninstall(ctx context.Context, out io.Writer) error
	Status(ctx context.Context, out io.Writer) error
}

// ServiceInstall makes sure folder (default ~/Documents) is watched, then
// installs, enables, and starts the daemon as a systemd user service. All
// pre-flight checks (OS, config, PID lock, folder, binary path, unit ownership,
// systemd reachability) run before the watch list or unit file is changed; only
// a systemctl failure after that point can leave the folder added. stateDir is
// ~/backup_repo (for the daemon PID file); home resolves "~" and the default.
// With linger, lingering is enabled (best-effort) after a successful install,
// so the service also runs at boot and after logout; a failed install leaves
// login policy untouched. Without it, a hint is printed while lingering is off.
func ServiceInstall(ctx context.Context, st *store.Store, mgr ServiceManager, folder, home, stateDir string,
	deleteRemoved, force, linger bool, out io.Writer) error {
	if err := mgr.Supported(); err != nil {
		return err
	}
	ok, err := st.IsConfigured(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return apperr.ErrNotConfigured
	}
	// A daemon started by hand (`bb start`) holds the PID lock; the unit would
	// crash-loop on "already running". If the unit itself is the live daemon,
	// restarting it is exactly what install wants. On Linux and Windows a reused
	// PID is recognised by its start time (ADR-023/024); other OSes use a plain
	// liveness probe, so the error names the PID file instead of prescribing
	// `bb stop` blindly.
	if pid, running := daemon.RunningPID(stateDir); running && !mgr.Active(ctx) {
		return fmt.Errorf("%w: %s names a live process (pid %d), so a daemon may already be running outside systemd; "+
			"if it is bb, run `bb stop` first, otherwise delete that PID file",
			apperr.ErrService, filepath.Join(stateDir, "daemon.pid"), pid)
	}
	path, err := resolveFolder(folder, home)
	if err != nil {
		return err
	}
	if err := checkFolderDir(path); err != nil {
		return err
	}
	if err := checkStoredFolders(ctx, st, deleteRemoved, out); err != nil {
		return err
	}
	if err := mgr.Preflight(ctx, force); err != nil {
		return err
	}
	if err := Watch(ctx, st, path, out); err != nil {
		return err
	}
	if err := mgr.Install(ctx, deleteRemoved, force, out); err != nil {
		return err
	}
	if linger {
		mgr.EnableLinger(ctx, out)
	} else {
		mgr.LingerHint(ctx, out)
	}
	return nil
}

// checkFolderDir verifies the folder to watch exists and is a directory, the
// same rule `bb watch` applies, so it is checked before anything is changed.
func checkFolderDir(path string) error {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("%w: %s", apperr.ErrFolderNotFound, path)
	}
	return nil
}

// checkStoredFolders guards against relative watch entries (stored verbatim by
// older `bb watch` calls). A systemd user unit runs in $HOME, so a relative
// entry would resolve to a different directory there; with --delete that could
// delete remote backups whose files merely "look" missing. Refuse with
// --delete, warn without it.
func checkStoredFolders(ctx context.Context, st *store.Store, deleteRemoved bool, out io.Writer) error {
	folders, err := st.ListFolders(ctx)
	if err != nil {
		return err
	}
	var rel []string
	for _, f := range folders {
		if !filepath.IsAbs(f) {
			rel = append(rel, f)
		}
	}
	if len(rel) == 0 {
		return nil
	}
	if deleteRemoved {
		return fmt.Errorf("%w: watched folder(s) %q are relative paths, which resolve against $HOME under systemd; "+
			"`bb unwatch` them and `bb watch` the absolute path before installing with --delete",
			apperr.ErrService, rel)
	}
	fmt.Fprintf(out, "Warning: watched folder(s) %q are relative paths; under systemd they resolve against $HOME. "+
		"Re-add them with an absolute path (`bb unwatch` + `bb watch`).\n", rel)
	return nil
}

// ServiceUninstall stops, disables, and removes the systemd user service.
func ServiceUninstall(ctx context.Context, mgr ServiceManager, out io.Writer) error {
	return mgr.Uninstall(ctx, out)
}

// ServiceStatus reports whether the systemd user service is installed/running.
func ServiceStatus(ctx context.Context, mgr ServiceManager, out io.Writer) error {
	return mgr.Status(ctx, out)
}

// resolveFolder turns the --folder value into a clean absolute path: empty
// means ~/Documents, and a leading "~" is expanded here because the shell does
// not expand it inside `--folder=~/x`.
func resolveFolder(folder, home string) (string, error) {
	if folder == "" {
		folder = filepath.Join(home, DefaultServiceFolder)
	}
	abs, err := filepath.Abs(expandHome(folder, home))
	if err != nil {
		return "", fmt.Errorf("%w: %s: %v", apperr.ErrFolderNotFound, folder, err)
	}
	return abs, nil
}
