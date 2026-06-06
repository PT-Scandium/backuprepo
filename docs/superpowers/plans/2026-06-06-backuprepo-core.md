# backuprepo Core Foundation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the testable core of `backuprepo` — encrypted local DB, B2/S3 uploader, change detection, and CLI subcommands (`init`/`watch`/`unwatch`/`list`/`status`/`upload`/`config`).

**Architecture:** Single Go module, one binary. Logic lives in focused `internal/` packages. Credentials are AES-256-GCM sealed with a 32-byte key from `~/backup_repo/key` (0600) before being stored in a pure-Go SQLite DB. The uploader is an interface (`aws-sdk-go-v2` S3 impl + in-memory fake) so backup logic is unit-testable without real B2.

**Tech Stack:** Go 1.25, `modernc.org/sqlite` (pure Go, no CGO), `aws-sdk-go-v2` (S3 + transfer manager), stdlib `crypto/aes`+`crypto/cipher`, stdlib `flag`.

**Note on `errors.go`:** the design named a root `errors.go`. Go forbids `internal/` packages from importing the root `main` package, so the shared typed-error catalog lives in **`internal/apperr`** instead (importable by every package). This preserves the "typed errors only" invariant; behavior is unchanged.

**Spec:** `docs/superpowers/specs/2026-06-06-backuprepo-core-design.md`

---

## File Structure

| File | Responsibility |
|------|----------------|
| `go.mod` | Module `backuprepo`, deps |
| `internal/apperr/errors.go` | Typed sentinel errors, shared by all packages |
| `internal/crypto/crypto.go` | `Seal`/`Open` (AES-256-GCM) |
| `internal/crypto/crypto_test.go` | round-trip, tamper, wrong-key |
| `internal/config/config.go` | `Config` struct, `~/backup_repo` paths, key-file load/create |
| `internal/config/config_test.go` | key creation, 0600 perms, reuse |
| `internal/store/store.go` | SQLite open/migrate, config (encrypted), folders, files |
| `internal/store/store_test.go` | CRUD + encryption-at-rest |
| `internal/b2/uploader.go` | `Uploader` interface + `Config` + `FakeUploader` |
| `internal/b2/s3.go` | `S3Uploader` (aws-sdk-go-v2) |
| `internal/b2/uploader_test.go` | fake behavior |
| `internal/backup/backup.go` | walk folders, change detection, upload |
| `internal/backup/backup_test.go` | change-detection matrix with fake |
| `internal/cli/cli.go` | subcommand handlers (testable, io injected) |
| `internal/cli/cli_test.go` | handler behavior + exit semantics |
| `main.go` | arg dispatch, wiring real deps |

---

## Task 1: Module setup, dependencies, typed errors

**Files:**
- Create: `go.mod`
- Create: `internal/apperr/errors.go`

- [ ] **Step 1: Initialize the module**

Run:
```bash
cd /home/edisonch/projects/backuprepo
go mod init backuprepo
```
Expected: creates `go.mod` with `module backuprepo` and a `go 1.25` line.

- [ ] **Step 2: Add dependencies**

Run:
```bash
go get modernc.org/sqlite
go get github.com/aws/aws-sdk-go-v2/aws
go get github.com/aws/aws-sdk-go-v2/config
go get github.com/aws/aws-sdk-go-v2/credentials
go get github.com/aws/aws-sdk-go-v2/service/s3
go get github.com/aws/aws-sdk-go-v2/feature/s3/manager
```
Expected: deps appear in `go.mod`/`go.sum`. (Network access required.)

- [ ] **Step 3: Write the typed-error catalog**

Create `internal/apperr/errors.go`:
```go
// Package apperr holds the shared typed sentinel errors for backuprepo.
// Every package wraps one of these with %w and added context, per the
// "errors are typed, never raw strings" invariant.
package apperr

import "errors"

var (
	ErrNotConfigured      = errors.New("backuprepo: not configured (run `backuprepo init`)")
	ErrAlreadyConfigured  = errors.New("backuprepo: already configured")
	ErrInvalidCredentials = errors.New("backuprepo: invalid credentials")
	ErrFolderNotFound     = errors.New("backuprepo: folder not found")
	ErrFolderNotWatched   = errors.New("backuprepo: folder is not watched")
	ErrUploadFailed       = errors.New("backuprepo: upload failed")
	ErrStore              = errors.New("backuprepo: database error")
	ErrCrypto             = errors.New("backuprepo: encryption error")
)
```

- [ ] **Step 4: Verify it compiles**

Run: `go build ./...`
Expected: no output, exit 0.

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum internal/apperr/errors.go
git commit -m "chore: init module, deps, typed error catalog"
```

---

## Task 2: crypto package (AES-256-GCM)

**Files:**
- Create: `internal/crypto/crypto.go`
- Test: `internal/crypto/crypto_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/crypto/crypto_test.go`:
```go
package crypto

import (
	"bytes"
	"testing"
)

func key32() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i)
	}
	return k
}

func TestSealOpenRoundTrip(t *testing.T) {
	k := key32()
	plain := []byte("hello-secret-key-id")
	ct, err := Seal(k, plain)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Contains(ct, plain) {
		t.Fatal("ciphertext leaks plaintext")
	}
	got, err := Open(k, ct)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("round-trip mismatch: got %q", got)
	}
}

func TestOpenWrongKeyFails(t *testing.T) {
	ct, _ := Seal(key32(), []byte("data"))
	wrong := key32()
	wrong[0] ^= 0xFF
	if _, err := Open(wrong, ct); err == nil {
		t.Fatal("expected error with wrong key")
	}
}

func TestOpenTamperedFails(t *testing.T) {
	ct, _ := Seal(key32(), []byte("data"))
	ct[len(ct)-1] ^= 0xFF
	if _, err := Open(key32(), ct); err == nil {
		t.Fatal("expected error with tampered ciphertext")
	}
}

func TestSealRejectsBadKeyLen(t *testing.T) {
	if _, err := Seal([]byte("short"), []byte("x")); err == nil {
		t.Fatal("expected error for non-32-byte key")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/crypto/ -v`
Expected: build failure / FAIL — `Seal`/`Open` undefined.

- [ ] **Step 3: Write minimal implementation**

Create `internal/crypto/crypto.go`:
```go
// Package crypto provides AES-256-GCM sealing for credential fields.
// The nonce is randomly generated per call and prepended to the ciphertext.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"

	"backuprepo/internal/apperr"
)

// KeySize is the required master-key length in bytes (AES-256).
const KeySize = 32

// Seal encrypts plaintext with key, returning nonce||ciphertext.
func Seal(key, plaintext []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("%w: nonce: %v", apperr.ErrCrypto, err)
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// Open decrypts nonce||ciphertext produced by Seal.
func Open(key, data []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	ns := gcm.NonceSize()
	if len(data) < ns {
		return nil, fmt.Errorf("%w: ciphertext too short", apperr.ErrCrypto)
	}
	plain, err := gcm.Open(nil, data[:ns], data[ns:], nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", apperr.ErrCrypto, err)
	}
	return plain, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("%w: key must be %d bytes, got %d", apperr.ErrCrypto, KeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", apperr.ErrCrypto, err)
	}
	return cipher.NewGCM(block)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/crypto/ -v`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/crypto/
git commit -m "feat: AES-256-GCM seal/open for credential fields"
```

---

## Task 3: config package (paths + key file)

**Files:**
- Create: `internal/config/config.go`
- Test: `internal/config/config_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/config/config_test.go`:
```go
package config

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLoadCreatesKeyAndDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // windows

	cfg, err := Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Key) != 32 {
		t.Fatalf("key len = %d, want 32", len(cfg.Key))
	}
	if _, err := os.Stat(cfg.DBPath); err == nil {
		t.Fatal("DB should not be created by config.Load")
	}
	info, err := os.Stat(cfg.KeyPath)
	if err != nil {
		t.Fatalf("key file missing: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("key perms = %v, want 0600", info.Mode().Perm())
	}
	if filepath.Dir(cfg.KeyPath) != cfg.Dir {
		t.Fatal("key not under backup dir")
	}
}

func TestLoadReusesExistingKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	first, err := Load(context.Background())
	if err != nil {
		t.Fatalf("first Load: %v", err)
	}
	second, err := Load(context.Background())
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if !bytes.Equal(first.Key, second.Key) {
		t.Fatal("key changed between loads")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -v`
Expected: FAIL — `Load`/`Config` undefined.

- [ ] **Step 3: Write minimal implementation**

Create `internal/config/config.go`:
```go
// Package config resolves backuprepo's home paths and manages the master key.
// The master key is a random 32-byte file at ~/backup_repo/key (0600); it is
// created on first load and reused afterwards so the daemon can start silently.
package config

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"

	"backuprepo/internal/apperr"
	"backuprepo/internal/crypto"
)

// Config holds resolved paths and the loaded master key.
type Config struct {
	Dir     string // ~/backup_repo
	DBPath  string // ~/backup_repo/backup.db
	KeyPath string // ~/backup_repo/key
	Key     []byte // 32-byte master key
}

// Load resolves paths, creates the backup dir and master key if absent,
// and returns the populated Config. It does NOT create the database.
func Load(ctx context.Context) (*Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("%w: home dir: %v", apperr.ErrStore, err)
	}
	dir := filepath.Join(home, "backup_repo")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("%w: mkdir %s: %v", apperr.ErrStore, dir, err)
	}
	cfg := &Config{
		Dir:     dir,
		DBPath:  filepath.Join(dir, "backup.db"),
		KeyPath: filepath.Join(dir, "key"),
	}
	key, err := loadOrCreateKey(cfg.KeyPath)
	if err != nil {
		return nil, err
	}
	cfg.Key = key
	return cfg, nil
}

func loadOrCreateKey(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		if len(data) != crypto.KeySize {
			return nil, fmt.Errorf("%w: key file corrupt (len %d)", apperr.ErrCrypto, len(data))
		}
		return data, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("%w: read key: %v", apperr.ErrCrypto, err)
	}
	key := make([]byte, crypto.KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("%w: gen key: %v", apperr.ErrCrypto, err)
	}
	if err := os.WriteFile(path, key, 0o600); err != nil {
		return nil, fmt.Errorf("%w: write key: %v", apperr.ErrCrypto, err)
	}
	return key, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/config/ -v`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config/
git commit -m "feat: config paths and 0600 master-key file"
```

---

## Task 4: store package (SQLite, encrypted config, folders, files)

**Files:**
- Create: `internal/store/store.go`
- Test: `internal/store/store_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/store/store_test.go`:
```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -v`
Expected: FAIL — `Open`/`Store`/`S3Config`/`FileRecord` undefined.

- [ ] **Step 3: Write minimal implementation**

Create `internal/store/store.go`:
```go
// Package store is backuprepo's SQLite-backed persistence layer. Credential
// fields are AES-GCM sealed before insertion and opened on read, so secrets
// are never written in plaintext.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	_ "modernc.org/sqlite"

	"backuprepo/internal/apperr"
	"backuprepo/internal/crypto"
)

// Store wraps the SQLite handle and the master key used for field encryption.
type Store struct {
	db  *sql.DB
	key []byte
}

// S3Config is the decrypted destination configuration.
type S3Config struct {
	Endpoint string
	Region   string
	Bucket   string
	KeyID    string
	AppKey   string
}

// FileRecord is a tracked file's backup state. LastBackup is nil if never uploaded.
type FileRecord struct {
	Path       string
	Size       int64
	ModTime    int64
	SHA256     string
	LastBackup *int64
	RemoteKey  string
}

const schema = `
CREATE TABLE IF NOT EXISTS config (
  id           INTEGER PRIMARY KEY CHECK (id = 1),
  s3_endpoint  TEXT, s3_region TEXT, bucket_name TEXT,
  key_id_enc   BLOB, app_key_enc BLOB, created_at INTEGER
);
CREATE TABLE IF NOT EXISTS folders (
  path TEXT PRIMARY KEY, added_at INTEGER
);
CREATE TABLE IF NOT EXISTS files (
  path TEXT PRIMARY KEY, size INTEGER, mod_time INTEGER,
  sha256 TEXT, last_backup INTEGER, remote_key TEXT
);`

// Open opens (creating if needed) the SQLite DB at path and applies the schema.
func Open(ctx context.Context, path string, key []byte) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("%w: open: %v", apperr.ErrStore, err)
	}
	if _, err := db.ExecContext(ctx, schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("%w: migrate: %v", apperr.ErrStore, err)
	}
	return &Store{db: db, key: key}, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// SaveConfig encrypts the credentials and upserts the single config row.
func (s *Store) SaveConfig(ctx context.Context, cfg S3Config) error {
	keyEnc, err := crypto.Seal(s.key, []byte(cfg.KeyID))
	if err != nil {
		return err
	}
	appEnc, err := crypto.Seal(s.key, []byte(cfg.AppKey))
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO config (id, s3_endpoint, s3_region, bucket_name, key_id_enc, app_key_enc, created_at)
		VALUES (1, ?, ?, ?, ?, ?, strftime('%s','now'))
		ON CONFLICT(id) DO UPDATE SET
		  s3_endpoint=excluded.s3_endpoint, s3_region=excluded.s3_region,
		  bucket_name=excluded.bucket_name, key_id_enc=excluded.key_id_enc,
		  app_key_enc=excluded.app_key_enc`,
		cfg.Endpoint, cfg.Region, cfg.Bucket, keyEnc, appEnc)
	if err != nil {
		return fmt.Errorf("%w: save config: %v", apperr.ErrStore, err)
	}
	return nil
}

// GetConfig returns the decrypted config, or ErrNotConfigured if none exists.
func (s *Store) GetConfig(ctx context.Context) (S3Config, error) {
	var cfg S3Config
	var keyEnc, appEnc []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT s3_endpoint, s3_region, bucket_name, key_id_enc, app_key_enc FROM config WHERE id=1`).
		Scan(&cfg.Endpoint, &cfg.Region, &cfg.Bucket, &keyEnc, &appEnc)
	if errors.Is(err, sql.ErrNoRows) {
		return S3Config{}, apperr.ErrNotConfigured
	}
	if err != nil {
		return S3Config{}, fmt.Errorf("%w: get config: %v", apperr.ErrStore, err)
	}
	keyID, err := crypto.Open(s.key, keyEnc)
	if err != nil {
		return S3Config{}, err
	}
	appKey, err := crypto.Open(s.key, appEnc)
	if err != nil {
		return S3Config{}, err
	}
	cfg.KeyID, cfg.AppKey = string(keyID), string(appKey)
	return cfg, nil
}

// IsConfigured reports whether a config row exists.
func (s *Store) IsConfigured(ctx context.Context) (bool, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM config WHERE id=1`).Scan(&n); err != nil {
		return false, fmt.Errorf("%w: %v", apperr.ErrStore, err)
	}
	return n > 0, nil
}

// AddFolder records a watched folder (idempotent).
func (s *Store) AddFolder(ctx context.Context, path string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO folders (path, added_at) VALUES (?, strftime('%s','now'))
		 ON CONFLICT(path) DO NOTHING`, path)
	if err != nil {
		return fmt.Errorf("%w: add folder: %v", apperr.ErrStore, err)
	}
	return nil
}

// RemoveFolder deletes a watched folder, or ErrFolderNotWatched if absent.
func (s *Store) RemoveFolder(ctx context.Context, path string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM folders WHERE path=?`, path)
	if err != nil {
		return fmt.Errorf("%w: remove folder: %v", apperr.ErrStore, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return apperr.ErrFolderNotWatched
	}
	return nil
}

// ListFolders returns watched folder paths sorted ascending.
func (s *Store) ListFolders(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT path FROM folders ORDER BY path`)
	if err != nil {
		return nil, fmt.Errorf("%w: list folders: %v", apperr.ErrStore, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("%w: scan folder: %v", apperr.ErrStore, err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetFile returns the tracked record for path, or (nil, nil) if untracked.
func (s *Store) GetFile(ctx context.Context, path string) (*FileRecord, error) {
	var r FileRecord
	var lastBackup sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT path, size, mod_time, sha256, last_backup, remote_key FROM files WHERE path=?`, path).
		Scan(&r.Path, &r.Size, &r.ModTime, &r.SHA256, &lastBackup, &r.RemoteKey)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: get file: %v", apperr.ErrStore, err)
	}
	if lastBackup.Valid {
		r.LastBackup = &lastBackup.Int64
	}
	return &r, nil
}

// UpsertFile inserts or updates a file record by path.
func (s *Store) UpsertFile(ctx context.Context, r FileRecord) error {
	var lastBackup any
	if r.LastBackup != nil {
		lastBackup = *r.LastBackup
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO files (path, size, mod_time, sha256, last_backup, remote_key)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
		  size=excluded.size, mod_time=excluded.mod_time, sha256=excluded.sha256,
		  last_backup=excluded.last_backup, remote_key=excluded.remote_key`,
		r.Path, r.Size, r.ModTime, r.SHA256, lastBackup, r.RemoteKey)
	if err != nil {
		return fmt.Errorf("%w: upsert file: %v", apperr.ErrStore, err)
	}
	return nil
}

// ListFiles returns all tracked file records sorted by path.
func (s *Store) ListFiles(ctx context.Context) ([]FileRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT path, size, mod_time, sha256, last_backup, remote_key FROM files ORDER BY path`)
	if err != nil {
		return nil, fmt.Errorf("%w: list files: %v", apperr.ErrStore, err)
	}
	defer rows.Close()
	var out []FileRecord
	for rows.Next() {
		var r FileRecord
		var lastBackup sql.NullInt64
		if err := rows.Scan(&r.Path, &r.Size, &r.ModTime, &r.SHA256, &lastBackup, &r.RemoteKey); err != nil {
			return nil, fmt.Errorf("%w: scan file: %v", apperr.ErrStore, err)
		}
		if lastBackup.Valid {
			r.LastBackup = &lastBackup.Int64
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/store/ -v`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store/
git commit -m "feat: SQLite store with encrypted credentials, folders, files"
```

---

## Task 5: b2 package (Uploader interface, fake, S3 impl)

**Files:**
- Create: `internal/b2/uploader.go`
- Create: `internal/b2/s3.go`
- Test: `internal/b2/uploader_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/b2/uploader_test.go`:
```go
package b2

import (
	"bytes"
	"context"
	"io"
	"testing"
)

func TestFakeUploaderStoresAndReports(t *testing.T) {
	ctx := context.Background()
	f := NewFake()

	ok, err := f.Exists(ctx, "k")
	if err != nil || ok {
		t.Fatalf("expected not exist: %v, %v", ok, err)
	}
	if err := f.Upload(ctx, "k", bytes.NewReader([]byte("payload")), 7); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	ok, err = f.Exists(ctx, "k")
	if err != nil || !ok {
		t.Fatalf("expected exist after upload: %v, %v", ok, err)
	}
	if got := string(f.Objects["k"]); got != "payload" {
		t.Fatalf("stored %q", got)
	}
}

// compile-time assertions that both impls satisfy the interface.
var _ Uploader = (*FakeUploader)(nil)
var _ Uploader = (*S3Uploader)(nil)

var _ io.Reader = bytes.NewReader(nil)
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/b2/ -v`
Expected: FAIL — `Uploader`/`NewFake`/`S3Uploader` undefined.

- [ ] **Step 3: Write the interface and fake**

Create `internal/b2/uploader.go`:
```go
// Package b2 uploads objects to a Backblaze B2 bucket via the S3-compatible API.
// Uploader is the abstraction backup logic depends on; FakeUploader backs tests.
package b2

import (
	"context"
	"io"
)

// Uploader stores objects in the destination bucket.
type Uploader interface {
	Upload(ctx context.Context, key string, r io.Reader, size int64) error
	Exists(ctx context.Context, key string) (bool, error)
}

// Config is the destination credentials/addressing for the real uploader.
type Config struct {
	Endpoint string
	Region   string
	Bucket   string
	KeyID    string
	AppKey   string
}

// FakeUploader is an in-memory Uploader for tests.
type FakeUploader struct {
	Objects map[string][]byte
}

// NewFake returns an empty in-memory uploader.
func NewFake() *FakeUploader {
	return &FakeUploader{Objects: map[string][]byte{}}
}

// Upload reads r fully and stores it under key.
func (f *FakeUploader) Upload(ctx context.Context, key string, r io.Reader, size int64) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.Objects[key] = data
	return nil
}

// Exists reports whether key was previously uploaded.
func (f *FakeUploader) Exists(ctx context.Context, key string) (bool, error) {
	_, ok := f.Objects[key]
	return ok, nil
}
```

- [ ] **Step 4: Write the S3 implementation**

Create `internal/b2/s3.go`:
```go
package b2

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"backuprepo/internal/apperr"
)

// multipartPartSize is the part size for the transfer manager. The manager
// automatically uses a single PutObject for small files and multipart for
// large ones, satisfying the spec's 100 MB threshold.
const multipartPartSize = 100 * 1024 * 1024

// S3Uploader uploads to a B2 bucket via the S3-compatible API.
type S3Uploader struct {
	client *s3.Client
	bucket string
}

// NewS3Uploader builds an S3 client pointed at the B2 endpoint using static creds.
func NewS3Uploader(ctx context.Context, cfg Config) (*S3Uploader, error) {
	if cfg.KeyID == "" || cfg.AppKey == "" {
		return nil, apperr.ErrInvalidCredentials
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(cfg.Region),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.KeyID, cfg.AppKey, "")),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: aws config: %v", apperr.ErrUploadFailed, err)
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		o.UsePathStyle = true // B2 S3 works reliably with path-style addressing
	})
	return &S3Uploader{client: client, bucket: cfg.Bucket}, nil
}

// Upload stores r under key, auto-selecting single vs multipart by size.
func (u *S3Uploader) Upload(ctx context.Context, key string, r io.Reader, size int64) error {
	uploader := manager.NewUploader(u.client, func(m *manager.Uploader) {
		m.PartSize = multipartPartSize
	})
	_, err := uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket: aws.String(u.bucket),
		Key:    aws.String(key),
		Body:   r,
	})
	if err != nil {
		return fmt.Errorf("%w: put %s: %v", apperr.ErrUploadFailed, key, err)
	}
	return nil
}

// Exists reports whether key is present in the bucket.
func (u *S3Uploader) Exists(ctx context.Context, key string) (bool, error) {
	_, err := u.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(u.bucket),
		Key:    aws.String(key),
	})
	if err == nil {
		return true, nil
	}
	var notFound *types.NotFound
	var noKey *types.NoSuchKey
	if errors.As(err, &notFound) || errors.As(err, &noKey) {
		return false, nil
	}
	return false, fmt.Errorf("%w: head %s: %v", apperr.ErrUploadFailed, key, err)
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/b2/ -v`
Expected: all PASS (compile-time interface assertions hold).

- [ ] **Step 6: Commit**

```bash
git add internal/b2/
git commit -m "feat: B2 uploader interface, in-memory fake, S3 implementation"
```

---

## Task 6: backup package (walk + change detection + upload)

**Files:**
- Create: `internal/backup/backup.go`
- Test: `internal/backup/backup_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/backup/backup_test.go`:
```go
package backup

import (
	"context"
	"os"
	"path/filepath"
	"testing"

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

func newSvc(t *testing.T) (*Service, *store.Store, *b2.FakeUploader) {
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
```

Also add this helper at the bottom of the test file (kept separate so the time import is obvious):
```go
import "time" // add to the import block above

func mustTime(t *testing.T) time.Time {
	t.Helper()
	return time.Now().Add(2 * time.Second)
}
```
> When writing the file, merge `"time"` into the single import block rather than a second `import` line.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/backup/ -v`
Expected: FAIL — `Service`/`New`/`UploadChanged`/`PendingCount` undefined.

- [ ] **Step 3: Write minimal implementation**

Create `internal/backup/backup.go`:
```go
// Package backup walks watched folders, detects changed files against the
// store, and uploads them via the b2.Uploader. Change detection uses size+mtime
// first and falls back to a SHA-256 content hash, skipping unchanged files.
package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"backuprepo/internal/apperr"
	"backuprepo/internal/b2"
	"backuprepo/internal/store"
)

// Service orchestrates change detection and uploads.
type Service struct {
	store *store.Store
	up    b2.Uploader
}

// New constructs a Service from a store and an uploader.
func New(st *store.Store, up b2.Uploader) *Service {
	return &Service{store: st, up: up}
}

// Result summarizes an UploadChanged run.
type Result struct {
	Uploaded int
	Skipped  int
	Failed   int
	Errors   []error
}

// UploadChanged scans every watched folder and uploads changed/new files.
// A single file's failure is recorded but does not abort the run.
func (s *Service) UploadChanged(ctx context.Context) (Result, error) {
	var res Result
	err := s.eachFile(ctx, func(path string, info fs.FileInfo) error {
		changed, hash, prior, err := s.isChanged(ctx, path, info)
		if err != nil {
			res.Failed++
			res.Errors = append(res.Errors, err)
			return nil
		}
		if !changed {
			res.Skipped++
			return nil
		}
		if err := s.uploadOne(ctx, path, info, hash, prior); err != nil {
			res.Failed++
			res.Errors = append(res.Errors, err)
			return nil
		}
		res.Uploaded++
		return nil
	})
	if err != nil {
		return res, err
	}
	if res.Failed > 0 {
		return res, fmt.Errorf("%w: %d file(s) failed", apperr.ErrUploadFailed, res.Failed)
	}
	return res, nil
}

// PendingCount returns how many files would be uploaded by UploadChanged.
func (s *Service) PendingCount(ctx context.Context) (int, error) {
	n := 0
	err := s.eachFile(ctx, func(path string, info fs.FileInfo) error {
		changed, _, _, err := s.isChanged(ctx, path, info)
		if err != nil {
			return err
		}
		if changed {
			n++
		}
		return nil
	})
	return n, err
}

// eachFile walks all watched folders and calls fn for each regular file.
func (s *Service) eachFile(ctx context.Context, fn func(path string, info fs.FileInfo) error) error {
	folders, err := s.store.ListFolders(ctx)
	if err != nil {
		return err
	}
	for _, folder := range folders {
		walkErr := filepath.WalkDir(folder, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // skip unreadable entries rather than abort the whole walk
			}
			if d.IsDir() || !d.Type().IsRegular() {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			return fn(path, info)
		})
		if walkErr != nil {
			return walkErr
		}
	}
	return nil
}

// isChanged reports whether path differs from its stored record. It returns the
// content hash (computed only when needed) and the prior record (may be nil).
func (s *Service) isChanged(ctx context.Context, path string, info fs.FileInfo) (bool, string, *store.FileRecord, error) {
	prior, err := s.store.GetFile(ctx, path)
	if err != nil {
		return false, "", nil, err
	}
	size := info.Size()
	mtime := info.ModTime().Unix()
	if prior != nil && prior.Size == size && prior.ModTime == mtime && prior.LastBackup != nil {
		return false, prior.SHA256, prior, nil // cheap check: unchanged
	}
	hash, err := hashFile(path)
	if err != nil {
		return false, "", prior, fmt.Errorf("%w: hash %s: %v", apperr.ErrUploadFailed, path, err)
	}
	if prior != nil && prior.SHA256 == hash && prior.LastBackup != nil {
		// content identical (only metadata moved); refresh metadata, no upload
		_ = s.store.UpsertFile(ctx, store.FileRecord{
			Path: path, Size: size, ModTime: mtime, SHA256: hash,
			LastBackup: prior.LastBackup, RemoteKey: prior.RemoteKey,
		})
		return false, hash, prior, nil
	}
	return true, hash, prior, nil
}

// uploadOne uploads the file and records its new backup state.
func (s *Service) uploadOne(ctx context.Context, path string, info fs.FileInfo, hash string, prior *store.FileRecord) error {
	key := RemoteKey(path)
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("%w: open %s: %v", apperr.ErrUploadFailed, path, err)
	}
	defer f.Close()
	if err := s.up.Upload(ctx, key, f, info.Size()); err != nil {
		return err
	}
	now := time.Now().Unix()
	return s.store.UpsertFile(ctx, store.FileRecord{
		Path: path, Size: info.Size(), ModTime: info.ModTime().Unix(),
		SHA256: hash, LastBackup: &now, RemoteKey: key,
	})
}

// RemoteKey maps an absolute local path to a stable bucket object key.
func RemoteKey(path string) string {
	return strings.TrimPrefix(filepath.ToSlash(path), "/")
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := copyInto(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
```

Add the small copy helper at the end of the same file:
```go
import "io" // merge into the import block above

func copyInto(dst io.Writer, src io.Reader) (int64, error) {
	return io.Copy(dst, src)
}
```
> Merge `"io"` into the single import block; do not add a second `import` line.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/backup/ -v`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/backup/
git commit -m "feat: backup orchestration with size/mtime+sha256 change detection"
```

---

## Task 7: cli package (subcommand handlers)

**Files:**
- Create: `internal/cli/cli.go`
- Test: `internal/cli/cli_test.go`

Handlers take an `io.Writer` for output (and `io.Reader` for `Init`) so they are
testable without touching real stdin/stdout. `main.go` (Task 8) wires real deps.

- [ ] **Step 1: Write the failing test**

Create `internal/cli/cli_test.go`:
```go
package cli

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"backuprepo/internal/apperr"
	"backuprepo/internal/store"
)

func key32() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i)
	}
	return k
}

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "c.db"), key32())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestInitThenConfigMasksSecret(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	in := strings.NewReader(strings.Join([]string{
		"0001keyid",                                  // keyID
		"K001-this-is-secret",                        // appKey
		"my-bucket",                                  // bucket name
		"https://s3.us-west-004.backblazeb2.com",     // endpoint
		"us-west-004",                                // region
		"",                                           // first folder (skip)
	}, "\n"))
	var out bytes.Buffer
	if err := Init(ctx, st, in, &out); err != nil {
		t.Fatalf("Init: %v", err)
	}

	var cfgOut bytes.Buffer
	if err := Config(ctx, st, &cfgOut); err != nil {
		t.Fatalf("Config: %v", err)
	}
	s := cfgOut.String()
	if strings.Contains(s, "K001-this-is-secret") {
		t.Fatal("config output leaked the secret app key")
	}
	if !strings.Contains(s, "my-bucket") {
		t.Fatalf("config output missing bucket: %q", s)
	}
}

func TestWatchUnwatchList(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	dir := t.TempDir()

	var out bytes.Buffer
	if err := Watch(ctx, st, dir, &out); err != nil {
		t.Fatalf("Watch: %v", err)
	}
	out.Reset()
	if err := List(ctx, st, &out); err != nil {
		t.Fatalf("List: %v", err)
	}
	if !strings.Contains(out.String(), dir) {
		t.Fatalf("List missing folder: %q", out.String())
	}
	if err := Unwatch(ctx, st, dir, &out); err != nil {
		t.Fatalf("Unwatch: %v", err)
	}
	if err := Unwatch(ctx, st, dir, &out); !errors.Is(err, apperr.ErrFolderNotWatched) {
		t.Fatalf("want ErrFolderNotWatched, got %v", err)
	}
}

func TestWatchRejectsMissingDir(t *testing.T) {
	st := newStore(t)
	var out bytes.Buffer
	err := Watch(context.Background(), st, "/no/such/dir/here", &out)
	if !errors.Is(err, apperr.ErrFolderNotFound) {
		t.Fatalf("want ErrFolderNotFound, got %v", err)
	}
}

func TestStatusNotConfigured(t *testing.T) {
	st := newStore(t)
	var out bytes.Buffer
	if err := Status(context.Background(), st, &out); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !strings.Contains(out.String(), "not configured") {
		t.Fatalf("status should report not configured: %q", out.String())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -v`
Expected: FAIL — handlers undefined.

- [ ] **Step 3: Write minimal implementation**

Create `internal/cli/cli.go`:
```go
// Package cli implements backuprepo's subcommand handlers. Handlers take an
// io.Writer (and io.Reader for Init) so they are unit-testable; main.go wires
// the real stdin/stdout and the real uploader.
package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"backuprepo/internal/apperr"
	"backuprepo/internal/b2"
	"backuprepo/internal/backup"
	"backuprepo/internal/store"
)

// Init runs interactive setup, reading answers from in and writing prompts to out.
func Init(ctx context.Context, st *store.Store, in io.Reader, out io.Writer) error {
	r := bufio.NewReader(in)
	ask := func(label string) string {
		fmt.Fprintf(out, "%s: ", label)
		line, _ := r.ReadString('\n')
		return strings.TrimSpace(line)
	}

	cfg := store.S3Config{
		KeyID:    ask("Backblaze keyID (access key ID)"),
		AppKey:   ask("Backblaze applicationKey (secret)"),
		Bucket:   ask("Bucket name"),
		Endpoint: ask("S3 endpoint URL (e.g. https://s3.us-west-004.backblazeb2.com)"),
		Region:   ask("S3 region (e.g. us-west-004)"),
	}
	if cfg.KeyID == "" || cfg.AppKey == "" || cfg.Bucket == "" {
		return fmt.Errorf("%w: keyID, applicationKey and bucket are required", apperr.ErrInvalidCredentials)
	}
	if err := st.SaveConfig(ctx, cfg); err != nil {
		return err
	}
	fmt.Fprintln(out, "Configuration saved.")

	folder := ask("Folder to watch (blank to skip)")
	if folder != "" {
		if err := Watch(ctx, st, folder, out); err != nil {
			return err
		}
	}
	return nil
}

// Watch adds an existing directory to the watch list.
func Watch(ctx context.Context, st *store.Store, path string, out io.Writer) error {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("%w: %s", apperr.ErrFolderNotFound, path)
	}
	if err := st.AddFolder(ctx, path); err != nil {
		return err
	}
	fmt.Fprintf(out, "Watching %s\n", path)
	return nil
}

// Unwatch removes a folder from the watch list.
func Unwatch(ctx context.Context, st *store.Store, path string, out io.Writer) error {
	if err := st.RemoveFolder(ctx, path); err != nil {
		return err
	}
	fmt.Fprintf(out, "Stopped watching %s\n", path)
	return nil
}

// List prints watched folders and tracked files with backup state.
func List(ctx context.Context, st *store.Store, out io.Writer) error {
	folders, err := st.ListFolders(ctx)
	if err != nil {
		return err
	}
	if len(folders) == 0 {
		fmt.Fprintln(out, "No folders are being watched.")
		return nil
	}
	fmt.Fprintln(out, "Watched folders:")
	for _, f := range folders {
		fmt.Fprintf(out, "  %s\n", f)
	}
	files, err := st.ListFiles(ctx)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return nil
	}
	fmt.Fprintln(out, "\nTracked files:")
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "PATH\tSIZE\tLAST BACKUP")
	for _, fr := range files {
		fmt.Fprintf(tw, "%s\t%d\t%s\n", fr.Path, fr.Size, lastBackup(fr.LastBackup))
	}
	return tw.Flush()
}

// Status prints whether configured, folder count, and pending upload count.
func Status(ctx context.Context, st *store.Store, out io.Writer) error {
	configured, err := st.IsConfigured(ctx)
	if err != nil {
		return err
	}
	if !configured {
		fmt.Fprintln(out, "Status: not configured (run `backuprepo init`)")
		return nil
	}
	folders, err := st.ListFolders(ctx)
	if err != nil {
		return err
	}
	svc := backup.New(st, b2.NewFake()) // PendingCount does not upload
	pending, err := svc.PendingCount(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Status: configured\nWatched folders: %d\nPending uploads: %d\n",
		len(folders), pending)
	return nil
}

// Config prints the current config with the secret masked.
func Config(ctx context.Context, st *store.Store, out io.Writer) error {
	cfg, err := st.GetConfig(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Endpoint:    %s\n", cfg.Endpoint)
	fmt.Fprintf(out, "Region:      %s\n", cfg.Region)
	fmt.Fprintf(out, "Bucket:      %s\n", cfg.Bucket)
	fmt.Fprintf(out, "Key ID:      %s\n", cfg.KeyID)
	fmt.Fprintf(out, "App Key:     %s\n", mask(cfg.AppKey))
	folders, err := st.ListFolders(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Watched folders: %d\n", len(folders))
	for _, f := range folders {
		fmt.Fprintf(out, "  %s\n", f)
	}
	return nil
}

// Upload force-scans watched folders and uploads changed files.
func Upload(ctx context.Context, st *store.Store, up b2.Uploader, out io.Writer) error {
	svc := backup.New(st, up)
	res, err := svc.UploadChanged(ctx)
	fmt.Fprintf(out, "Uploaded: %d, Skipped: %d, Failed: %d\n", res.Uploaded, res.Skipped, res.Failed)
	return err
}

func mask(secret string) string {
	if len(secret) <= 4 {
		return "****"
	}
	return "****" + secret[len(secret)-4:]
}

func lastBackup(ts *int64) string {
	if ts == nil {
		return "never"
	}
	return time.Unix(*ts, 0).Format(time.RFC3339)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/cli/ -v`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/
git commit -m "feat: CLI subcommand handlers (init/watch/unwatch/list/status/upload/config)"
```

---

## Task 8: main.go dispatch + wiring + build

**Files:**
- Create: `main.go`

- [ ] **Step 1: Write main.go**

Create `main.go`:
```go
// Command backuprepo uploads changed files from watched folders to a Backblaze
// B2 bucket via the S3-compatible API. This binary covers the core CLI; the
// background daemon and web UI are separate follow-up work.
package main

import (
	"context"
	"fmt"
	"os"

	"backuprepo/internal/b2"
	"backuprepo/internal/cli"
	"backuprepo/internal/config"
	"backuprepo/internal/store"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	ctx := context.Background()
	cfg, err := config.Load(ctx)
	if err != nil {
		return fail(err)
	}
	st, err := store.Open(ctx, cfg.DBPath, cfg.Key)
	if err != nil {
		return fail(err)
	}
	defer st.Close()

	cmd := "status"
	var rest []string
	if len(args) > 0 {
		cmd, rest = args[0], args[1:]
	}

	switch cmd {
	case "init":
		err = cli.Init(ctx, st, os.Stdin, os.Stdout)
	case "watch":
		if len(rest) != 1 {
			return fail(fmt.Errorf("usage: backuprepo watch /path/to/dir"))
		}
		err = cli.Watch(ctx, st, rest[0], os.Stdout)
	case "unwatch":
		if len(rest) != 1 {
			return fail(fmt.Errorf("usage: backuprepo unwatch /path/to/dir"))
		}
		err = cli.Unwatch(ctx, st, rest[0], os.Stdout)
	case "list":
		err = cli.List(ctx, st, os.Stdout)
	case "status":
		err = cli.Status(ctx, st, os.Stdout)
	case "config":
		err = cli.Config(ctx, st, os.Stdout)
	case "upload":
		err = runUpload(ctx, st)
	case "help", "-h", "--help":
		usage(os.Stdout)
	default:
		usage(os.Stderr)
		return 1
	}
	if err != nil {
		return fail(err)
	}
	return 0
}

func runUpload(ctx context.Context, st *store.Store) error {
	cfg, err := st.GetConfig(ctx)
	if err != nil {
		return err
	}
	up, err := b2.NewS3Uploader(ctx, b2.Config{
		Endpoint: cfg.Endpoint, Region: cfg.Region, Bucket: cfg.Bucket,
		KeyID: cfg.KeyID, AppKey: cfg.AppKey,
	})
	if err != nil {
		return err
	}
	return cli.Upload(ctx, st, up, os.Stdout)
}

func usage(w *os.File) {
	fmt.Fprint(w, `backuprepo - back up watched folders to Backblaze B2

Usage:
  backuprepo init                 Interactive setup (credentials, bucket, folder)
  backuprepo watch /path/to/dir   Add a folder to the watch list
  backuprepo unwatch /path/to/dir Remove a folder from the watch list
  backuprepo list                 List watched folders and tracked files
  backuprepo status               Show configuration and pending uploads
  backuprepo upload               Upload all changed files now
  backuprepo config               Show current configuration (secret masked)
`)
}

func fail(err error) int {
	fmt.Fprintln(os.Stderr, "error:", err)
	return 1
}
```

- [ ] **Step 2: Build the whole project**

Run: `go build ./...`
Expected: no output, exit 0.

- [ ] **Step 3: Run the full test suite**

Run: `go test ./...`
Expected: all packages PASS (`ok` for crypto, config, store, b2, backup, cli).

- [ ] **Step 4: Vet and build the stripped binary**

Run:
```bash
go vet ./...
go build -ldflags="-s -w" -o backuprepo .
ls -lh backuprepo
```
Expected: vet clean; binary produced (verify size toward the <10 MB goal — modernc sqlite + aws sdk may push this; note actual size).

- [ ] **Step 5: Smoke-test the CLI against a temp HOME**

Run:
```bash
HOME=$(mktemp -d) ./backuprepo status
```
Expected: prints `Status: not configured (run \`backuprepo init\`)`, exit 0.

- [ ] **Step 6: Add a .gitignore for the binary and commit**

Create `.gitignore`:
```
/backuprepo
```
Then:
```bash
git add main.go .gitignore
git commit -m "feat: CLI entrypoint, dispatch, real uploader wiring"
```

---

## Task 9: README + manual B2 verification (optional, needs real creds)

**Files:**
- Create: `README.md`

- [ ] **Step 1: Write a short README**

Create `README.md` documenting build (`go build -ldflags="-s -w" -o backuprepo .`),
the subcommands, the `~/backup_repo/` layout (`backup.db`, `key`), and the bucket
**name** (not ID) requirement. (Content is free-form prose; no code to verify.)

- [ ] **Step 2: Commit the README**

```bash
git add README.md
git commit -m "docs: README for backuprepo core CLI"
```

- [ ] **Step 3: Manual end-to-end test (only when real B2 creds are available)**

Run interactively:
```bash
./backuprepo init        # enter real keyID, applicationKey, bucket NAME, endpoint, region
./backuprepo watch /some/test/folder
./backuprepo upload      # expect "Uploaded: N, Skipped: 0, Failed: 0"
./backuprepo upload      # second run: expect "Uploaded: 0, Skipped: N"
```
Expected: objects appear in the B2 bucket; second run skips unchanged files.
This step is manual and is NOT part of the automated suite.

---

## Self-Review Notes

- **Spec coverage:** config+key file (Task 3) ✓; encrypted DB/folders/files (Task 4) ✓;
  uploader interface + S3 + fake (Task 5) ✓; change detection + per-file upload (Task 6) ✓;
  all in-scope CLI subcommands (Tasks 7–8) ✓; typed errors (Task 1) ✓; ctx-first (all) ✓;
  build/strip (Task 8) ✓. Daemon + web UI correctly excluded (follow-up spec).
- **Naming consistency:** `S3Config` (store) vs `b2.Config` are intentionally distinct;
  `runUpload` maps one to the other. `RemoteKey`, `UploadChanged`, `PendingCount`,
  `IsConfigured` used consistently across tasks.
- **No placeholders:** every code step contains complete code; the two "merge the import"
  notes are explicit instructions, not deferred work.
- **Risk noted:** binary size may exceed the <10 MB goal due to aws-sdk-go-v2 + modernc
  sqlite. Task 8 Step 4 records the actual size so we can decide on trimming later.
