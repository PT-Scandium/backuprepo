package service

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"backuprepo/internal/apperr"
)

// fakeRunner records systemctl invocations and returns canned results keyed by
// the joined args (e.g. "--user restart backuprepo.service").
type fakeRunner struct {
	calls   []string
	outputs map[string]string
	errs    map[string]error
}

func (f *fakeRunner) run(_ context.Context, name string, args ...string) (string, error) {
	key := strings.Join(args, " ")
	f.calls = append(f.calls, name+" "+key)
	return f.outputs[key], f.errs[key]
}

// newManager returns a Linux Manager over a temp unit dir and a fake runner
// whose unit reports "active" (healthy) unless a test overrides it.
func newManager(t *testing.T) (*Manager, *fakeRunner) {
	t.Helper()
	// The fixtures model a Linux host (Unix exe paths, systemd). bb service
	// refuses other OSes; TestUnsupportedOS covers that by setting GOOS.
	if runtime.GOOS != "linux" {
		t.Skip("systemd user units are Linux-only; fixtures use Unix paths")
	}
	fr := &fakeRunner{
		outputs: map[string]string{"--user is-active backuprepo.service": "active"},
		errs:    map[string]error{},
	}
	return &Manager{
		UnitDir: filepath.Join(t.TempDir(), "systemd", "user"),
		Exe:     "/home/u/.local/bin/bb",
		GOOS:    "linux",
		Run:     fr.run,
	}, fr
}

func TestRenderUnit(t *testing.T) {
	got := Render("/home/u/.local/bin/bb", false)
	for _, want := range []string{
		marker,
		"[Unit]", "StartLimitIntervalSec=600", "StartLimitBurst=5",
		"[Service]", "Type=exec",
		`ExecStart="/home/u/.local/bin/bb" start` + "\n",
		"Restart=on-failure", "RestartSec=30",
		"[Install]", "WantedBy=default.target",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("unit missing %q:\n%s", want, got)
		}
	}
	if !strings.HasPrefix(got, marker) {
		t.Errorf("unit must start with the managed marker:\n%s", got)
	}
	if strings.Contains(got, "--delete") {
		t.Errorf("--delete must be opt-in:\n%s", got)
	}
}

func TestRenderUnitWithDelete(t *testing.T) {
	got := Render("/opt/bb", true)
	if !strings.Contains(got, `ExecStart="/opt/bb" start --delete`+"\n") {
		t.Errorf("want --delete on ExecStart:\n%s", got)
	}
}

// TestQuoteExecArg covers systemd's rules for the executable path: quoted so
// spaces survive, `%` specifiers doubled, and `$` left alone (systemd neither
// expands nor un-doubles `$` in argv[0]; verified with systemd-analyze).
func TestQuoteExecArg(t *testing.T) {
	cases := map[string]string{
		"/usr/bin/bb":      `"/usr/bin/bb"`,
		"/home/a b/bin/bb": `"/home/a b/bin/bb"`,
		"/home/100%/bb":    `"/home/100%%/bb"`,
		"/home/$USER/bb":   `"/home/$USER/bb"`,
	}
	for in, want := range cases {
		if got := quoteExecArg(in); got != want {
			t.Errorf("quoteExecArg(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestInstallWritesUnitAndStartsService(t *testing.T) {
	m, fr := newManager(t)
	var out bytes.Buffer
	if err := m.Install(context.Background(), false, false, &out); err != nil {
		t.Fatalf("Install: %v", err)
	}
	data, err := os.ReadFile(m.UnitPath())
	if err != nil {
		t.Fatalf("unit not written: %v", err)
	}
	if string(data) != Render(m.Exe, false) {
		t.Errorf("unit content mismatch:\n%s", data)
	}
	if _, err := os.Stat(m.UnitPath() + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("temp file left behind: %v", err)
	}
	// Order matters: probe the user manager, reload so systemd sees the new
	// file, enable for future logins, clear any start-limit failure, restart
	// (not start) so a running unit picks up new folders, then health-check.
	want := []string{
		"systemctl --user show-environment",
		"systemctl --user daemon-reload",
		"systemctl --user enable backuprepo.service",
		"systemctl --user reset-failed backuprepo.service",
		"systemctl --user restart backuprepo.service",
		"systemctl --user is-active backuprepo.service",
		"loginctl show-user " + uid() + " --property=Linger --value", // decides the linger hint
	}
	if !reflect.DeepEqual(fr.calls, want) {
		t.Errorf("systemctl calls = %q, want %q", fr.calls, want)
	}
	for _, s := range []string{"enabled and running", "journalctl --user -u backuprepo.service", "enable-linger"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("output missing %q:\n%s", s, out.String())
		}
	}
}

func TestInstallReinstallOverwritesManagedUnit(t *testing.T) {
	m, _ := newManager(t)
	ctx := context.Background()
	if err := m.Install(ctx, false, false, &bytes.Buffer{}); err != nil {
		t.Fatalf("first Install: %v", err)
	}
	if err := m.Install(ctx, true, false, &bytes.Buffer{}); err != nil {
		t.Fatalf("reinstall: %v", err)
	}
	data, _ := os.ReadFile(m.UnitPath())
	if !strings.Contains(string(data), "start --delete") {
		t.Errorf("reinstall did not update the unit:\n%s", data)
	}
}

func TestInstallRefusesHandWrittenUnitWithoutForce(t *testing.T) {
	m, fr := newManager(t)
	ctx := context.Background()
	handWritten := "[Service]\nExecStart=%h/.local/bin/bb start\n"
	if err := os.MkdirAll(m.UnitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.UnitPath(), []byte(handWritten), 0o644); err != nil {
		t.Fatal(err)
	}

	err := m.Install(ctx, false, false, &bytes.Buffer{})
	if !errors.Is(err, apperr.ErrService) || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("want ErrService mentioning --force, got %v", err)
	}
	if data, _ := os.ReadFile(m.UnitPath()); string(data) != handWritten {
		t.Errorf("hand-written unit was modified:\n%s", data)
	}
	if len(fr.calls) != 0 {
		t.Errorf("systemctl must not run on refusal, got %q", fr.calls)
	}

	if err := m.Install(ctx, false, true, &bytes.Buffer{}); err != nil {
		t.Fatalf("Install --force: %v", err)
	}
	if data, _ := os.ReadFile(m.UnitPath()); !strings.HasPrefix(string(data), marker) {
		t.Errorf("--force did not replace the unit:\n%s", data)
	}
}

func TestInstallSystemctlFailure(t *testing.T) {
	m, fr := newManager(t)
	fr.errs["--user restart backuprepo.service"] = errors.New("exit status 1")
	fr.outputs["--user restart backuprepo.service"] = "Job for backuprepo.service failed."
	err := m.Install(context.Background(), false, false, &bytes.Buffer{})
	if !errors.Is(err, apperr.ErrService) {
		t.Fatalf("want ErrService, got %v", err)
	}
	for _, s := range []string{"restart", "Job for backuprepo.service failed.", "journalctl"} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("error missing %q: %v", s, err)
		}
	}
}

func TestInstallRejectsBadExecutables(t *testing.T) {
	for _, exe := range []string{
		"bb",                              // relative
		"/tmp/go-build123456/b001/exe/bb", // `go run` throwaway
		`/home/q"uote/bb`,                 // systemd rejects quotes...
		"/home/o'brien/bb",                //
		`/home/back\slash/bb`,             // ...and backslashes in an executable
		"/home/u/bb\n[Service]",           // newline would inject a directive
	} {
		t.Run(exe, func(t *testing.T) {
			m, fr := newManager(t)
			m.Exe = exe
			err := m.Install(context.Background(), false, false, &bytes.Buffer{})
			if !errors.Is(err, apperr.ErrService) {
				t.Fatalf("want ErrService, got %v", err)
			}
			if _, statErr := os.Stat(m.UnitPath()); !errors.Is(statErr, os.ErrNotExist) {
				t.Errorf("unit must not be written for %q", exe)
			}
			if len(fr.calls) != 0 {
				t.Errorf("systemctl must not run, got %q", fr.calls)
			}
		})
	}
}

func TestUnsupportedOS(t *testing.T) {
	m, fr := newManager(t)
	m.GOOS = "windows"
	ctx := context.Background()
	for name, call := range map[string]func() error{
		"install":   func() error { return m.Install(ctx, false, false, &bytes.Buffer{}) },
		"uninstall": func() error { return m.Uninstall(ctx, &bytes.Buffer{}) },
		"status":    func() error { return m.Status(ctx, &bytes.Buffer{}) },
	} {
		if err := call(); !errors.Is(err, apperr.ErrService) || !strings.Contains(err.Error(), "systemd") {
			t.Errorf("%s: want ErrService about systemd, got %v", name, err)
		}
	}
	if len(fr.calls) != 0 {
		t.Errorf("systemctl must not run on unsupported OS, got %q", fr.calls)
	}
}

func TestUninstall(t *testing.T) {
	m, fr := newManager(t)
	ctx := context.Background()
	if err := m.Install(ctx, false, false, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	fr.calls = nil

	var out bytes.Buffer
	if err := m.Uninstall(ctx, &out); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, err := os.Stat(m.UnitPath()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("unit file still present: %v", err)
	}
	want := []string{
		"systemctl --user disable --now backuprepo.service",
		"systemctl --user daemon-reload",
	}
	if !reflect.DeepEqual(fr.calls, want) {
		t.Errorf("systemctl calls = %q, want %q", fr.calls, want)
	}
	if !strings.Contains(out.String(), "removed") {
		t.Errorf("output = %q", out.String())
	}
}

func TestUninstallNotInstalledIsNoop(t *testing.T) {
	m, fr := newManager(t)
	var out bytes.Buffer
	if err := m.Uninstall(context.Background(), &out); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if !strings.Contains(out.String(), "not installed") || len(fr.calls) != 0 {
		t.Errorf("want no-op, got output %q calls %q", out.String(), fr.calls)
	}
}

func TestUninstallRefusesHandWrittenUnit(t *testing.T) {
	m, fr := newManager(t)
	_ = os.MkdirAll(m.UnitDir, 0o755)
	_ = os.WriteFile(m.UnitPath(), []byte("[Service]\n"), 0o644)
	err := m.Uninstall(context.Background(), &bytes.Buffer{})
	if !errors.Is(err, apperr.ErrService) {
		t.Fatalf("want ErrService, got %v", err)
	}
	if _, statErr := os.Stat(m.UnitPath()); statErr != nil {
		t.Errorf("hand-written unit must be kept: %v", statErr)
	}
	if len(fr.calls) != 0 {
		t.Errorf("systemctl must not run, got %q", fr.calls)
	}
}

func TestUninstallDisableFailure(t *testing.T) {
	m, fr := newManager(t)
	ctx := context.Background()
	_ = m.Install(ctx, false, false, &bytes.Buffer{})
	fr.errs["--user disable --now backuprepo.service"] = errors.New("exit status 1")
	if err := m.Uninstall(ctx, &bytes.Buffer{}); !errors.Is(err, apperr.ErrService) {
		t.Fatalf("want ErrService, got %v", err)
	}
	if _, err := os.Stat(m.UnitPath()); err != nil {
		t.Errorf("unit must be kept when disable fails: %v", err)
	}
}

func TestStatus(t *testing.T) {
	m, fr := newManager(t)
	ctx := context.Background()

	var out bytes.Buffer
	if err := m.Status(ctx, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "not installed") {
		t.Errorf("want not installed, got %q", out.String())
	}

	_ = m.Install(ctx, false, false, &bytes.Buffer{})
	// is-active exits non-zero (3) for an inactive unit but still prints the
	// state; Status must report the state, not fail.
	fr.outputs["--user is-enabled backuprepo.service"] = "enabled"
	fr.outputs["--user is-active backuprepo.service"] = "failed"
	fr.errs["--user is-active backuprepo.service"] = errors.New("exit status 3")
	out.Reset()
	if err := m.Status(ctx, &out); err != nil {
		t.Fatalf("Status: %v", err)
	}
	for _, s := range []string{"Enabled: enabled", "Active:  failed", "journalctl"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("status missing %q:\n%s", s, out.String())
		}
	}
}

func TestStatusHandWrittenUnitAndUnknownState(t *testing.T) {
	m, fr := newManager(t)
	delete(fr.outputs, "--user is-active backuprepo.service") // systemctl printed nothing
	_ = os.MkdirAll(m.UnitDir, 0o755)
	_ = os.WriteFile(m.UnitPath(), []byte("[Service]\n"), 0o644)
	var out bytes.Buffer
	if err := m.Status(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"hand-written", "Enabled: unknown", "Active:  unknown"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("status missing %q:\n%s", s, out.String())
		}
	}
}

func TestActive(t *testing.T) {
	m, fr := newManager(t)
	ctx := context.Background()
	fr.outputs["--user is-active backuprepo.service"] = "active"
	if !m.Active(ctx) {
		t.Error("want active")
	}
	fr.outputs["--user is-active backuprepo.service"] = "inactive"
	fr.errs["--user is-active backuprepo.service"] = errors.New("exit status 3")
	if m.Active(ctx) {
		t.Error("want inactive")
	}
}

func TestNewManagerUsesXDGConfigHome(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	m, err := NewManager()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(xdg, "systemd", "user"); m.UnitDir != want {
		t.Errorf("UnitDir = %q, want %q", m.UnitDir, want)
	}
	if !filepath.IsAbs(m.Exe) || m.Run == nil || m.GOOS == "" {
		t.Errorf("incomplete manager: %+v", m)
	}
}

func TestNewManagerDefaultsToHomeConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows
	m, err := NewManager()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".config", "systemd", "user"); m.UnitDir != want {
		t.Errorf("UnitDir = %q, want %q", m.UnitDir, want)
	}
}

func TestExecRunnerTrimsOutput(t *testing.T) {
	out, err := ExecRunner(context.Background(), "echo", "  hi  ")
	if err != nil || out != "hi" {
		t.Errorf("ExecRunner = %q, %v", out, err)
	}
}

func TestInstallResetFailedErrorIsIgnored(t *testing.T) {
	m, fr := newManager(t)
	// reset-failed errors when the unit was never loaded/failed; that's fine.
	fr.errs["--user reset-failed backuprepo.service"] = errors.New("exit status 1")
	if err := m.Install(context.Background(), false, false, &bytes.Buffer{}); err != nil {
		t.Fatalf("Install: %v", err)
	}
}

// TestInstallReportsDaemonThatDidNotStayRunning: with Restart=on-failure a
// daemon that exits at startup shows "activating" (auto-restart) or "failed";
// install must fail loudly rather than claim success.
func TestInstallReportsDaemonThatDidNotStayRunning(t *testing.T) {
	m, fr := newManager(t)
	m.Settle = time.Millisecond
	fr.outputs["--user is-active backuprepo.service"] = "activating"
	fr.errs["--user is-active backuprepo.service"] = errors.New("exit status 3")
	err := m.Install(context.Background(), false, false, &bytes.Buffer{})
	if !errors.Is(err, apperr.ErrService) || !strings.Contains(err.Error(), "activating") ||
		!strings.Contains(err.Error(), "journalctl") {
		t.Fatalf("want ErrService naming the state + journalctl, got %v", err)
	}
}

func TestInstallSettleHonoursContext(t *testing.T) {
	m, _ := newManager(t)
	m.Settle = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.Install(ctx, false, false, &bytes.Buffer{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func TestPreflightUserManagerUnreachable(t *testing.T) {
	m, fr := newManager(t)
	fr.errs["--user show-environment"] = errors.New("exit status 1")
	fr.outputs["--user show-environment"] = "Failed to connect to bus"
	err := m.Install(context.Background(), false, false, &bytes.Buffer{})
	if !errors.Is(err, apperr.ErrService) || !strings.Contains(err.Error(), "systemd user session") {
		t.Fatalf("want ErrService about the user session, got %v", err)
	}
	if _, statErr := os.Stat(m.UnitPath()); !errors.Is(statErr, os.ErrNotExist) {
		t.Error("unit must not be written when the user manager is unreachable")
	}
}

func TestInstallNotesDroppedDelete(t *testing.T) {
	m, _ := newManager(t)
	ctx := context.Background()
	if err := m.Install(ctx, true, false, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := m.Install(ctx, false, false, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "deletion propagation OFF") {
		t.Errorf("want a note that --delete was dropped, got %q", out.String())
	}
	out.Reset()
	if err := m.Install(ctx, false, false, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "deletion propagation OFF") {
		t.Errorf("no note expected when --delete was already off: %q", out.String())
	}
}

func TestNewManagerIgnoresRelativeXDGConfigHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", "relative/cfg")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows
	m, err := NewManager()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".config", "systemd", "user"); m.UnitDir != want {
		t.Errorf("UnitDir = %q, want %q", m.UnitDir, want)
	}
	if m.Settle != DefaultSettle {
		t.Errorf("Settle = %v, want %v", m.Settle, DefaultSettle)
	}
}

// TestInstallLingerHint checks the "enable lingering" note appears only while
// lingering is off.
func TestInstallLingerHint(t *testing.T) {
	for _, on := range []bool{false, true} {
		m, fr := newManager(t)
		if on {
			fr.outputs["show-user "+uid()+" --property=Linger --value"] = "yes"
		}
		var out bytes.Buffer
		if err := m.Install(context.Background(), false, false, &out); err != nil {
			t.Fatalf("Install: %v", err)
		}
		if got := strings.Contains(out.String(), "loginctl enable-linger"); got == on {
			t.Errorf("lingering=%v: hint shown=%v, want %v:\n%s", on, got, !on, out.String())
		}
	}
}

// TestEnableLinger covers --linger: it enables lingering for the current UID,
// skips the call when already on, and only warns on failure.
func TestEnableLinger(t *testing.T) {
	ctx := context.Background()
	enable := "loginctl enable-linger " + uid()

	m, fr := newManager(t)
	var out bytes.Buffer
	m.EnableLinger(ctx, &out)
	if !strings.Contains(strings.Join(fr.calls, "|"), enable) || !strings.Contains(out.String(), "Lingering enabled") {
		t.Errorf("enable: calls %q, out %q", fr.calls, out.String())
	}

	m, fr = newManager(t)
	fr.outputs["show-user "+uid()+" --property=Linger --value"] = "yes"
	out.Reset()
	m.EnableLinger(ctx, &out)
	if strings.Contains(strings.Join(fr.calls, "|"), enable) || !strings.Contains(out.String(), "already enabled") {
		t.Errorf("already on: calls %q, out %q", fr.calls, out.String())
	}

	m, fr = newManager(t)
	fr.errs["enable-linger "+uid()] = errors.New("exit status 1")
	fr.outputs["enable-linger "+uid()] = "Access denied"
	out.Reset()
	m.EnableLinger(ctx, &out)
	if got := out.String(); !strings.Contains(got, "Warning: could not enable lingering") || !strings.Contains(got, "Access denied") {
		t.Errorf("failure should warn with the detail: %q", got)
	}
}

// TestState covers the `bb status` service line.
func TestState(t *testing.T) {
	ctx := context.Background()
	m, fr := newManager(t)
	if got := m.State(ctx); got != "not installed" {
		t.Errorf("no unit: State = %q", got)
	}
	if err := m.writeUnit(Render(m.Exe, false)); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ out, want string }{{"active", "active"}, {"failed", "failed"}, {"", "unknown"}} {
		fr.outputs["--user is-active backuprepo.service"] = tc.out
		if got := m.State(ctx); got != tc.want {
			t.Errorf("is-active %q: State = %q, want %q", tc.out, got, tc.want)
		}
	}
	m.GOOS = "windows"
	if got := m.State(ctx); got != "" {
		t.Errorf("unsupported OS: State = %q, want empty", got)
	}
}
