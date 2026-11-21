// Package service installs the bb watcher daemon (`bb start`) as a systemd
// *user* service on Linux, so backups run in the background and resume on every
// login without a terminal. It is a user unit (not a system one) because all of
// bb's state — the master key and encrypted DB in ~/backup_repo — belongs to the
// login user.
//
// systemctl is driven through an injectable Runner so tests never touch the real
// service manager.
package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"backuprepo/internal/apperr"
)

// UnitName is the systemd unit bb manages. It matches the name the README
// previously told users to hand-write, so `systemctl --user ... backuprepo`
// keeps working.
const UnitName = "backuprepo.service"

// marker is the first line of every generated unit. Install only overwrites,
// and Uninstall only removes, unit files that carry it (or with --force), so a
// hand-written unit at the same path is never clobbered silently.
const marker = "# Managed by `bb service install`"

// Runner executes a command and returns its trimmed combined output.
type Runner func(ctx context.Context, name string, args ...string) (string, error)

// ExecRunner is the production Runner, backed by os/exec.
func ExecRunner(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// Manager installs, removes, and reports on the bb systemd user unit.
type Manager struct {
	UnitDir string // directory for user units, e.g. ~/.config/systemd/user
	Exe     string // absolute path of the bb binary the unit's ExecStart runs
	GOOS    string // target OS; only "linux" is supported
	Run     Runner // runs systemctl
	// Settle is how long Install waits after `restart` before checking that the
	// daemon is still running, so a startup failure (not configured, no
	// folders, PID lock held) is reported instead of a false "started".
	Settle time.Duration
}

// DefaultSettle is the production post-restart wait before the health check.
const DefaultSettle = 2 * time.Second

// NewManager returns a Manager for the current user and running binary.
func NewManager() (*Manager, error) {
	// Per the XDG spec a relative XDG_CONFIG_HOME is invalid and must be ignored.
	cfgDir := os.Getenv("XDG_CONFIG_HOME")
	if !filepath.IsAbs(cfgDir) {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("%w: home dir: %v", apperr.ErrService, err)
		}
		cfgDir = filepath.Join(home, ".config")
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("%w: locate bb binary: %v", apperr.ErrService, err)
	}
	if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		exe = resolved
	}
	return &Manager{
		UnitDir: filepath.Join(cfgDir, "systemd", "user"),
		Exe:     exe,
		GOOS:    runtime.GOOS,
		Run:     ExecRunner,
		Settle:  DefaultSettle,
	}, nil
}

// UnitPath is the full path of the managed unit file.
func (m *Manager) UnitPath() string { return filepath.Join(m.UnitDir, UnitName) }

// Supported returns an error unless the target OS uses systemd (Linux).
func (m *Manager) Supported() error {
	if m.GOOS != "linux" {
		return fmt.Errorf("%w: `bb service` needs systemd (Linux); on %s use the OS service manager to run `bb start`",
			apperr.ErrService, m.GOOS)
	}
	return nil
}

// Active reports whether the unit is currently running.
func (m *Manager) Active(ctx context.Context) bool {
	out, err := m.systemctl(ctx, "is-active", UnitName)
	return err == nil && out == "active"
}

// Render returns the unit file contents for running exe as the daemon.
//
// Type=exec (not simple) makes `systemctl restart` fail when the binary can't
// be executed, instead of reporting success for a unit that dies at once.
//
// Restart policy: `bb stop` and `systemctl stop` both make the daemon exit 0,
// so Restart=on-failure keeps a deliberate stop stopped while recovering from
// crashes. Startup failures that persist (DB not configured, no watched
// folders) would otherwise restart forever; the start limit (5 starts in 10
// minutes, with 30 s between them) makes systemd give up and mark the unit
// failed, which `bb service status` and journalctl surface. The limit also
// counts manual starts, so Install runs `reset-failed` before restarting.
func Render(exe string, deleteRemoved bool) string {
	execStart := quoteExecArg(exe) + " start"
	if deleteRemoved {
		execStart += " --delete"
	}
	return strings.Join([]string{
		marker + " - regenerated on reinstall; remove with `bb service uninstall`.",
		"[Unit]",
		"Description=bb (backuprepo) - back up watched folders to Backblaze B2",
		"StartLimitIntervalSec=600",
		"StartLimitBurst=5",
		"",
		"[Service]",
		"Type=exec",
		"ExecStart=" + execStart,
		"Restart=on-failure",
		"RestartSec=30",
		"",
		"[Install]",
		"WantedBy=default.target",
		"",
	}, "\n")
}

// quoteExecArg quotes the ExecStart executable path for systemd: it is
// double-quoted (so spaces survive) and `%` specifiers are doubled. `$` is left
// as-is: systemd does not expand variables in the executable itself (and does
// not un-double `$$` there). Characters systemd rejects in an executable path
// (quotes, backslashes, control characters) are refused earlier by checkExe.
func quoteExecArg(s string) string {
	return `"` + strings.ReplaceAll(s, "%", "%%") + `"`
}

// Preflight runs every check Install would fail on, without changing anything,
// so callers can validate before mutating other state (e.g. the watch list):
// supported OS, a usable binary path, unit-file ownership (unless force), and a
// reachable systemd user manager.
func (m *Manager) Preflight(ctx context.Context, force bool) error {
	if err := m.Supported(); err != nil {
		return err
	}
	if err := checkExe(m.Exe); err != nil {
		return err
	}
	managed, exists, err := m.unitState()
	if err != nil {
		return err
	}
	if exists && !managed && !force {
		return fmt.Errorf("%w: %s exists and was not created by bb; move it aside or re-run with --force to replace it",
			apperr.ErrService, m.UnitPath())
	}
	if _, err := m.systemctl(ctx, "show-environment"); err != nil {
		return fmt.Errorf("%w (is a systemd user session running?)", err)
	}
	return nil
}

// Install writes the unit file, then reloads systemd, enables the unit for
// future logins, and (re)starts it so it picks up the current folder list.
// An existing unit not generated by bb is refused unless force is set. After
// the restart it waits Settle and fails if the daemon is no longer running.
func (m *Manager) Install(ctx context.Context, deleteRemoved, force bool, out io.Writer) error {
	if err := m.Preflight(ctx, force); err != nil {
		return err
	}
	if !deleteRemoved && m.unitHasDelete() {
		fmt.Fprintln(out, "Note: the previous service ran with --delete; this install turns deletion propagation OFF.")
	}
	if err := m.writeUnit(Render(m.Exe, deleteRemoved)); err != nil {
		return err
	}
	fmt.Fprintf(out, "Wrote %s\n", m.UnitPath())

	if _, err := m.systemctl(ctx, "daemon-reload"); err != nil {
		return err
	}
	if _, err := m.systemctl(ctx, "enable", UnitName); err != nil {
		return err
	}
	// The start limit counts manual restarts too; clear any earlier failure so a
	// re-install (e.g. to add a folder, or after fixing a failed unit) can start.
	_, _ = m.Run(ctx, "systemctl", "--user", "reset-failed", UnitName)
	if _, err := m.systemctl(ctx, "restart", UnitName); err != nil {
		return fmt.Errorf("%w (see: journalctl --user -u %s)", err, UnitName)
	}
	if m.Settle > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(m.Settle):
		}
	}
	if state, _ := m.Run(ctx, "systemctl", "--user", "is-active", UnitName); state != "active" {
		return fmt.Errorf("%w: %s did not stay running (state %q); see: journalctl --user -u %s",
			apperr.ErrService, UnitName, orUnknown(state), UnitName)
	}
	fmt.Fprintf(out, "Service %s enabled and running (runs %s start", UnitName, m.Exe)
	if deleteRemoved {
		fmt.Fprint(out, " --delete")
	}
	fmt.Fprintln(out, ").")
	fmt.Fprintf(out, "Logs:   journalctl --user -u %s -f\n", UnitName)
	return nil
}

// LingerHint tells the user how to keep the service running after logout and
// start it at boot, unless lingering is already on.
func (m *Manager) LingerHint(ctx context.Context, out io.Writer) {
	if m.lingering(ctx) {
		return
	}
	fmt.Fprintln(out, "Note:   user services run while you are logged in; to keep backing up after logout")
	fmt.Fprintln(out, "        and start at boot, run once: loginctl enable-linger $USER  (or reinstall with --linger)")
}

// uid is the current user's numeric ID, the form loginctl accepts for a user.
func uid() string { return strconv.Itoa(os.Getuid()) }

// lingering reports whether systemd lingering is on for the current user. An
// unanswerable query (no loginctl, no logind) reads as off.
func (m *Manager) lingering(ctx context.Context) bool {
	out, err := m.Run(ctx, "loginctl", "show-user", uid(), "--property=Linger", "--value")
	return err == nil && out == "yes"
}

// EnableLinger turns on systemd lingering for the current user, so the user
// service starts at boot and keeps running after logout. It changes the
// user's login policy, so it only runs on request (`bb service install
// --linger`). A failure is a warning, not an error: the service still runs
// while the user is logged in.
func (m *Manager) EnableLinger(ctx context.Context, out io.Writer) {
	if m.lingering(ctx) {
		fmt.Fprintln(out, "Lingering is already enabled: the service starts at boot and runs after logout.")
		return
	}
	if res, err := m.Run(ctx, "loginctl", "enable-linger", uid()); err != nil {
		detail := ""
		if res != "" {
			detail = ": " + res
		}
		fmt.Fprintf(out, "Warning: could not enable lingering (%v%s); run once: loginctl enable-linger $USER\n", err, detail)
		return
	}
	fmt.Fprintln(out, "Lingering enabled: the service starts at boot and keeps running after logout.")
}

// State is the service's state for `bb status`: "not installed", or systemd's
// is-active answer (active, inactive, failed, ...). It is "" where bb service
// does not apply (not Linux), and "unknown" when systemd can't be asked.
func (m *Manager) State(ctx context.Context) string {
	if m.Supported() != nil {
		return ""
	}
	_, exists, err := m.unitState()
	if err != nil {
		return "unknown"
	}
	if !exists {
		return "not installed"
	}
	active, _ := m.Run(ctx, "systemctl", "--user", "is-active", UnitName)
	// Run returns combined output, so a failure to reach the user manager
	// (no bus over SSH/cron/sudo) arrives as an error message, not a state.
	switch active {
	case "active", "inactive", "failed", "activating", "deactivating", "reloading", "maintenance", "refreshing":
		return active
	}
	return "unknown"
}

// Uninstall stops and disables the unit and removes its file. It is a no-op if
// nothing is installed, and refuses to remove a unit bb did not generate.
func (m *Manager) Uninstall(ctx context.Context, out io.Writer) error {
	if err := m.Supported(); err != nil {
		return err
	}
	managed, exists, err := m.unitState()
	if err != nil {
		return err
	}
	if !exists {
		fmt.Fprintf(out, "Service %s is not installed.\n", UnitName)
		return nil
	}
	if !managed {
		return fmt.Errorf("%w: %s was not created by bb; remove it manually", apperr.ErrService, m.UnitPath())
	}
	if _, err := m.systemctl(ctx, "disable", "--now", UnitName); err != nil {
		return err
	}
	if err := os.Remove(m.UnitPath()); err != nil {
		return fmt.Errorf("%w: remove %s: %v", apperr.ErrService, m.UnitPath(), err)
	}
	if _, err := m.systemctl(ctx, "daemon-reload"); err != nil {
		return err
	}
	fmt.Fprintf(out, "Service %s stopped, disabled, and removed. Watched folders are unchanged.\n", UnitName)
	return nil
}

// Status prints whether the unit is installed, enabled, and running.
func (m *Manager) Status(ctx context.Context, out io.Writer) error {
	if err := m.Supported(); err != nil {
		return err
	}
	managed, exists, err := m.unitState()
	if err != nil {
		return err
	}
	switch {
	case !exists:
		fmt.Fprintf(out, "Unit:    %s (not installed; run `bb service install`)\n", m.UnitPath())
		return nil
	case managed:
		fmt.Fprintf(out, "Unit:    %s\n", m.UnitPath())
	default:
		fmt.Fprintf(out, "Unit:    %s (hand-written, not managed by bb)\n", m.UnitPath())
	}
	// is-enabled / is-active exit non-zero for "disabled" / "inactive" / "failed"
	// while still printing the state, so report their output, not their error.
	enabled, _ := m.Run(ctx, "systemctl", "--user", "is-enabled", UnitName)
	active, _ := m.Run(ctx, "systemctl", "--user", "is-active", UnitName)
	fmt.Fprintf(out, "Enabled: %s\n", orUnknown(enabled))
	fmt.Fprintf(out, "Active:  %s\n", orUnknown(active))
	fmt.Fprintf(out, "Logs:    journalctl --user -u %s -f\n", UnitName)
	return nil
}

// systemctl runs `systemctl --user args...`, wrapping failures with its output.
func (m *Manager) systemctl(ctx context.Context, args ...string) (string, error) {
	full := append([]string{"--user"}, args...)
	out, err := m.Run(ctx, "systemctl", full...)
	if err != nil {
		detail := ""
		if out != "" {
			detail = ": " + out
		}
		return out, fmt.Errorf("%w: systemctl --user %s: %v%s",
			apperr.ErrService, strings.Join(args, " "), err, detail)
	}
	return out, nil
}

// unitState reports whether a unit file exists and whether bb generated it.
func (m *Manager) unitState() (managed, exists bool, err error) {
	data, err := os.ReadFile(m.UnitPath())
	if errors.Is(err, os.ErrNotExist) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("%w: read %s: %v", apperr.ErrService, m.UnitPath(), err)
	}
	return strings.HasPrefix(string(data), marker), true, nil
}

// writeUnit atomically writes the unit file (temp file + rename), creating the
// unit directory if needed.
func (m *Manager) writeUnit(content string) error {
	if err := os.MkdirAll(m.UnitDir, 0o755); err != nil {
		return fmt.Errorf("%w: mkdir %s: %v", apperr.ErrService, m.UnitDir, err)
	}
	tmp := m.UnitPath() + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return fmt.Errorf("%w: write %s: %v", apperr.ErrService, tmp, err)
	}
	if err := os.Rename(tmp, m.UnitPath()); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("%w: install %s: %v", apperr.ErrService, m.UnitPath(), err)
	}
	return nil
}

// unitHasDelete reports whether the installed unit runs the daemon with --delete.
func (m *Manager) unitHasDelete() bool {
	data, err := os.ReadFile(m.UnitPath())
	return err == nil && strings.Contains(string(data), " start --delete")
}

// checkExe rejects binary paths a long-lived unit must not point at: relative
// paths, the throwaway binaries `go run` builds under the temp dir, and paths
// with characters systemd refuses in an executable (quotes, backslashes,
// control characters — a newline would even inject a unit directive).
func checkExe(exe string) error {
	if !filepath.IsAbs(exe) {
		return fmt.Errorf("%w: binary path %q is not absolute", apperr.ErrService, exe)
	}
	if strings.ContainsAny(exe, "\"'\\") || strings.IndexFunc(exe, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return fmt.Errorf("%w: binary path %q contains characters systemd cannot run; install bb to a plain path (e.g. make install)",
			apperr.ErrService, exe)
	}
	if strings.Contains(exe, string(filepath.Separator)+"go-build") {
		return fmt.Errorf("%w: %s is a temporary `go run` binary; install bb first (make install) and run `bb service install` from it",
			apperr.ErrService, exe)
	}
	return nil
}

// orUnknown substitutes "unknown" for empty systemctl output.
func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
