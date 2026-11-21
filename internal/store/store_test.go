package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"backuprepo/internal/apperr"
)

// key32 returns a deterministic 32-byte test key.
func key32() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i * 7)
	}
	return k
}

// openTest opens an encrypted store at a temp path and returns it with its path,
// registering cleanup to close it.
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

// TestGetConfigNotConfigured verifies GetConfig returns ErrNotConfigured on a fresh store.
func TestGetConfigNotConfigured(t *testing.T) {
	st, _ := openTest(t)
	if _, err := st.GetConfig(context.Background()); !errors.Is(err, apperr.ErrNotConfigured) {
		t.Fatalf("want ErrNotConfigured, got %v", err)
	}
}

// TestSaveAndGetConfigRoundTrip verifies a saved RemoteConfig is read back unchanged.
func TestSaveAndGetConfigRoundTrip(t *testing.T) {
	st, _ := openTest(t)
	ctx := context.Background()
	in := RemoteConfig{
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

// TestConfigEncryptedAtRest verifies credentials are not stored in plaintext on disk.
func TestConfigEncryptedAtRest(t *testing.T) {
	st, path := openTest(t)
	ctx := context.Background()
	in := RemoteConfig{Bucket: "b", KeyID: "PLAINTEXT_KEY_ID", AppKey: "PLAINTEXT_SECRET"}
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

// TestFolderAddListRemove verifies folder add (idempotent), list, and remove
// (returning ErrFolderNotWatched when absent).
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

// TestFileUpsertAndGet verifies GetFile returns nil for unknown paths and reads
// back a record after UpsertFile.
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

// TestConfigRoundTripWithBucketIDAndBackend verifies a RemoteConfig including a
// bucket ID round-trips through save and get unchanged.
func TestConfigRoundTripWithBucketIDAndBackend(t *testing.T) {
	st, _ := openTest(t)
	ctx := context.Background()
	in := RemoteConfig{
		Endpoint: "https://s3.us-west-004.backblazeb2.com",
		Region:   "us-west-004",
		Bucket:   "my-bucket",
		BucketID: "abc123",
		KeyID:    "0001abcd",
		AppKey:   "K001-secret",
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

// TestBackendDefaultsToS3AndCanBeSet verifies the backend defaults to s3, can be
// set to b2, and rejects invalid backends with ErrInvalidBackend.
func TestBackendDefaultsToS3AndCanBeSet(t *testing.T) {
	st, _ := openTest(t)
	ctx := context.Background()
	got, err := st.GetBackend(ctx)
	if err != nil {
		t.Fatalf("GetBackend: %v", err)
	}
	if got != "s3" {
		t.Fatalf("default backend = %q, want s3", got)
	}
	// SetBackend requires an existing config row.
	if err := st.SaveConfig(ctx, RemoteConfig{Bucket: "b", KeyID: "k", AppKey: "a"}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	if err := st.SetBackend(ctx, "b2"); err != nil {
		t.Fatalf("SetBackend: %v", err)
	}
	got, _ = st.GetBackend(ctx)
	if got != "b2" {
		t.Fatalf("backend after set = %q, want b2", got)
	}
	if err := st.SetBackend(ctx, "azure"); !errors.Is(err, apperr.ErrInvalidBackend) {
		t.Fatalf("want ErrInvalidBackend, got %v", err)
	}
}

// TestOpenMigratesOldSchema verifies Open adds the missing bucket_id/backend
// columns to a pre-existing old-schema config table and works normally.
func TestOpenMigratesOldSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	// Create a DB with the OLD config schema (no bucket_id / backend columns).
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `CREATE TABLE config (
		id INTEGER PRIMARY KEY CHECK (id=1),
		s3_endpoint TEXT, s3_region TEXT, bucket_name TEXT,
		key_id_enc BLOB, app_key_enc BLOB, created_at INTEGER);`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	// Opening through the store must add the missing columns and work normally.
	st, err := Open(ctx, path, key32())
	if err != nil {
		t.Fatalf("Open (migrate): %v", err)
	}
	defer st.Close()
	if b, err := st.GetBackend(ctx); err != nil || b != "s3" {
		t.Fatalf("GetBackend after migrate = %q, %v", b, err)
	}
}

// TestWatchedFolderPrefix covers the per-folder bucket prefix (ADR-027): a plain
// AddFolder has none, AddFolderWithPrefix stores one (and refuses a folder
// already watched), SetFolderPrefix changes it, and only for watched folders.
func TestWatchedFolderPrefix(t *testing.T) {
	st, _ := openTest(t)
	ctx := context.Background()
	if err := st.AddFolder(ctx, "/data/plain"); err != nil {
		t.Fatal(err)
	}
	if err := st.AddFolderWithPrefix(ctx, "/data/sewa", "SEWA/"); err != nil {
		t.Fatalf("AddFolderWithPrefix: %v", err)
	}
	if err := st.AddFolderWithPrefix(ctx, "/data/sewa", "OTHER/"); !errors.Is(err, apperr.ErrStore) {
		t.Errorf("AddFolderWithPrefix(duplicate) = %v, want ErrStore", err)
	}
	got, err := st.ListWatched(ctx)
	want := []Folder{{"/data/plain", ""}, {"/data/sewa", "SEWA/"}}
	if err != nil || len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("ListWatched = %+v, %v; want %+v", got, err, want)
	}
	if err := st.SetFolderPrefix(ctx, "/data/plain", "PLAIN/"); err != nil {
		t.Fatalf("SetFolderPrefix: %v", err)
	}
	if got, _ := st.ListWatched(ctx); got[0].RemotePrefix != "PLAIN/" {
		t.Errorf("prefix after SetFolderPrefix = %q", got[0].RemotePrefix)
	}
	if err := st.SetFolderPrefix(ctx, "/data/nope", "X/"); !errors.Is(err, apperr.ErrFolderNotWatched) {
		t.Errorf("SetFolderPrefix(unwatched) = %v, want ErrFolderNotWatched", err)
	}
	if folders, _ := st.ListFolders(ctx); len(folders) != 2 {
		t.Errorf("ListFolders = %q, want both paths", folders)
	}
}

// TestOpenMigratesFolderPrefix: a folders table from before ADR-027 (no
// remote_prefix column) gains the column on Open, and existing rows read back
// with an empty prefix, so they keep their old object names.
func TestOpenMigratesFolderPrefix(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE folders (path TEXT PRIMARY KEY, added_at INTEGER);
		INSERT INTO folders (path, added_at) VALUES ('/old/docs', 1);`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	st, err := Open(ctx, path, key32())
	if err != nil {
		t.Fatalf("Open (migrate): %v", err)
	}
	defer st.Close()
	got, err := st.ListWatched(ctx)
	if err != nil || len(got) != 1 || got[0] != (Folder{Path: "/old/docs"}) {
		t.Fatalf("ListWatched after migrate = %+v, %v", got, err)
	}
	// Re-opening a migrated DB must not try to add the column again.
	st.Close()
	if st2, err := Open(ctx, path, key32()); err != nil {
		t.Fatalf("re-Open: %v", err)
	} else {
		st2.Close()
	}
}
