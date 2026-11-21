package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
	if err := Rm(ctx, be, "test-bucket", "a.txt", false, false, strings.NewReader("n\n"), &out); err != nil {
		t.Fatalf("Rm(declined): %v", err)
	}
	if ok, _ := be.Exists(ctx, "a.txt"); !ok {
		t.Fatal("object should survive a declined delete")
	}
	// Force: object removed without prompt.
	if err := Rm(ctx, be, "test-bucket", "a.txt", false, true, strings.NewReader(""), &out); err != nil {
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
	if err := Rm(ctx, be, "test-bucket", "p/", true, true, strings.NewReader(""), &out); err != nil {
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

// TestRmOutputSaysRemote guards the wording that makes clear rm acts on the
// bucket, not the local disk (a user mistook "deleted home/..." lines for local
// deletions): prompt, per-object lines and summary all name the bucket.
func TestRmOutputSaysRemote(t *testing.T) {
	ctx := context.Background()
	be := seedBackend(ctx, "home/u/Documents/a.txt", "home/u/Documents/b.txt")

	var out bytes.Buffer
	if err := Rm(ctx, be, "my-bucket", "home/", true, false, strings.NewReader("n\n"), &out); err != nil {
		t.Fatalf("Rm(declined): %v", err)
	}
	if !strings.Contains(out.String(), `from bucket "my-bucket" (local files are not affected)?`) {
		t.Errorf("confirm prompt must name the bucket and reassure about local files: %q", out.String())
	}
	if len(be.Objects) != 2 {
		t.Fatalf("declined rm deleted objects: %d left", len(be.Objects))
	}

	out.Reset()
	if err := Rm(ctx, be, "my-bucket", "home/", true, true, strings.NewReader(""), &out); err != nil {
		t.Fatalf("Rm(-r -f): %v", err)
	}
	for _, want := range []string{
		"deleted (remote) home/u/Documents/a.txt\n",
		"deleted (remote) home/u/Documents/b.txt\n",
		`Deleted 2 object(s) from bucket "my-bucket". Local files were not touched.`,
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}

	out.Reset()
	if err := Rm(ctx, be, "my-bucket", "home/", true, true, strings.NewReader(""), &out); err != nil {
		t.Fatalf("Rm(empty): %v", err)
	}
	if !strings.Contains(out.String(), `nothing to delete under "home/" in bucket "my-bucket"`) {
		t.Errorf("empty rm output = %q", out.String())
	}

	be = seedBackend(ctx, "x.txt")
	out.Reset()
	if err := Rm(ctx, be, "my-bucket", "x.txt", false, true, strings.NewReader(""), &out); err != nil {
		t.Fatalf("Rm(single): %v", err)
	}
	if !strings.Contains(out.String(), `deleted (remote) x.txt from bucket "my-bucket". Local files were not touched.`) {
		t.Errorf("single rm output = %q", out.String())
	}
}

// failSecondDelete fails the second Delete call, to exercise rm's partial-failure report.
type failSecondDelete struct {
	*b2.FakeBackend
	calls int
}

func (f *failSecondDelete) Delete(ctx context.Context, key string) error {
	f.calls++
	if f.calls == 2 {
		return apperr.ErrDeleteFailed
	}
	return f.FakeBackend.Delete(ctx, key)
}

func TestRmRecursiveReportsPartialFailure(t *testing.T) {
	ctx := context.Background()
	be := &failSecondDelete{FakeBackend: seedBackend(ctx, "p/1", "p/2", "p/3")}
	var out bytes.Buffer
	err := Rm(ctx, be, "my-bucket", "p", true, true, strings.NewReader(""), &out)
	if !errors.Is(err, apperr.ErrDeleteFailed) {
		t.Fatalf("want ErrDeleteFailed, got %v", err)
	}
	if !strings.Contains(err.Error(), `deleted 1 of 3 object(s) from bucket "my-bucket"`) {
		t.Errorf("error must say how far it got: %v", err)
	}
	if strings.Contains(out.String(), "Local files were not touched") {
		t.Errorf("no success summary on failure: %q", out.String())
	}
}

// TestBucketsFallsBackForScopedKey is the regression for `bb buckets` failing
// outright with a key restricted to one bucket: B2 denies the unfiltered list,
// so it now shows the configured bucket (looked up by name) and says why. Only
// an access denial falls back; other errors are returned as before.
func TestBucketsFallsBackForScopedKey(t *testing.T) {
	ctx := context.Background()
	cfg := b2.Config{KeyID: "k", AppKey: "s", BucketName: "my-bucket"}
	denied := fmt.Errorf("%w: list buckets: %w", apperr.ErrListFailed, apperr.ErrAccessDenied)
	listDenied := func(context.Context, b2.Config) ([]b2.BucketInfo, error) { return nil, denied }
	var findCalls int
	findOne := func(_ context.Context, c b2.Config) ([]b2.BucketInfo, error) {
		findCalls++
		if c.BucketName != "my-bucket" {
			t.Errorf("fallback looked up %q, want my-bucket", c.BucketName)
		}
		return []b2.BucketInfo{{Name: "my-bucket", ID: "bid", Type: "allPrivate"}}, nil
	}

	var out bytes.Buffer
	if err := Buckets(ctx, cfg, listDenied, findOne, &out); err != nil {
		t.Fatalf("Buckets: %v", err)
	}
	for _, s := range []string{"restricted to one", "my-bucket", "bid", "*"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("output missing %q:\n%s", s, out.String())
		}
	}

	// A non-denial error does not fall back.
	other := errors.New("network down")
	findCalls = 0
	err := Buckets(ctx, cfg, func(context.Context, b2.Config) ([]b2.BucketInfo, error) { return nil, other }, findOne, &bytes.Buffer{})
	if !errors.Is(err, other) || findCalls != 0 {
		t.Errorf("other error: err=%v findCalls=%d, want the error and no fallback", err, findCalls)
	}
	// A failed fallback reports the original denial.
	failFind := func(context.Context, b2.Config) ([]b2.BucketInfo, error) { return nil, errors.New("also denied") }
	if err := Buckets(ctx, cfg, listDenied, failFind, &bytes.Buffer{}); !errors.Is(err, apperr.ErrAccessDenied) {
		t.Errorf("failed fallback: err=%v, want the original access denial", err)
	}
	// No configured bucket: nothing to fall back to.
	if err := Buckets(ctx, b2.Config{KeyID: "k"}, listDenied, findOne, &bytes.Buffer{}); !errors.Is(err, apperr.ErrAccessDenied) {
		t.Errorf("no bucket name: err=%v, want the denial", err)
	}
}
