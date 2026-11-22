package cli

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
	"backuprepo/internal/store"
	"backuprepo/internal/syncer"
)

// TestMapToCreatesMissingBucketFolder: mapping a folder to a bucket folder that
// doesn't exist yet creates it (a zero-byte .bzEmpty); an existing one is left.
func TestMapToCreatesMissingBucketFolder(t *testing.T) {
	ctx := context.Background()
	st, fake := newStore(t), b2.NewFake()
	dir := t.TempDir()
	var out bytes.Buffer
	if err := MapTo(ctx, st, fake, "b2", t.TempDir(), dir, "SEWA", &out); err != nil {
		t.Fatalf("MapTo: %v", err)
	}
	if _, ok := fake.Objects["SEWA/.bzEmpty"]; !ok || !strings.Contains(out.String(), "Created bucket folder SEWA/") {
		t.Errorf("marker missing; output %q", out.String())
	}
	if got, _ := st.ListWatched(ctx); len(got) != 1 || got[0].RemotePrefix != "SEWA/" {
		t.Errorf("ListWatched = %+v", got)
	}
	st2, fake2 := newStore(t), b2.NewFake()
	fake2.Objects["SEWA/a.pdf"] = []byte("a")
	if err := MapTo(ctx, st2, fake2, "b2", t.TempDir(), t.TempDir(), "SEWA", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := fake2.Objects["SEWA/.bzEmpty"]; ok {
		t.Error("an existing bucket folder got a marker")
	}
}

// TestMapToRefusedNextToWholeBucketMirror: with the whole bucket mirrored in
// one folder, mapping another folder would give two local copies.
func TestMapToRefusedNextToWholeBucketMirror(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	if err := st.AddFolderWithPrefix(ctx, t.TempDir(), store.RootPrefix); err != nil {
		t.Fatal(err)
	}
	err := MapTo(ctx, st, b2.NewFake(), "b2", t.TempDir(), t.TempDir(), "SEWA", &bytes.Buffer{})
	if !errors.Is(err, apperr.ErrInvalidPrefix) {
		t.Fatalf("MapTo = %v, want ErrInvalidPrefix", err)
	}
	if got, _ := st.ListWatched(ctx); len(got) != 1 {
		t.Errorf("watch list changed: %+v", got)
	}
}

// TestUnmapAndMaps: maps lists mapped folders only; unmap removes a mapping
// and refuses one-way folders and unknown paths.
func TestUnmapAndMaps(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	mapped, root, plain := t.TempDir(), t.TempDir(), t.TempDir()
	if err := st.AddFolderWithPrefix(ctx, mapped, "SEWA/"); err != nil {
		t.Fatal(err)
	}
	if err := st.AddFolderWithPrefix(ctx, root, store.RootPrefix); err != nil {
		t.Fatal(err)
	}
	if err := st.AddFolder(ctx, plain); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Maps(ctx, st, &out); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, mapped+" <-> SEWA/") || !strings.Contains(s, root+" <-> (whole bucket)") || strings.Contains(s, plain) {
		t.Errorf("Maps = %q", s)
	}
	if err := Unmap(ctx, st, plain, &bytes.Buffer{}); !errors.Is(err, apperr.ErrFolderNotWatched) {
		t.Errorf("Unmap(one-way) = %v", err)
	}
	if err := Unmap(ctx, st, t.TempDir(), &bytes.Buffer{}); !errors.Is(err, apperr.ErrFolderNotWatched) {
		t.Errorf("Unmap(unknown) = %v", err)
	}
	if err := Unmap(ctx, st, mapped, &bytes.Buffer{}); err != nil {
		t.Fatalf("Unmap: %v", err)
	}
	if got, _ := st.ListWatched(ctx); len(got) != 2 {
		t.Errorf("after unmap: %+v", got)
	}
	st2 := newStore(t)
	out.Reset()
	if err := Maps(ctx, st2, &out); err != nil || !strings.Contains(out.String(), "No folders are mapped") {
		t.Errorf("Maps(empty) = %q, %v", out.String(), err)
	}
}

// TestSyncRefusals: sync needs the b2 backend and a stopped daemon (except
// for a dry run), and refuses mappings that would sync the same objects twice.
func TestSyncRefusals(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	dir := t.TempDir()
	base := SyncOptions{Mode: ModeFolder, Folder: "SEWA", Dir: dir, Backend: "b2", StateDir: t.TempDir()}

	s3 := base
	s3.Backend = "s3"
	if err := Sync(ctx, st, b2.NewFake(), s3, &bytes.Buffer{}); !errors.Is(err, apperr.ErrSync) {
		t.Errorf("s3 backend: %v", err)
	}
	running := base
	running.DaemonRunning = true
	if err := Sync(ctx, st, b2.NewFake(), running, &bytes.Buffer{}); !errors.Is(err, apperr.ErrSync) {
		t.Errorf("daemon running: %v", err)
	}
	running.DryRun = true
	if err := Sync(ctx, st, b2.NewFake(), running, &bytes.Buffer{}); err != nil {
		t.Errorf("dry run while the daemon runs should be allowed: %v", err)
	}
	if err := st.AddFolderWithPrefix(ctx, dir, "OTHER/"); err != nil {
		t.Fatal(err)
	}
	if err := Sync(ctx, st, b2.NewFake(), base, &bytes.Buffer{}); !errors.Is(err, apperr.ErrInvalidPrefix) {
		t.Errorf("folder already mapped elsewhere: %v", err)
	}
	all := base
	all.Mode, all.Dir = ModeWholeBucket, t.TempDir()
	if err := Sync(ctx, st, b2.NewFake(), all, &bytes.Buffer{}); !errors.Is(err, apperr.ErrInvalidPrefix) {
		t.Errorf("whole-bucket mirror next to a mapped folder: %v", err)
	}
	nested := base
	nested.Folder, nested.Dir = "OTHER/sub", t.TempDir()
	if err := Sync(ctx, st, b2.NewFake(), nested, &bytes.Buffer{}); !errors.Is(err, apperr.ErrInvalidPrefix) {
		t.Errorf("bucket folder inside another mapping's: %v", err)
	}
}

// TestSyncFolderDryRunThenReal: a dry run previews without mapping or changing
// anything; the real run maps the folder and downloads; plain `bb sync` then
// syncs it again (nothing to do).
func TestSyncFolderDryRunThenReal(t *testing.T) {
	ctx := context.Background()
	st, fake := newStore(t), b2.NewFake()
	if err := fake.UploadWithModTime(ctx, "SEWA/a.pdf", strings.NewReader("pdf"), 3, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	opt := SyncOptions{Mode: ModeFolder, Folder: "SEWA", Dir: dir, Backend: "b2", StateDir: t.TempDir(), DryRun: true}
	var out bytes.Buffer
	if err := Sync(ctx, st, fake, opt, &out); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !strings.Contains(out.String(), "dry run") || !strings.Contains(out.String(), "download: 1") {
		t.Errorf("dry-run output = %q", out.String())
	}
	if got, _ := st.ListWatched(ctx); len(got) != 0 {
		t.Errorf("dry run mapped the folder: %+v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.pdf")); !os.IsNotExist(err) {
		t.Error("dry run downloaded")
	}

	opt.DryRun = false
	out.Reset()
	if err := Sync(ctx, st, fake, opt, &out); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if !strings.Contains(out.String(), "Mapped "+dir+" <-> SEWA/") || !strings.Contains(out.String(), "Downloaded: 1,") {
		t.Errorf("output = %q", out.String())
	}
	if b, err := os.ReadFile(filepath.Join(dir, "a.pdf")); err != nil || string(b) != "pdf" {
		t.Errorf("a.pdf = %q, %v", b, err)
	}
	out.Reset()
	if err := Sync(ctx, st, fake, SyncOptions{Mode: ModeMapped, Backend: "b2", StateDir: opt.StateDir}, &out); err != nil {
		t.Fatalf("sync mapped: %v", err)
	}
	if !strings.Contains(out.String(), "Downloaded: 0, Uploaded: 0") {
		t.Errorf("second sync output = %q", out.String())
	}
}

// TestSyncWholeBucket maps the folder to the bucket root and mirrors it.
func TestSyncWholeBucket(t *testing.T) {
	ctx := context.Background()
	st, fake := newStore(t), b2.NewFake()
	fake.Objects["2025-SEWA/a.pdf"] = []byte("a")
	fake.Objects["MANDALATAMA-SEWA/b.pdf"] = []byte("b")
	dir := t.TempDir()
	opt := SyncOptions{Mode: ModeWholeBucket, Dir: dir, Backend: "b2", StateDir: t.TempDir()}
	var out bytes.Buffer
	if err := Sync(ctx, st, fake, opt, &out); err != nil {
		t.Fatalf("sync --all: %v", err)
	}
	if !strings.Contains(out.String(), "(whole bucket)") || !strings.Contains(out.String(), "Downloaded: 2,") {
		t.Errorf("output = %q", out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "MANDALATAMA-SEWA", "b.pdf")); err != nil {
		t.Error(err)
	}
}

// TestSyncMappedStep: the step run after `bb upload`/daemon scans does nothing
// without mapped folders, only warns on the S3 backend, and syncs otherwise.
func TestSyncMappedStep(t *testing.T) {
	ctx := context.Background()
	st, fake := newStore(t), b2.NewFake()
	var out bytes.Buffer
	if err := SyncMapped(ctx, st, fake, "b2", t.TempDir(), &out); err != nil || out.Len() != 0 {
		t.Fatalf("no mapped folders: %q, %v", out.String(), err)
	}
	dir := t.TempDir()
	if err := st.AddFolderWithPrefix(ctx, dir, "P/"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SyncMapped(ctx, st, fake, "s3", t.TempDir(), &out); err != nil || !strings.Contains(out.String(), "warning: mapped folders are not synced") {
		t.Errorf("s3: %q, %v", out.String(), err)
	}
	out.Reset()
	if err := SyncMapped(ctx, st, fake, "b2", t.TempDir(), &out); err != nil || !strings.Contains(out.String(), "Uploaded: 1") {
		t.Errorf("b2: %q, %v", out.String(), err)
	}
	if _, ok := fake.Objects["P/a.txt"]; !ok {
		t.Error("P/a.txt not uploaded")
	}
}

// TestMapToRefusals: mapto needs the b2 backend, never remaps an already
// mapped folder (that would re-upload everything without a preview), and
// never maps a folder containing bb's own state folder.
func TestMapToRefusals(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	dir := t.TempDir()
	if err := MapTo(ctx, st, b2.NewFake(), "s3", t.TempDir(), dir, "A", &bytes.Buffer{}); !errors.Is(err, apperr.ErrSync) {
		t.Errorf("s3 backend: %v", err)
	}
	if err := MapTo(ctx, st, b2.NewFake(), "b2", t.TempDir(), dir, "A", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := MapTo(ctx, st, b2.NewFake(), "b2", t.TempDir(), dir, "B", &bytes.Buffer{}); !errors.Is(err, apperr.ErrInvalidPrefix) {
		t.Errorf("remapping a mapped folder: %v", err)
	}
	home := t.TempDir()
	state := filepath.Join(home, "backup_repo")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := MapTo(ctx, st, b2.NewFake(), "b2", state, home, "HOME", &bytes.Buffer{}); !errors.Is(err, apperr.ErrInvalidPrefix) {
		t.Errorf("folder containing the state folder: %v", err)
	}
	if got, _ := st.ListWatched(ctx); len(got) != 1 || got[0].RemotePrefix != "A/" {
		t.Errorf("watch list = %+v", got)
	}
}

// TestSyncHonoursLock: while another process holds the sync lock, `bb sync`
// fails and the after-upload sync step skips quietly.
func TestSyncHonoursLock(t *testing.T) {
	ctx := context.Background()
	st, fake := newStore(t), b2.NewFake()
	state := t.TempDir()
	dir := t.TempDir()
	if err := st.AddFolderWithPrefix(ctx, dir, "P/"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	unlock, err := syncer.Lock(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := Sync(ctx, st, fake, SyncOptions{Mode: ModeMapped, Backend: "b2", StateDir: state}, &bytes.Buffer{}); !errors.Is(err, syncer.ErrBusy) {
		t.Errorf("Sync while locked = %v, want ErrBusy", err)
	}
	var out bytes.Buffer
	if err := SyncMapped(ctx, st, fake, "b2", state, &out); err != nil || !strings.Contains(out.String(), "skipped") {
		t.Errorf("SyncMapped while locked = %q, %v", out.String(), err)
	}
	if len(fake.Uploads) != 0 {
		t.Errorf("uploaded while another process held the lock: %v", fake.Uploads)
	}
	unlock()
	if err := SyncMapped(ctx, st, fake, "b2", state, &bytes.Buffer{}); err != nil || len(fake.Uploads) != 1 {
		t.Errorf("after unlock: %v, uploads %v", err, fake.Uploads)
	}
}
