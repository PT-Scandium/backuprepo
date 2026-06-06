package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"backuprepo/internal/b2"
	"backuprepo/internal/store"
)

func seedBackend(ctx context.Context, keys ...string) *b2.FakeBackend {
	be := b2.NewFake()
	for _, k := range keys {
		be.Upload(ctx, k, bytes.NewReader([]byte("data-"+k)), int64(len("data-"+k)))
	}
	return be
}

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

func TestPutUploadsFile(t *testing.T) {
	ctx := context.Background()
	be := b2.NewFake()
	src := filepath.Join(t.TempDir(), "local.txt")
	os.WriteFile(src, []byte("hello"), 0o644)
	var out bytes.Buffer
	if err := Put(ctx, be, src, "remote/local.txt", false, &out); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if string(be.Objects["remote/local.txt"]) != "hello" {
		t.Fatalf("uploaded = %q", be.Objects["remote/local.txt"])
	}
}

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

func saveMinimalConfig(t *testing.T, st *store.Store) {
	t.Helper()
	err := st.SaveConfig(context.Background(), store.RemoteConfig{
		Bucket: "b", BucketID: "id", KeyID: "k", AppKey: "a",
	})
	if err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
}
