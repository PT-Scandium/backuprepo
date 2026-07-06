package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"backuprepo/internal/apperr"
	"backuprepo/internal/b2"
	"backuprepo/internal/store"
)

// seedBackend returns a fake backend pre-populated with an object per key.
func seedBackend(ctx context.Context, keys ...string) *b2.FakeBackend {
	be := b2.NewFake()
	for _, k := range keys {
		be.Upload(ctx, k, bytes.NewReader([]byte("data-"+k)), int64(len("data-"+k)))
	}
	return be
}

// TestLsGroupsFolders verifies Ls lists files and groups keys into folder prefixes.
func TestLsGroupsFolders(t *testing.T) {
	ctx := context.Background()
	be := seedBackend(ctx, "a.txt", "photos/1.jpg", "photos/2.jpg")
	var out bytes.Buffer
	if err := Ls(ctx, be, "", false, &out); err != nil {
		t.Fatalf("Ls: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "photos/") || !strings.Contains(s, "a.txt") {
		t.Fatalf("ls output: %q", s)
	}
}

// TestFindSubstring verifies Find returns only keys containing the query.
func TestFindSubstring(t *testing.T) {
	ctx := context.Background()
	be := seedBackend(ctx, "report-2024.pdf", "photos/cat.jpg", "notes.txt")
	var out bytes.Buffer
	if err := Find(ctx, be, "cat", "", &out); err != nil {
		t.Fatalf("Find: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "photos/cat.jpg") || strings.Contains(s, "notes.txt") {
		t.Fatalf("find output: %q", s)
	}
}

// TestGetWritesFile verifies Get downloads an object to the given local path.
func TestGetWritesFile(t *testing.T) {
	ctx := context.Background()
	be := seedBackend(ctx, "dir/file.txt")
	dest := filepath.Join(t.TempDir(), "out.txt")
	var out bytes.Buffer
	if err := Get(ctx, be, "dir/file.txt", dest, false, &out); err != nil {
		t.Fatalf("Get: %v", err)
	}
	data, err := os.ReadFile(dest)
	if err != nil || string(data) != "data-dir/file.txt" {
		t.Fatalf("downloaded = %q, %v", data, err)
	}
}

// TestPutUploadsFile verifies Put uploads a local file under the given remote key.
func TestPutUploadsFile(t *testing.T) {
	ctx := context.Background()
	be := b2.NewFake()
	src := filepath.Join(t.TempDir(), "local.txt")
	os.WriteFile(src, []byte("hello"), 0o644)
	var out bytes.Buffer
	if err := Put(ctx, be, src, "remote/local.txt", false, false, &out); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if string(be.Objects["remote/local.txt"]) != "hello" {
		t.Fatalf("uploaded = %q", be.Objects["remote/local.txt"])
	}
}

// TestPutRecursiveContinuesPastFailure verifies that in recursive mode one failed
// file is reported and counted but the rest still upload, and Put returns an error.
func TestPutRecursiveContinuesPastFailure(t *testing.T) {
	ctx := context.Background()
	be := b2.NewFake()
	dir := t.TempDir()
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Make the middle file fail to upload.
	be.FailUpload = func(key string) error {
		if strings.HasSuffix(key, "b.txt") {
			return apperr.ErrUploadFailed
		}
		return nil
	}
	var out bytes.Buffer
	err := Put(ctx, be, dir, "dest", true, false, &out)
	if !errors.Is(err, apperr.ErrUploadFailed) {
		t.Fatalf("want ErrUploadFailed (1 file failed), got %v", err)
	}
	// The other two files uploaded despite b.txt failing.
	if string(be.Objects["dest/a.txt"]) != "a.txt" || string(be.Objects["dest/c.txt"]) != "c.txt" {
		t.Fatalf("a and c should have uploaded; objects=%v", be.Objects)
	}
	if _, ok := be.Objects["dest/b.txt"]; ok {
		t.Fatal("b.txt should not have been stored")
	}
	s := out.String()
	if !strings.Contains(s, "FAILED dest/b.txt") || !strings.Contains(s, "Uploaded: 2, Skipped: 0, Failed: 1") {
		t.Fatalf("summary/report missing: %q", s)
	}
}

// TestPutRecursiveSkipExisting verifies --skip-existing skips files already
// present remotely and uploads only the new ones.
func TestPutRecursiveSkipExisting(t *testing.T) {
	ctx := context.Background()
	be := b2.NewFake()
	be.Objects["dest/a.txt"] = []byte("old") // pretend a.txt is already uploaded
	dir := t.TempDir()
	for _, name := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("new-"+name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if err := Put(ctx, be, dir, "dest", true, true, &out); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if string(be.Objects["dest/a.txt"]) != "old" {
		t.Fatalf("a.txt should have been skipped (still %q), got %q", "old", be.Objects["dest/a.txt"])
	}
	if string(be.Objects["dest/b.txt"]) != "new-b.txt" {
		t.Fatalf("b.txt should have uploaded, got %q", be.Objects["dest/b.txt"])
	}
	if !strings.Contains(out.String(), "Uploaded: 1, Skipped: 1, Failed: 0") {
		t.Fatalf("summary missing: %q", out.String())
	}
}

// TestRmConfirmAndForce verifies Rm honors a declined prompt and deletes with force.
func TestRmConfirmAndForce(t *testing.T) {
	ctx := context.Background()
	be := seedBackend(ctx, "a.txt")
	// Declined confirmation: object stays.
	var out bytes.Buffer
	if err := Rm(ctx, be, "a.txt", false, false, strings.NewReader("n\n"), &out); err != nil {
		t.Fatalf("Rm(declined): %v", err)
	}
	if ok, _ := be.Exists(ctx, "a.txt"); !ok {
		t.Fatal("object should survive a declined delete")
	}
	// Force: object removed without prompt.
	if err := Rm(ctx, be, "a.txt", false, true, strings.NewReader(""), &out); err != nil {
		t.Fatalf("Rm(force): %v", err)
	}
	if ok, _ := be.Exists(ctx, "a.txt"); ok {
		t.Fatal("object should be deleted with --force")
	}
}

// TestRmRecursive verifies Rm -r deletes every object under a prefix.
func TestRmRecursive(t *testing.T) {
	ctx := context.Background()
	be := seedBackend(ctx, "p/1.txt", "p/2.txt", "q/3.txt")
	var out bytes.Buffer
	if err := Rm(ctx, be, "p/", true, true, strings.NewReader(""), &out); err != nil {
		t.Fatalf("Rm(-r): %v", err)
	}
	if len(be.Objects) != 1 {
		t.Fatalf("after recursive rm, remaining=%d want 1", len(be.Objects))
	}
}

// TestBackendShowAndSet verifies Backend sets and then reports the stored backend.
func TestBackendShowAndSet(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	// Configure first so SetBackend has a row.
	saveMinimalConfig(t, st)
	var out bytes.Buffer
	if err := Backend(ctx, st, "b2", &out); err != nil {
		t.Fatalf("Backend set: %v", err)
	}
	out.Reset()
	if err := Backend(ctx, st, "", &out); err != nil {
		t.Fatalf("Backend show: %v", err)
	}
	if !strings.Contains(out.String(), "b2") {
		t.Fatalf("backend show: %q", out.String())
	}
}

// saveMinimalConfig stores a minimal valid config so backend operations have a row.
func saveMinimalConfig(t *testing.T, st *store.Store) {
	t.Helper()
	err := st.SaveConfig(context.Background(), store.RemoteConfig{
		Bucket: "b", BucketID: "id", KeyID: "k", AppKey: "a",
	})
	if err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
}

// TestGetRecursivePreservesStructure verifies Get -r recreates the remote tree locally.
func TestGetRecursivePreservesStructure(t *testing.T) {
	ctx := context.Background()
	be := seedBackend(ctx, "docs/a.txt", "docs/sub/b.txt")
	tmpDir := t.TempDir()
	var out bytes.Buffer
	if err := Get(ctx, be, "docs/", tmpDir, true, &out); err != nil {
		t.Fatalf("Get recursive: %v", err)
	}
	cases := []struct {
		rel  string
		want string
	}{
		{"a.txt", "data-docs/a.txt"},
		{filepath.Join("sub", "b.txt"), "data-docs/sub/b.txt"},
	}
	for _, c := range cases {
		path := filepath.Join(tmpDir, c.rel)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", path, err)
		}
		if string(data) != c.want {
			t.Fatalf("file %s: got %q, want %q", c.rel, data, c.want)
		}
	}
}

// TestGetRejectsTraversal verifies Get rejects keys that would escape the destination dir.
func TestGetRejectsTraversal(t *testing.T) {
	ctx := context.Background()
	be := b2.NewFake()
	be.Objects["../escape.txt"] = []byte("x")
	tmpDir := t.TempDir()
	var out bytes.Buffer
	err := Get(ctx, be, "", tmpDir, true, &out)
	if err == nil {
		t.Fatal("expected error for path traversal, got nil")
	}
	if !errors.Is(err, apperr.ErrDownloadFailed) {
		t.Fatalf("expected ErrDownloadFailed, got: %v", err)
	}
}

// TestPutDirWithoutRecursiveErrors verifies Put errors on a directory without -r.
func TestPutDirWithoutRecursiveErrors(t *testing.T) {
	ctx := context.Background()
	be := b2.NewFake()
	tmpDir := t.TempDir()
	var out bytes.Buffer
	err := Put(ctx, be, tmpDir, "remote", false, false, &out)
	if err == nil {
		t.Fatal("expected error when putting directory without -r, got nil")
	}
}
