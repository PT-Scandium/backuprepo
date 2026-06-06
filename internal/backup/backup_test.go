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
