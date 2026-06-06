package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"backuprepo/internal/apperr"
)

func key32() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i * 7)
	}
	return k
}

func openTest(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(context.Background(), path, key32())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st, path
}

func TestGetConfigNotConfigured(t *testing.T) {
	st, _ := openTest(t)
	if _, err := st.GetConfig(context.Background()); !errors.Is(err, apperr.ErrNotConfigured) {
		t.Fatalf("want ErrNotConfigured, got %v", err)
	}
}

func TestSaveAndGetConfigRoundTrip(t *testing.T) {
	st, _ := openTest(t)
	ctx := context.Background()
	in := S3Config{
		Endpoint: "https://s3.us-west-004.backblazeb2.com",
		Region:   "us-west-004",
		Bucket:   "my-bucket",
		KeyID:    "0001abcd",
		AppKey:   "K001-supersecret",
	}
	if err := st.SaveConfig(ctx, in); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	got, err := st.GetConfig(ctx)
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	if got != in {
		t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", got, in)
	}
}

func TestConfigEncryptedAtRest(t *testing.T) {
	st, path := openTest(t)
	ctx := context.Background()
	in := S3Config{Bucket: "b", KeyID: "PLAINTEXT_KEY_ID", AppKey: "PLAINTEXT_SECRET"}
	if err := st.SaveConfig(ctx, in); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	st.Close()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read db: %v", err)
	}
	if bytes.Contains(raw, []byte("PLAINTEXT_KEY_ID")) || bytes.Contains(raw, []byte("PLAINTEXT_SECRET")) {
		t.Fatal("credentials stored in plaintext on disk")
	}
}

func TestFolderAddListRemove(t *testing.T) {
	st, _ := openTest(t)
	ctx := context.Background()
	if err := st.AddFolder(ctx, "/data/a"); err != nil {
		t.Fatalf("AddFolder: %v", err)
	}
	if err := st.AddFolder(ctx, "/data/a"); err != nil {
		t.Fatalf("re-add should be idempotent: %v", err)
	}
	folders, err := st.ListFolders(ctx)
	if err != nil || len(folders) != 1 || folders[0] != "/data/a" {
		t.Fatalf("ListFolders = %v, %v", folders, err)
	}
	if err := st.RemoveFolder(ctx, "/data/a"); err != nil {
		t.Fatalf("RemoveFolder: %v", err)
	}
	if err := st.RemoveFolder(ctx, "/data/a"); !errors.Is(err, apperr.ErrFolderNotWatched) {
		t.Fatalf("want ErrFolderNotWatched, got %v", err)
	}
}

func TestFileUpsertAndGet(t *testing.T) {
	st, _ := openTest(t)
	ctx := context.Background()
	if got, err := st.GetFile(ctx, "/x"); err != nil || got != nil {
		t.Fatalf("missing file: got %v, %v", got, err)
	}
	bk := int64(123)
	rec := FileRecord{Path: "/x", Size: 10, ModTime: 99, SHA256: "abc", LastBackup: &bk, RemoteKey: "x"}
	if err := st.UpsertFile(ctx, rec); err != nil {
		t.Fatalf("UpsertFile: %v", err)
	}
	got, err := st.GetFile(ctx, "/x")
	if err != nil || got == nil {
		t.Fatalf("GetFile: %v, %v", got, err)
	}
	if got.Size != 10 || got.SHA256 != "abc" || got.LastBackup == nil || *got.LastBackup != 123 {
		t.Fatalf("record mismatch: %+v", got)
	}
}
