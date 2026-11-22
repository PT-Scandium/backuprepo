package syncer

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"backuprepo/internal/apperr"
	"backuprepo/internal/b2"
	"backuprepo/internal/backup"
	"backuprepo/internal/store"
)

// env is a synced folder, its store and bucket for one test.
type env struct {
	t        *testing.T
	ctx      context.Context
	st       *store.Store
	fake     *b2.FakeBackend
	root     string
	folder   store.Folder
	conflict string
	now      time.Time
}

// newEnv maps a fresh temp folder to bucket folder prefix ("/" = whole bucket).
func newEnv(t *testing.T, prefix string) *env {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "s.db"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	root := t.TempDir()
	if err := st.AddFolderWithPrefix(ctx, root, prefix); err != nil {
		t.Fatal(err)
	}
	return &env{t: t, ctx: ctx, st: st, fake: b2.NewFake(), root: root,
		folder: store.Folder{Path: root, RemotePrefix: prefix}, conflict: filepath.Join(t.TempDir(), "conflicts"),
		now: time.Date(2026, 11, 22, 9, 30, 0, 0, time.UTC)}
}

// sync runs one sync of the env's folder (dry when dry is set).
func (e *env) sync(dry bool) Report {
	e.t.Helper()
	folders, err := e.st.ListWatched(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	rep, err := Folder(e.ctx, e.st, e.fake, folders, e.folder, Options{DryRun: dry, ConflictDir: e.conflict, Now: func() time.Time { return e.now }})
	if err != nil {
		e.t.Fatalf("Folder: %v", err)
	}
	return rep
}

// putRemote uploads content to key as another client would, with source time t.
func (e *env) putRemote(key, content string, t time.Time) {
	e.t.Helper()
	if err := e.fake.UploadWithModTime(e.ctx, key, strings.NewReader(content), int64(len(content)), t); err != nil {
		e.t.Fatal(err)
	}
}

// object returns the listed metadata of key.
func (e *env) object(key string) b2.ObjectInfo {
	e.t.Helper()
	l, err := e.fake.List(e.ctx, key, true)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, o := range l.Objects {
		if o.Key == key {
			return o
		}
	}
	e.t.Fatalf("object %s not listed", key)
	return b2.ObjectInfo{}
}

// writeLocal writes rel under the root with modification time t.
func (e *env) writeLocal(rel, content string, t time.Time) string {
	e.t.Helper()
	p := filepath.Join(e.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
	if err := os.Chtimes(p, t, t); err != nil {
		e.t.Fatal(err)
	}
	return p
}

// readLocal returns rel's content ("" and false when missing).
func (e *env) readLocal(rel string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(e.root, filepath.FromSlash(rel)))
	return string(b), err == nil
}

// resetLog clears the fake's transfer logs.
func (e *env) resetLog() { e.fake.Uploads, e.fake.Downloads = nil, nil }

// actions maps rel → action for a report's items, failing on any item error.
func actions(t *testing.T, r Report) map[string]Action {
	t.Helper()
	out := map[string]Action{}
	for _, it := range r.Items {
		if it.Err != nil {
			t.Fatalf("item %s (%v) failed: %v", it.Rel, it.Action, it.Err)
		}
		out[it.Rel] = it.Action
	}
	return out
}

var t0 = time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)

// TestFirstSyncDownloadsRemote: an empty local folder receives the bucket
// folder (not its .bzEmpty marker, not other folders), with the remote times;
// a second run is a no-op.
func TestFirstSyncDownloadsRemote(t *testing.T) {
	e := newEnv(t, "P/")
	e.putRemote("P/a.txt", "alpha", t0)
	e.putRemote("P/sub/b.txt", "beta", t0.Add(time.Hour))
	e.putRemote("P/"+FolderMarker, "", time.Time{})
	e.putRemote("Q/other.txt", "not mine", t0)
	e.resetLog()

	rep := e.sync(false)
	if got := actions(t, rep); len(got) != 2 || got["a.txt"] != Download || got["sub/b.txt"] != Download {
		t.Fatalf("actions = %v", got)
	}
	if rep.Downloaded != 2 || rep.Uploaded != 0 {
		t.Errorf("report = %+v", rep)
	}
	if c, ok := e.readLocal("sub/b.txt"); !ok || c != "beta" {
		t.Errorf("sub/b.txt = %q, %v", c, ok)
	}
	if _, ok := e.readLocal(FolderMarker); ok {
		t.Error("the folder marker must not be downloaded")
	}
	info, _ := os.Stat(filepath.Join(e.root, "sub", "b.txt"))
	if !info.ModTime().Equal(t0.Add(time.Hour)) {
		t.Errorf("mtime = %v, want the remote source time", info.ModTime())
	}
	if rec, _ := e.st.GetFile(e.ctx, filepath.Join(e.root, "a.txt")); rec == nil || rec.RemoteKey != "P/a.txt" || rec.RemoteFileID == "" {
		t.Errorf("record = %+v", rec)
	}
	e.resetLog()
	if rep := e.sync(false); len(rep.Items) != 0 || len(e.fake.Uploads)+len(e.fake.Downloads) != 0 {
		t.Errorf("second run = %+v (up %v, down %v); want nothing", rep.Items, e.fake.Uploads, e.fake.Downloads)
	}
	if matches, _ := filepath.Glob(filepath.Join(e.root, "*", backup.SyncTempPrefix+"*")); len(matches) != 0 {
		t.Errorf("temp files left: %v", matches)
	}
}

// TestFirstSyncUploadsLocal: local files go up with their modification time;
// the next run learns the new versions' ids without transferring anything.
func TestFirstSyncUploadsLocal(t *testing.T) {
	e := newEnv(t, "P/")
	p := e.writeLocal("a.txt", "alpha", t0)
	e.writeLocal("d/b.txt", "beta", t0)
	rep := e.sync(false)
	if rep.Uploaded != 2 || len(e.fake.Uploads) != 2 {
		t.Fatalf("report = %+v, uploads %v", rep, e.fake.Uploads)
	}
	if m := e.fake.Meta["P/a.txt"]; !m.SrcModified.Equal(t0) {
		t.Errorf("uploaded source time = %v, want %v", m.SrcModified, t0)
	}
	e.resetLog()
	if rep := e.sync(false); len(rep.Items) != 0 || len(e.fake.Uploads)+len(e.fake.Downloads) != 0 {
		t.Fatalf("second run = %+v; want nothing", rep.Items)
	}
	if rec, _ := e.st.GetFile(e.ctx, p); rec == nil || rec.RemoteFileID != e.fake.Meta["P/a.txt"].FileID {
		t.Errorf("record = %+v, want the uploaded version's id learned", rec)
	}
}

// TestSameBytesOnBothSidesOnlyRecords: files already identical on both sides
// (e.g. copied by hand) are recorded without any transfer.
func TestSameBytesOnBothSidesOnlyRecords(t *testing.T) {
	e := newEnv(t, "P/")
	e.writeLocal("a.txt", "same", t0)
	e.putRemote("P/a.txt", "same", t0.Add(time.Hour))
	e.resetLog()
	rep := e.sync(false)
	if got := actions(t, rep); got["a.txt"] != Record || rep.Recorded != 1 {
		t.Fatalf("actions = %v, report %+v", got, rep)
	}
	if len(e.fake.Uploads)+len(e.fake.Downloads) != 0 {
		t.Errorf("transfers: up %v down %v", e.fake.Uploads, e.fake.Downloads)
	}
	if rep := e.sync(false); len(rep.Items) != 0 {
		t.Errorf("second run = %+v; want nothing", rep.Items)
	}
}

// TestOneSidedEditsCopyOver: after a first sync, a remote edit downloads and a
// local edit uploads.
func TestOneSidedEditsCopyOver(t *testing.T) {
	e := newEnv(t, "P/")
	e.putRemote("P/r.txt", "r1", t0)
	e.writeLocal("l.txt", "l1", t0)
	e.sync(false)

	e.putRemote("P/r.txt", "r2-remote-edit", t0.Add(2*time.Hour))
	e.writeLocal("l.txt", "l2-local-edit", t0.Add(2*time.Hour))
	e.resetLog()
	got := actions(t, e.sync(false))
	if got["r.txt"] != Download || got["l.txt"] != Upload || len(got) != 2 {
		t.Fatalf("actions = %v", got)
	}
	if c, _ := e.readLocal("r.txt"); c != "r2-remote-edit" {
		t.Errorf("r.txt = %q", c)
	}
	if string(e.fake.Objects["P/l.txt"]) != "l2-local-edit" {
		t.Errorf("remote l.txt = %q", e.fake.Objects["P/l.txt"])
	}
}

// TestConflictNewestWins: both sides edited; the newer version wins and a
// losing local edit is moved to the conflicts folder, never lost.
func TestConflictNewestWins(t *testing.T) {
	t.Run("remote newer", func(t *testing.T) {
		e := newEnv(t, "P/")
		e.writeLocal("a.txt", "base", t0)
		e.sync(false)
		e.writeLocal("a.txt", "local edit", t0.Add(time.Hour))
		e.putRemote("P/a.txt", "remote edit", t0.Add(2*time.Hour))
		rep := e.sync(false)
		if got := actions(t, rep); got["a.txt"] != KeepRemote || rep.Conflicts != 1 {
			t.Fatalf("actions = %v, report %+v", got, rep)
		}
		if c, _ := e.readLocal("a.txt"); c != "remote edit" {
			t.Errorf("local a.txt = %q, want the remote edit", c)
		}
		aside := filepath.Join(e.conflict, e.now.Format("20060102-150405"), filepath.Base(e.root), "a.txt")
		if b, err := os.ReadFile(aside); err != nil || string(b) != "local edit" {
			t.Errorf("kept-aside local = %q, %v", b, err)
		}
	})
	t.Run("local newer", func(t *testing.T) {
		e := newEnv(t, "P/")
		e.writeLocal("a.txt", "base", t0)
		e.sync(false)
		e.putRemote("P/a.txt", "remote edit", t0.Add(time.Hour))
		e.writeLocal("a.txt", "local edit", t0.Add(2*time.Hour))
		rep := e.sync(false)
		if got := actions(t, rep); got["a.txt"] != KeepLocal || rep.Conflicts != 1 {
			t.Fatalf("actions = %v, report %+v", got, rep)
		}
		if string(e.fake.Objects["P/a.txt"]) != "local edit" {
			t.Errorf("remote = %q, want the local edit", e.fake.Objects["P/a.txt"])
		}
	})
}

// TestNeverDeletes: a file deleted on one side is restored from the other; one
// gone on both sides just loses its snapshot.
func TestNeverDeletes(t *testing.T) {
	e := newEnv(t, "P/")
	e.writeLocal("keep-remote.txt", "1", t0)
	e.writeLocal("keep-local.txt", "2", t0)
	gone := e.writeLocal("gone.txt", "3", t0)
	e.sync(false)

	if err := os.Remove(filepath.Join(e.root, "keep-remote.txt")); err != nil {
		t.Fatal(err)
	}
	if err := e.fake.Delete(e.ctx, "P/keep-local.txt"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	if err := e.fake.Delete(e.ctx, "P/gone.txt"); err != nil {
		t.Fatal(err)
	}
	got := actions(t, e.sync(false))
	if got["keep-remote.txt"] != Download || got["keep-local.txt"] != Upload || got["gone.txt"] != Forget {
		t.Fatalf("actions = %v", got)
	}
	if c, ok := e.readLocal("keep-remote.txt"); !ok || c != "1" {
		t.Errorf("restored local = %q, %v", c, ok)
	}
	if string(e.fake.Objects["P/keep-local.txt"]) != "2" {
		t.Errorf("restored remote = %q", e.fake.Objects["P/keep-local.txt"])
	}
	if rec, _ := e.st.GetFile(e.ctx, gone); rec != nil {
		t.Errorf("snapshot of a file gone on both sides kept: %+v", rec)
	}
}

// TestDryRunChangesNothing: a preview reports the plan but transfers, writes
// and records nothing.
func TestDryRunChangesNothing(t *testing.T) {
	e := newEnv(t, "P/")
	e.putRemote("P/a.txt", "alpha", t0)
	e.writeLocal("b.txt", "beta", t0)
	e.resetLog()
	got := actions(t, e.sync(true))
	if got["a.txt"] != Download || got["b.txt"] != Upload {
		t.Fatalf("plan = %v", got)
	}
	if _, ok := e.readLocal("a.txt"); ok || len(e.fake.Uploads)+len(e.fake.Downloads) != 0 {
		t.Error("dry run transferred or wrote files")
	}
	if files, _ := e.st.ListFiles(e.ctx); len(files) != 0 {
		t.Errorf("dry run recorded %d snapshots", len(files))
	}
}

// TestMissingRootIsNeverRecreated: a mapped folder that is missing (e.g. an
// unmounted drive) is not synced, and not re-created and refilled.
func TestMissingRootIsNeverRecreated(t *testing.T) {
	e := newEnv(t, "P/")
	e.putRemote("P/a.txt", "alpha", t0)
	if err := os.Remove(e.root); err != nil {
		t.Fatal(err)
	}
	folders, _ := e.st.ListWatched(e.ctx)
	_, err := Folder(e.ctx, e.st, e.fake, folders, e.folder, Options{ConflictDir: e.conflict})
	if !errors.Is(err, apperr.ErrSync) {
		t.Fatalf("Folder = %v, want ErrSync", err)
	}
	if _, err := os.Stat(e.root); !os.IsNotExist(err) {
		t.Errorf("missing root was re-created (stat err %v)", err)
	}
	if _, err := Folder(e.ctx, e.st, e.fake, folders, e.folder, Options{}); !errors.Is(err, apperr.ErrSync) {
		t.Errorf("Folder without a conflicts folder = %v, want ErrSync", err)
	}
}

// TestWholeBucketExcludesOtherFolders: a whole-bucket mirror leaves alone the
// objects of a one-way folder backed up to the same bucket, and skips a
// watched folder nested inside it.
func TestWholeBucketExcludesOtherFolders(t *testing.T) {
	e := newEnv(t, store.RootPrefix)
	docs := t.TempDir()
	if err := e.st.AddFolder(e.ctx, docs); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(e.root, "nested")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := e.st.AddFolder(e.ctx, nested); err != nil {
		t.Fatal(err)
	}
	e.writeLocal("nested/private.txt", "n", t0)
	e.putRemote("SEWA/a.pdf", "a", t0)
	e.putRemote(backup.RemoteKey(filepath.Join(docs, "notes.md")), "obsidian", t0)
	e.putRemote(backup.RemoteKey(filepath.Join(nested, "private.txt")), "n", t0)
	e.resetLog()

	got := actions(t, e.sync(false))
	if len(got) != 1 || got["SEWA/a.pdf"] != Download {
		t.Fatalf("actions = %v; want only SEWA/a.pdf", got)
	}
	if len(e.fake.Uploads) != 0 {
		t.Errorf("uploaded %v; the nested folder belongs to its own watch", e.fake.Uploads)
	}
}

// TestAllSyncsOnlyMappedFolders: All syncs mapped folders, leaves one-way
// folders alone, and reports a missing mapped folder without stopping.
func TestAllSyncsOnlyMappedFolders(t *testing.T) {
	e := newEnv(t, "P/")
	e.putRemote("P/a.txt", "a", t0)
	plain := t.TempDir()
	if err := os.WriteFile(filepath.Join(plain, "x.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.st.AddFolder(e.ctx, plain); err != nil {
		t.Fatal(err)
	}
	if err := e.st.AddFolderWithPrefix(e.ctx, filepath.Join(t.TempDir(), "missing"), "M/"); err != nil {
		t.Fatal(err)
	}
	e.resetLog()
	reports, err := All(e.ctx, e.st, e.fake, Options{ConflictDir: e.conflict})
	if !errors.Is(err, apperr.ErrSync) || len(reports) != 2 {
		t.Fatalf("All = %d reports, %v; want 2 reports and the missing folder's error", len(reports), err)
	}
	if len(e.fake.Downloads) != 1 || len(e.fake.Uploads) != 0 {
		t.Errorf("transfers: up %v down %v; want only P/a.txt down", e.fake.Uploads, e.fake.Downloads)
	}
}

// TestUnsafeRemoteNameIsSkipped: an object whose name would climb out of the
// synced folder is reported as failed and never written.
func TestUnsafeRemoteNameIsSkipped(t *testing.T) {
	e := newEnv(t, "P/")
	e.fake.Objects["P/../escape.txt"] = []byte("x")
	rep := e.sync(false)
	if rep.Failed != 1 {
		t.Fatalf("report = %+v, want 1 failed item", rep)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(e.root), "escape.txt")); !os.IsNotExist(err) {
		t.Error("escaped file was written")
	}
}

// TestDownloadAbortsWhenLocalChangedMeanwhile: download refuses to replace a
// local file that changed (or appeared) after the decision, leaving it intact.
func TestDownloadAbortsWhenLocalChangedMeanwhile(t *testing.T) {
	e := newEnv(t, "P/")
	p := e.writeLocal("a.txt", "v1", t0)
	seen, _ := os.Stat(p)
	e.writeLocal("a.txt", "edited meanwhile", t0.Add(time.Minute))
	e.putRemote("P/a.txt", "remote", t0)
	rf := remoteFile{key: "P/a.txt", obj: e.object("P/a.txt")}
	if err := download(e.ctx, e.st, e.fake, p, rf, seen, e.now); !errors.Is(err, apperr.ErrSync) {
		t.Fatalf("download = %v, want ErrSync", err)
	}
	if c, _ := e.readLocal("a.txt"); c != "edited meanwhile" {
		t.Errorf("local = %q, want the concurrent edit kept", c)
	}
	appeared := e.writeLocal("appeared.txt", "x", t0)
	if err := download(e.ctx, e.st, e.fake, appeared, rf, nil, e.now); !errors.Is(err, apperr.ErrSync) {
		t.Errorf("download over a file that appeared = %v, want ErrSync", err)
	}
	if err := download(e.ctx, e.st, e.fake, filepath.Join(e.root, "new.txt"), rf, nil, e.now); err != nil {
		t.Errorf("download to a new path: %v", err)
	}
	bad := rf
	bad.obj.SHA1 = strings.Repeat("0", 40)
	if err := download(e.ctx, e.st, e.fake, filepath.Join(e.root, "corrupt.txt"), bad, nil, e.now); !errors.Is(err, apperr.ErrSync) {
		t.Errorf("download with a mismatching SHA-1 = %v, want ErrSync", err)
	}
	if _, ok := e.readLocal("corrupt.txt"); ok {
		t.Error("a download that failed its SHA-1 check was kept")
	}
}

// TestSafeRel covers the remote-name checks.
func TestSafeRel(t *testing.T) {
	for rel, want := range map[string]bool{
		"a.txt": true, "dir/b.txt": true, "Kontrak 2025/c.pdf": true,
		"../x": false, "a/../../x": false, "/abs": false, "a//b": false, "a/./b": false, `a\b`: false, "..": false,
	} {
		if got := safeRel(rel); got != want {
			t.Errorf("safeRel(%q) = %v, want %v", rel, got, want)
		}
	}
}

// TestSetAside moves a file into a new nested folder.
func TestSetAside(t *testing.T) {
	src := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(src, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "x", "y", "a.txt")
	if err := setAside(src, dest); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dest); !bytes.Equal(b, []byte("keep me")) {
		t.Errorf("dest = %q", b)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("source still present")
	}
}
