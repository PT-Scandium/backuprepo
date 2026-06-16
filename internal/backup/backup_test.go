package backup

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"backuprepo/internal/b2"
	"backuprepo/internal/store"
)

func key32() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i)
	}
	return k
}

func newSvc(t *testing.T) (*Service, *store.Store, *b2.FakeBackend) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "b.db"), key32())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	fake := b2.NewFake()
	return New(st, fake), st, fake
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestUploadChangedUploadsNewFiles(t *testing.T) {
	svc, st, fake := newSvc(t)
	ctx := context.Background()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), "hello")
	if err := st.AddFolder(ctx, dir); err != nil {
		t.Fatal(err)
	}

	res, err := svc.UploadChanged(ctx)
	if err != nil {
		t.Fatalf("UploadChanged: %v", err)
	}
	if res.Uploaded != 1 || res.Skipped != 0 || res.Failed != 0 {
		t.Fatalf("got %+v, want 1 uploaded", res)
	}
	if len(fake.Objects) != 1 {
		t.Fatalf("expected 1 object, got %d", len(fake.Objects))
	}
}

func TestUploadChangedSkipsUnchanged(t *testing.T) {
	svc, st, _ := newSvc(t)
	ctx := context.Background()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), "hello")
	st.AddFolder(ctx, dir)

	if _, err := svc.UploadChanged(ctx); err != nil {
		t.Fatal(err)
	}
	res, err := svc.UploadChanged(ctx) // second run, nothing changed
	if err != nil {
		t.Fatal(err)
	}
	if res.Uploaded != 0 || res.Skipped != 1 {
		t.Fatalf("got %+v, want 0 uploaded / 1 skipped", res)
	}
}

func TestUploadChangedReuploadsOnContentChange(t *testing.T) {
	svc, st, _ := newSvc(t)
	ctx := context.Background()
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	writeFile(t, p, "hello")
	st.AddFolder(ctx, dir)
	if _, err := svc.UploadChanged(ctx); err != nil {
		t.Fatal(err)
	}

	// change content AND bump mtime so size/mtime cheap-check triggers a hash compare
	writeFile(t, p, "hello world larger")
	future := mustTime(t)
	os.Chtimes(p, future, future)

	res, err := svc.UploadChanged(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Uploaded != 1 {
		t.Fatalf("got %+v, want 1 re-uploaded", res)
	}
}

func TestPendingCount(t *testing.T) {
	svc, st, _ := newSvc(t)
	ctx := context.Background()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), "x")
	writeFile(t, filepath.Join(dir, "b.txt"), "y")
	st.AddFolder(ctx, dir)

	n, err := svc.PendingCount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("pending = %d, want 2", n)
	}
}

func mustTime(t *testing.T) time.Time {
	t.Helper()
	return time.Now().Add(2 * time.Second)
}

func TestDeletionPropagationDisabledByDefault(t *testing.T) {
	svc, st, fake := newSvc(t)
	ctx := context.Background()
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	writeFile(t, p, "hello")
	st.AddFolder(ctx, dir)
	if _, err := svc.UploadChanged(ctx); err != nil {
		t.Fatal(err)
	}

	// Delete locally, then run WITHOUT a deleter configured.
	os.Remove(p)
	res, err := svc.UploadChanged(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted != 0 {
		t.Fatalf("deletions should be off by default, got Deleted=%d", res.Deleted)
	}
	if len(fake.Objects) != 1 {
		t.Fatalf("remote object should remain, got %d objects", len(fake.Objects))
	}
}

func TestDeletionPropagationRemovesRemoteAndRecord(t *testing.T) {
	svc, st, fake := newSvc(t)
	svc = svc.WithDeleter(fake)
	ctx := context.Background()
	dir := t.TempDir()
	keep := filepath.Join(dir, "keep.txt")
	gone := filepath.Join(dir, "gone.txt")
	writeFile(t, keep, "keep")
	writeFile(t, gone, "gone")
	st.AddFolder(ctx, dir)
	if _, err := svc.UploadChanged(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fake.Objects) != 2 {
		t.Fatalf("expected 2 objects after upload, got %d", len(fake.Objects))
	}

	os.Remove(gone)
	res, err := svc.UploadChanged(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted != 1 {
		t.Fatalf("expected Deleted=1, got %+v", res)
	}
	if len(fake.Objects) != 1 {
		t.Fatalf("expected 1 remaining object, got %d", len(fake.Objects))
	}
	if _, ok := fake.Objects[RemoteKey(keep)]; !ok {
		t.Fatalf("kept file's object should remain")
	}
	// Local record for the deleted file must be gone too.
	rec, err := st.GetFile(ctx, gone)
	if err != nil {
		t.Fatal(err)
	}
	if rec != nil {
		t.Fatalf("expected file record removed, got %+v", rec)
	}
}

// TestDeletionPropagationSkipsMissingFolder is the safety guard: if a watched
// folder is entirely absent (e.g. an unmounted drive), the daemon must NOT
// interpret that as "every file was deleted" and purge the remote.
func TestDeletionPropagationSkipsMissingFolder(t *testing.T) {
	svc, st, fake := newSvc(t)
	svc = svc.WithDeleter(fake)
	ctx := context.Background()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), "x")
	writeFile(t, filepath.Join(dir, "b.txt"), "y")
	st.AddFolder(ctx, dir)
	if _, err := svc.UploadChanged(ctx); err != nil {
		t.Fatal(err)
	}

	// Simulate the whole watched folder vanishing (unmount), not individual files.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	res, err := svc.UploadChanged(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted != 0 {
		t.Fatalf("missing folder must not trigger deletions, got Deleted=%d", res.Deleted)
	}
	if len(fake.Objects) != 2 {
		t.Fatalf("remote objects must be preserved when folder is missing, got %d", len(fake.Objects))
	}
}
