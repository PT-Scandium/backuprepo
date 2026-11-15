package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"backuprepo/internal/apperr"
	"backuprepo/internal/store"
)

// fakeServiceManager records what ServiceInstall asked the systemd layer to do.
type fakeServiceManager struct {
	unsupported   error
	preflightErr  error
	active        bool
	installErr    error
	installed     bool
	deleteRemoved bool
	force         bool
}

func (f *fakeServiceManager) Supported() error                           { return f.unsupported }
func (f *fakeServiceManager) Active(context.Context) bool                { return f.active }
func (f *fakeServiceManager) Preflight(context.Context, bool) error      { return f.preflightErr }
func (f *fakeServiceManager) Uninstall(context.Context, io.Writer) error { return nil }
func (f *fakeServiceManager) Status(context.Context, io.Writer) error    { return nil }
func (f *fakeServiceManager) Install(_ context.Context, del, force bool, _ io.Writer) error {
	f.installed, f.deleteRemoved, f.force = true, del, force
	return f.installErr
}

// configuredStore returns a store that has completed `bb init`.
func configuredStore(t *testing.T) *store.Store {
	t.Helper()
	st := newStore(t)
	err := st.SaveConfig(context.Background(), store.RemoteConfig{
		KeyID: "k", AppKey: "a", Bucket: "b",
		Endpoint: "https://s3.us-west-004.backblazeb2.com", Region: "us-west-004",
	})
	if err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	return st
}

func TestServiceInstallDefaultsToDocuments(t *testing.T) {
	st := configuredStore(t)
	home := t.TempDir()
	docs := filepath.Join(home, "Documents")
	if err := os.Mkdir(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	mgr := &fakeServiceManager{}
	var out bytes.Buffer
	err := ServiceInstall(context.Background(), st, mgr, "", home, t.TempDir(), true, true, &out)
	if err != nil {
		t.Fatalf("ServiceInstall: %v", err)
	}
	folders, _ := st.ListFolders(context.Background())
	if len(folders) != 1 || folders[0] != docs {
		t.Errorf("folders = %q, want [%q]", folders, docs)
	}
	if !mgr.installed || !mgr.deleteRemoved || !mgr.force {
		t.Errorf("Install not called with flags: %+v", mgr)
	}
	if !strings.Contains(out.String(), "Watching "+docs) {
		t.Errorf("output = %q", out.String())
	}
}

// TestServiceInstallReusesEquivalentFolder guards against a near-duplicate
// watch entry: remote keys derive from the stored path, so re-adding the same
// folder under another spelling would re-upload everything.
func TestServiceInstallReusesEquivalentFolder(t *testing.T) {
	st := configuredStore(t)
	ctx := context.Background()
	home := t.TempDir()
	docs := filepath.Join(home, "Documents")
	_ = os.Mkdir(docs, 0o755)
	if err := st.AddFolder(ctx, docs+string(filepath.Separator)); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := ServiceInstall(ctx, st, &fakeServiceManager{}, "~/Documents", home, t.TempDir(), false, false, &out); err != nil {
		t.Fatalf("ServiceInstall: %v", err)
	}
	folders, _ := st.ListFolders(ctx)
	if len(folders) != 1 {
		t.Errorf("want the existing entry reused, got %q", folders)
	}
	if !strings.Contains(out.String(), "Already watching") {
		t.Errorf("output = %q", out.String())
	}
}

func TestServiceInstallFailsBeforeChangingAnything(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	_ = os.Mkdir(filepath.Join(home, "Documents"), 0o755)

	// A live PID file not owned by the (inactive) unit = a manual `bb start`.
	runningDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(runningDir, "daemon.pid"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name     string
		st       *store.Store
		mgr      *fakeServiceManager
		folder   string
		stateDir string
		want     error
	}{
		{"unsupported OS", configuredStore(t), &fakeServiceManager{unsupported: apperr.ErrService}, "", t.TempDir(), apperr.ErrService},
		{"not configured", newStore(t), &fakeServiceManager{}, "", t.TempDir(), apperr.ErrNotConfigured},
		{"manual daemon running", configuredStore(t), &fakeServiceManager{}, "", runningDir, apperr.ErrService},
		{"folder missing", configuredStore(t), &fakeServiceManager{}, filepath.Join(home, "nope"), t.TempDir(), apperr.ErrFolderNotFound},
		// e.g. a hand-written unit without --force, a `go run` binary, or no
		// user bus: the watch list must stay untouched.
		{"service preflight fails", configuredStore(t), &fakeServiceManager{preflightErr: apperr.ErrService}, "", t.TempDir(), apperr.ErrService},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ServiceInstall(ctx, tc.st, tc.mgr, tc.folder, home, tc.stateDir, false, false, &bytes.Buffer{})
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if tc.mgr.installed {
				t.Error("Install must not be called")
			}
			if folders, _ := tc.st.ListFolders(ctx); len(folders) != 0 {
				t.Errorf("no folder may be added, got %q", folders)
			}
		})
	}
}

// TestServiceInstallRestartsActiveUnit: when the live daemon IS the unit,
// install proceeds and restarts it to pick up the folder list.
func TestServiceInstallRestartsActiveUnit(t *testing.T) {
	st := configuredStore(t)
	home := t.TempDir()
	_ = os.Mkdir(filepath.Join(home, "Documents"), 0o755)
	stateDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(stateDir, "daemon.pid"), []byte(strconv.Itoa(os.Getpid())), 0o600)
	mgr := &fakeServiceManager{active: true}
	if err := ServiceInstall(context.Background(), st, mgr, "", home, stateDir, false, false, &bytes.Buffer{}); err != nil {
		t.Fatalf("ServiceInstall: %v", err)
	}
	if !mgr.installed {
		t.Error("Install not called")
	}
}

func TestServiceInstallPropagatesInstallError(t *testing.T) {
	st := configuredStore(t)
	home := t.TempDir()
	_ = os.Mkdir(filepath.Join(home, "Documents"), 0o755)
	mgr := &fakeServiceManager{installErr: apperr.ErrService}
	err := ServiceInstall(context.Background(), st, mgr, "", home, t.TempDir(), false, false, &bytes.Buffer{})
	if !errors.Is(err, apperr.ErrService) {
		t.Fatalf("err = %v", err)
	}
}

func TestResolveFolder(t *testing.T) {
	home := t.TempDir()
	cwd, _ := os.Getwd()
	cases := map[string]string{
		"":                filepath.Join(home, "Documents"),
		"~":               home,
		"~/Work/":         filepath.Join(home, "Work"),
		home + "/a/../b/": filepath.Join(home, "b"),
		"rel":             filepath.Join(cwd, "rel"),
	}
	for in, want := range cases {
		got, err := resolveFolder(in, home)
		if err != nil || got != want {
			t.Errorf("resolveFolder(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestServiceUninstallAndStatusDelegate(t *testing.T) {
	mgr := &fakeServiceManager{}
	if err := ServiceUninstall(context.Background(), mgr, &bytes.Buffer{}); err != nil {
		t.Error(err)
	}
	if err := ServiceStatus(context.Background(), mgr, &bytes.Buffer{}); err != nil {
		t.Error(err)
	}
}

// TestServiceInstallRelativeStoredFolders: a relative watch entry resolves
// against $HOME under systemd. With --delete that risks deleting remote
// backups, so install refuses; without it, install proceeds with a warning.
func TestServiceInstallRelativeStoredFolders(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	_ = os.Mkdir(filepath.Join(home, "Documents"), 0o755)

	st := configuredStore(t)
	if err := st.AddFolder(ctx, "projects"); err != nil {
		t.Fatal(err)
	}
	mgr := &fakeServiceManager{}
	err := ServiceInstall(ctx, st, mgr, "", home, t.TempDir(), true, false, &bytes.Buffer{})
	if !errors.Is(err, apperr.ErrService) || !strings.Contains(err.Error(), "projects") {
		t.Fatalf("--delete with relative folder: want ErrService naming it, got %v", err)
	}
	if mgr.installed {
		t.Error("Install must not be called")
	}
	if folders, _ := st.ListFolders(ctx); len(folders) != 1 {
		t.Errorf("watch list must be unchanged, got %q", folders)
	}

	var out bytes.Buffer
	if err := ServiceInstall(ctx, st, mgr, "", home, t.TempDir(), false, false, &out); err != nil {
		t.Fatalf("without --delete: %v", err)
	}
	if !mgr.installed || !strings.Contains(out.String(), "Warning") {
		t.Errorf("want install + warning, got installed=%v out=%q", mgr.installed, out.String())
	}
}
