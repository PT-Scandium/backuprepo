# backuprepo Dual-Backend + Manual File Client Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a native Backblaze B2 API backend alongside the existing S3 backend behind one `Backend` interface, a stored+overridable backend mode, and manual bucket commands (`ls`/`get`/`put`/`rm`/`find`/`backend`).

**Architecture:** Introduce `b2.Backend` (superset of the existing `b2.Uploader`) with two impls — `S3Backend` (aws-sdk, extends today's uploader) and `B2Backend` (native B2 API over stdlib `net/http`). A factory selects the impl from the stored/flagged mode. Backup keeps the narrow `Uploader` view (interface segregation); manual commands use the full `Backend`. The same concrete backend instance serves both.

**Tech Stack:** Go 1.25, `modernc.org/sqlite`, `aws-sdk-go-v2`, stdlib `net/http`/`encoding/json`/`crypto/sha1`, `flag`.

**Spec:** `docs/superpowers/specs/2026-06-06-backuprepo-backends-design.md`

**Design note (interface segregation):** The spec's "unified backend" requirement is met by passing ONE concrete backend (S3 or B2) everywhere. `backup.Service` continues to depend only on `b2.Uploader` (`Upload`+`Exists`) because that is all it needs; manual file commands depend on the wider `b2.Backend`. `Backend` embeds `Uploader`, so a `Backend` value is also an `Uploader`. This keeps `backup` untouched while still unifying the storage protocol.

---

## File Structure

| File | Action | Responsibility |
|------|--------|----------------|
| `internal/apperr/errors.go` | modify | Add `ErrAuthFailed`, `ErrObjectNotFound`, `ErrDownloadFailed`, `ErrDeleteFailed`, `ErrListFailed`, `ErrInvalidBackend` |
| `internal/b2/backend.go` | create | `Uploader`, `Backend`, `ObjectInfo`, `Listing`, `Config`, `NewBackend` factory |
| `internal/b2/uploader.go` | delete | contents moved to `backend.go` / `fake.go` |
| `internal/b2/fake.go` | create | `FakeBackend` (full interface) + `NewFake` |
| `internal/b2/s3.go` | modify | `S3Backend` (rename from `S3Uploader`) implementing full `Backend` |
| `internal/b2/native.go` | create | `B2Backend` native client (auth, upload-small, download, list, delete, search) |
| `internal/b2/largefile.go` | create | `B2Backend` large-file (multipart) upload |
| `internal/b2/native_test.go` | create | httptest-based B2 client tests |
| `internal/b2/uploader_test.go` | modify→rename `fake_test.go` | fake tests + interface assertions |
| `internal/store/store.go` | modify | `RemoteConfig` (rename), `bucket_id`/`backend` columns + migration, `GetBackend`/`SetBackend` |
| `internal/store/store_test.go` | modify | migration + backend get/set + config round-trip |
| `internal/backup/backup.go` | modify | `New` accepts `b2.Uploader` (unchanged behavior; just confirm type) |
| `internal/cli/cli.go` | modify | `Init` bucket-ID prompt; `Config`/`Status` show backend+bucketID; `RemoteConfig` rename |
| `internal/cli/cli_test.go` | modify | adapt to `RemoteConfig` + new init prompt |
| `internal/cli/files.go` | create | `Ls`/`Get`/`Put`/`Rm`/`Find`/`Backend` handlers |
| `internal/cli/files_test.go` | create | handler tests with `FakeBackend` |
| `main.go` | modify | dispatch new commands, per-subcommand flags, effective-backend factory |

---

## Task 1: New typed errors

**Files:** Modify `internal/apperr/errors.go`

- [ ] **Step 1: Add the new sentinels**

Replace the `var ( ... )` block in `internal/apperr/errors.go` with:
```go
var (
	ErrNotConfigured      = errors.New("backuprepo: not configured (run `backuprepo init`)")
	ErrAlreadyConfigured  = errors.New("backuprepo: already configured")
	ErrInvalidCredentials = errors.New("backuprepo: invalid credentials")
	ErrFolderNotFound     = errors.New("backuprepo: folder not found")
	ErrFolderNotWatched   = errors.New("backuprepo: folder is not watched")
	ErrUploadFailed       = errors.New("backuprepo: upload failed")
	ErrDownloadFailed     = errors.New("backuprepo: download failed")
	ErrListFailed         = errors.New("backuprepo: list failed")
	ErrDeleteFailed       = errors.New("backuprepo: delete failed")
	ErrObjectNotFound     = errors.New("backuprepo: object not found")
	ErrAuthFailed         = errors.New("backuprepo: authentication failed")
	ErrInvalidBackend     = errors.New("backuprepo: invalid backend (use 's3' or 'b2')")
	ErrStore              = errors.New("backuprepo: database error")
	ErrCrypto             = errors.New("backuprepo: encryption error")
)
```

- [ ] **Step 2: Verify build**

Run: `go build ./...`
Expected: exit 0.

- [ ] **Step 3: Commit**

```bash
git add internal/apperr/errors.go
git commit -m "feat: add backend/file-op typed errors"
```

---

## Task 2: Backend interface + FakeBackend + S3Backend full implementation

**Files:**
- Create `internal/b2/backend.go`, `internal/b2/fake.go`
- Delete `internal/b2/uploader.go`
- Modify `internal/b2/s3.go`
- Rename `internal/b2/uploader_test.go` → `internal/b2/fake_test.go`
- Modify `main.go` (one call site)

- [ ] **Step 1: Write the new fake tests (failing)**

Delete `internal/b2/uploader_test.go` and create `internal/b2/fake_test.go`:
```go
package b2

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"backuprepo/internal/apperr"
)

func TestFakeUploadDownloadExists(t *testing.T) {
	ctx := context.Background()
	f := NewFake()

	if ok, _ := f.Exists(ctx, "k"); ok {
		t.Fatal("should not exist yet")
	}
	if err := f.Upload(ctx, "k", bytes.NewReader([]byte("payload")), 7); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if ok, _ := f.Exists(ctx, "k"); !ok {
		t.Fatal("should exist after upload")
	}
	rc, n, err := f.Download(ctx, "k")
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	defer rc.Close()
	data, _ := io.ReadAll(rc)
	if string(data) != "payload" || n != 7 {
		t.Fatalf("download mismatch: %q n=%d", data, n)
	}
}

func TestFakeDownloadMissing(t *testing.T) {
	_, _, err := NewFake().Download(context.Background(), "nope")
	if !errors.Is(err, apperr.ErrObjectNotFound) {
		t.Fatalf("want ErrObjectNotFound, got %v", err)
	}
}

func TestFakeListNonRecursiveGroupsFolders(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	for _, k := range []string{"a.txt", "photos/1.jpg", "photos/2.jpg", "photos/sub/3.jpg"} {
		f.Upload(ctx, k, bytes.NewReader([]byte("x")), 1)
	}
	l, err := f.List(ctx, "", false)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(l.Objects) != 1 || l.Objects[0].Key != "a.txt" {
		t.Fatalf("objects = %+v", l.Objects)
	}
	if len(l.Prefixes) != 1 || l.Prefixes[0] != "photos/" {
		t.Fatalf("prefixes = %+v", l.Prefixes)
	}
}

func TestFakeListRecursive(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	for _, k := range []string{"photos/1.jpg", "photos/sub/3.jpg"} {
		f.Upload(ctx, k, bytes.NewReader([]byte("x")), 1)
	}
	l, err := f.List(ctx, "photos/", true)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(l.Objects) != 2 || len(l.Prefixes) != 0 {
		t.Fatalf("recursive list = %+v", l)
	}
}

func TestFakeDelete(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	f.Upload(ctx, "k", bytes.NewReader([]byte("x")), 1)
	if err := f.Delete(ctx, "k"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if ok, _ := f.Exists(ctx, "k"); ok {
		t.Fatal("should be gone")
	}
	if err := f.Delete(ctx, "k"); !errors.Is(err, apperr.ErrObjectNotFound) {
		t.Fatalf("want ErrObjectNotFound, got %v", err)
	}
}

// compile-time interface assertions
var _ Backend = (*FakeBackend)(nil)
var _ Backend = (*S3Backend)(nil)
var _ Uploader = (*FakeBackend)(nil)
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/b2/ -v`
Expected: build failure — `Backend`, `FakeBackend`, `S3Backend`, `Download`, `List` undefined.

- [ ] **Step 3: Create `backend.go`**

Create `internal/b2/backend.go`:
```go
// Package b2 stores and retrieves objects in a Backblaze B2 bucket, via either
// the S3-compatible API (S3Backend) or the native B2 API (B2Backend). Backend
// is the abstraction manual file commands depend on; Uploader is the narrower
// view the backup flow needs. FakeBackend backs tests.
package b2

import (
	"context"
	"io"
	"time"

	"backuprepo/internal/apperr"
)

// ObjectInfo describes one stored object.
type ObjectInfo struct {
	Key      string
	Size     int64
	Modified time.Time
}

// Listing is the result of List: files directly under a prefix, plus "folder"
// prefixes (common prefixes) when not listing recursively.
type Listing struct {
	Objects  []ObjectInfo
	Prefixes []string
}

// Uploader is the narrow write view the backup flow depends on.
type Uploader interface {
	Upload(ctx context.Context, key string, r io.Reader, size int64) error
	Exists(ctx context.Context, key string) (bool, error)
}

// Backend is the full bucket interface used by the manual file commands.
type Backend interface {
	Uploader
	Download(ctx context.Context, key string) (io.ReadCloser, int64, error)
	List(ctx context.Context, prefix string, recursive bool) (Listing, error)
	Delete(ctx context.Context, key string) error
}

// Config carries everything both backends need. BucketName is used by the S3
// API and by B2 download-by-name; BucketID is used by the B2 native list/upload.
type Config struct {
	Endpoint   string
	Region     string
	BucketName string
	BucketID   string
	KeyID      string
	AppKey     string
}

// NewBackend builds the backend for kind ("s3" or "b2").
func NewBackend(ctx context.Context, kind string, cfg Config) (Backend, error) {
	switch kind {
	case "", "s3":
		return newS3Backend(ctx, cfg)
	case "b2":
		return newB2Backend(cfg), nil
	default:
		return nil, apperr.ErrInvalidBackend
	}
}
```

- [ ] **Step 4: Create `fake.go`**

Create `internal/b2/fake.go`:
```go
package b2

import (
	"bytes"
	"context"
	"io"
	"sort"
	"strings"
	"time"

	"backuprepo/internal/apperr"
)

// FakeBackend is an in-memory Backend for tests.
type FakeBackend struct {
	Objects map[string][]byte
}

// NewFake returns an empty in-memory backend.
func NewFake() *FakeBackend {
	return &FakeBackend{Objects: map[string][]byte{}}
}

func (f *FakeBackend) Upload(ctx context.Context, key string, r io.Reader, size int64) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.Objects[key] = data
	return nil
}

func (f *FakeBackend) Exists(ctx context.Context, key string) (bool, error) {
	_, ok := f.Objects[key]
	return ok, nil
}

func (f *FakeBackend) Download(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	data, ok := f.Objects[key]
	if !ok {
		return nil, 0, apperr.ErrObjectNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), int64(len(data)), nil
}

func (f *FakeBackend) Delete(ctx context.Context, key string) error {
	if _, ok := f.Objects[key]; !ok {
		return apperr.ErrObjectNotFound
	}
	delete(f.Objects, key)
	return nil
}

func (f *FakeBackend) List(ctx context.Context, prefix string, recursive bool) (Listing, error) {
	var out Listing
	seen := map[string]bool{}
	keys := make([]string, 0, len(f.Objects))
	for k := range f.Objects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		rest := k[len(prefix):]
		if recursive {
			out.Objects = append(out.Objects, ObjectInfo{Key: k, Size: int64(len(f.Objects[k])), Modified: time.Time{}})
			continue
		}
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			folder := prefix + rest[:i+1]
			if !seen[folder] {
				seen[folder] = true
				out.Prefixes = append(out.Prefixes, folder)
			}
			continue
		}
		out.Objects = append(out.Objects, ObjectInfo{Key: k, Size: int64(len(f.Objects[k])), Modified: time.Time{}})
	}
	return out, nil
}
```

- [ ] **Step 5: Rewrite `s3.go` as `S3Backend`**

Replace the entire contents of `internal/b2/s3.go` with:
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

const multipartPartSize = 100 * 1024 * 1024

// S3Backend talks to a B2 bucket via the S3-compatible API.
type S3Backend struct {
	client *s3.Client
	bucket string
}

func newS3Backend(ctx context.Context, cfg Config) (*S3Backend, error) {
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
		o.UsePathStyle = true
	})
	return &S3Backend{client: client, bucket: cfg.BucketName}, nil
}

func (u *S3Backend) Upload(ctx context.Context, key string, r io.Reader, size int64) error {
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

func (u *S3Backend) Exists(ctx context.Context, key string) (bool, error) {
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

func (u *S3Backend) Download(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	out, err := u.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(u.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		var nsk *types.NoSuchKey
		var nf *types.NotFound
		if errors.As(err, &nsk) || errors.As(err, &nf) {
			return nil, 0, fmt.Errorf("%w: %s", apperr.ErrObjectNotFound, key)
		}
		return nil, 0, fmt.Errorf("%w: get %s: %v", apperr.ErrDownloadFailed, key, err)
	}
	var size int64
	if out.ContentLength != nil {
		size = *out.ContentLength
	}
	return out.Body, size, nil
}

func (u *S3Backend) List(ctx context.Context, prefix string, recursive bool) (Listing, error) {
	var out Listing
	in := &s3.ListObjectsV2Input{
		Bucket: aws.String(u.bucket),
		Prefix: aws.String(prefix),
	}
	if !recursive {
		in.Delimiter = aws.String("/")
	}
	p := s3.NewListObjectsV2Paginator(u.client, in)
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return Listing{}, fmt.Errorf("%w: list %s: %v", apperr.ErrListFailed, prefix, err)
		}
		for _, obj := range page.Contents {
			info := ObjectInfo{Key: aws.ToString(obj.Key)}
			if obj.Size != nil {
				info.Size = *obj.Size
			}
			if obj.LastModified != nil {
				info.Modified = *obj.LastModified
			}
			out.Objects = append(out.Objects, info)
		}
		for _, cp := range page.CommonPrefixes {
			out.Prefixes = append(out.Prefixes, aws.ToString(cp.Prefix))
		}
	}
	return out, nil
}

func (u *S3Backend) Delete(ctx context.Context, key string) error {
	// Delete every version if the bucket is versioned; otherwise the single object.
	vp := s3.NewListObjectVersionsPaginator(u.client, &s3.ListObjectVersionsInput{
		Bucket: aws.String(u.bucket),
		Prefix: aws.String(key),
	})
	var ids []types.ObjectIdentifier
	for vp.HasMorePages() {
		page, err := vp.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("%w: list versions %s: %v", apperr.ErrDeleteFailed, key, err)
		}
		for _, v := range page.Versions {
			if aws.ToString(v.Key) == key {
				ids = append(ids, types.ObjectIdentifier{Key: v.Key, VersionId: v.VersionId})
			}
		}
		for _, dm := range page.DeleteMarkers {
			if aws.ToString(dm.Key) == key {
				ids = append(ids, types.ObjectIdentifier{Key: dm.Key, VersionId: dm.VersionId})
			}
		}
	}
	if len(ids) == 0 {
		// Fall back to a plain delete (covers non-versioned buckets that report no versions).
		_, err := u.client.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(u.bucket), Key: aws.String(key),
		})
		if err != nil {
			return fmt.Errorf("%w: delete %s: %v", apperr.ErrDeleteFailed, key, err)
		}
		return nil
	}
	_, err := u.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
		Bucket: aws.String(u.bucket),
		Delete: &types.Delete{Objects: ids, Quiet: aws.Bool(true)},
	})
	if err != nil {
		return fmt.Errorf("%w: delete versions %s: %v", apperr.ErrDeleteFailed, key, err)
	}
	return nil
}
```

- [ ] **Step 6: Update the `main.go` call site**

In `main.go`, the `runUpload` helper currently calls `b2.NewS3Uploader(...)` with `b2.Config{... Bucket: ...}`. Update it to use the renamed field and constructor temporarily (the full factory arrives in Task 6):
```go
	up, err := b2.NewBackend(ctx, "s3", b2.Config{
		Endpoint: cfg.Endpoint, Region: cfg.Region, BucketName: cfg.Bucket,
		KeyID: cfg.KeyID, AppKey: cfg.AppKey,
	})
```
(Keep the rest of `runUpload` the same; `cli.Upload` still takes a `b2.Uploader`, and a `b2.Backend` satisfies it.)

- [ ] **Step 7: Run b2 tests + full build**

Run: `go test ./internal/b2/ -v && go build ./... && go test ./...`
Expected: b2 tests PASS; whole module builds; all existing package tests still PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/b2/ main.go
git commit -m "feat: Backend interface with full S3Backend + FakeBackend"
```

---

## Task 3: store RemoteConfig + bucket_id/backend columns + migration

**Files:** Modify `internal/store/store.go`, `internal/store/store_test.go`, `internal/cli/cli.go`, `internal/cli/cli_test.go`, `main.go`

- [ ] **Step 1: Write failing store tests**

Add to `internal/store/store_test.go` (append these tests; keep existing ones):
```go
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
```
Add `"database/sql"` to the store_test.go import block if not already present.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/store/ -v`
Expected: FAIL — `RemoteConfig`, `GetBackend`, `SetBackend` undefined.

- [ ] **Step 3: Update `store.go`**

In `internal/store/store.go` make these changes:

3a. Rename the struct and add fields:
```go
// RemoteConfig is the decrypted destination configuration for either backend.
type RemoteConfig struct {
	Endpoint string
	Region   string
	Bucket   string // bucket name (S3 + B2 download-by-name)
	BucketID string // B2 native list/upload
	KeyID    string
	AppKey   string
}
```

3b. Update the `config` table in the `schema` constant to include the new columns (for fresh DBs):
```go
CREATE TABLE IF NOT EXISTS config (
  id           INTEGER PRIMARY KEY CHECK (id = 1),
  s3_endpoint  TEXT, s3_region TEXT, bucket_name TEXT,
  bucket_id    TEXT, backend TEXT,
  key_id_enc   BLOB, app_key_enc BLOB, created_at INTEGER
);
```

3c. In `Open`, after applying `schema`, run a migration for pre-existing DBs. Replace the body of `Open` with:
```go
func Open(ctx context.Context, path string, key []byte) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("%w: open: %v", apperr.ErrStore, err)
	}
	if _, err := db.ExecContext(ctx, schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("%w: migrate: %v", apperr.ErrStore, err)
	}
	if err := migrateConfigColumns(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, key: key}, nil
}

// migrateConfigColumns adds columns introduced after the initial schema to
// pre-existing config tables. SQLite has no "ADD COLUMN IF NOT EXISTS".
func migrateConfigColumns(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(config)`)
	if err != nil {
		return fmt.Errorf("%w: table_info: %v", apperr.ErrStore, err)
	}
	have := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return fmt.Errorf("%w: scan column: %v", apperr.ErrStore, err)
		}
		have[name] = true
	}
	rows.Close()
	for _, col := range []struct{ name, ddl string }{
		{"bucket_id", "ALTER TABLE config ADD COLUMN bucket_id TEXT"},
		{"backend", "ALTER TABLE config ADD COLUMN backend TEXT"},
	} {
		if have[col.name] {
			continue
		}
		if _, err := db.ExecContext(ctx, col.ddl); err != nil {
			return fmt.Errorf("%w: add column %s: %v", apperr.ErrStore, col.name, err)
		}
	}
	return nil
}
```

3d. Update `SaveConfig` to persist bucket_id (do NOT touch the `backend` column here — it's managed by SetBackend):
```go
func (s *Store) SaveConfig(ctx context.Context, cfg RemoteConfig) error {
	keyEnc, err := crypto.Seal(s.key, []byte(cfg.KeyID))
	if err != nil {
		return err
	}
	appEnc, err := crypto.Seal(s.key, []byte(cfg.AppKey))
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO config (id, s3_endpoint, s3_region, bucket_name, bucket_id, key_id_enc, app_key_enc, created_at)
		VALUES (1, ?, ?, ?, ?, ?, ?, strftime('%s','now'))
		ON CONFLICT(id) DO UPDATE SET
		  s3_endpoint=excluded.s3_endpoint, s3_region=excluded.s3_region,
		  bucket_name=excluded.bucket_name, bucket_id=excluded.bucket_id,
		  key_id_enc=excluded.key_id_enc, app_key_enc=excluded.app_key_enc`,
		cfg.Endpoint, cfg.Region, cfg.Bucket, cfg.BucketID, keyEnc, appEnc)
	if err != nil {
		return fmt.Errorf("%w: save config: %v", apperr.ErrStore, err)
	}
	return nil
}
```

3e. Update `GetConfig`'s return type and SELECT to include bucket_id:
```go
func (s *Store) GetConfig(ctx context.Context) (RemoteConfig, error) {
	var cfg RemoteConfig
	var keyEnc, appEnc []byte
	var bucketID sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT s3_endpoint, s3_region, bucket_name, bucket_id, key_id_enc, app_key_enc FROM config WHERE id=1`).
		Scan(&cfg.Endpoint, &cfg.Region, &cfg.Bucket, &bucketID, &keyEnc, &appEnc)
	if errors.Is(err, sql.ErrNoRows) {
		return RemoteConfig{}, apperr.ErrNotConfigured
	}
	if err != nil {
		return RemoteConfig{}, fmt.Errorf("%w: get config: %v", apperr.ErrStore, err)
	}
	cfg.BucketID = bucketID.String
	keyID, err := crypto.Open(s.key, keyEnc)
	if err != nil {
		return RemoteConfig{}, err
	}
	appKey, err := crypto.Open(s.key, appEnc)
	if err != nil {
		return RemoteConfig{}, err
	}
	cfg.KeyID, cfg.AppKey = string(keyID), string(appKey)
	return cfg, nil
}
```

3f. Add backend get/set methods (anywhere after GetConfig):
```go
// GetBackend returns the stored backend kind, defaulting to "s3".
func (s *Store) GetBackend(ctx context.Context) (string, error) {
	var backend sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT backend FROM config WHERE id=1`).Scan(&backend)
	if errors.Is(err, sql.ErrNoRows) || !backend.Valid || backend.String == "" {
		return "s3", nil
	}
	if err != nil {
		return "", fmt.Errorf("%w: get backend: %v", apperr.ErrStore, err)
	}
	return backend.String, nil
}

// SetBackend persists the backend kind ("s3" or "b2"). Requires existing config.
func (s *Store) SetBackend(ctx context.Context, kind string) error {
	if kind != "s3" && kind != "b2" {
		return apperr.ErrInvalidBackend
	}
	res, err := s.db.ExecContext(ctx, `UPDATE config SET backend=? WHERE id=1`, kind)
	if err != nil {
		return fmt.Errorf("%w: set backend: %v", apperr.ErrStore, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return apperr.ErrNotConfigured
	}
	return nil
}
```

- [ ] **Step 4: Update `cli.go` for the rename + bucket-ID prompt**

In `internal/cli/cli.go`:

4a. In `Init`, change the config struct type and add the bucket-ID prompt:
```go
	cfg := store.RemoteConfig{
		KeyID:    ask("Backblaze keyID (access key ID)"),
		AppKey:   ask("Backblaze applicationKey (secret)"),
		Bucket:   ask("Bucket name"),
		BucketID: ask("Bucket ID (for native B2 API)"),
		Endpoint: ask("S3 endpoint URL (e.g. https://s3.us-west-004.backblazeb2.com)"),
		Region:   ask("S3 region (e.g. us-west-004)"),
	}
```
(Keep the existing required-fields check; bucket ID is optional so do not add it to the required set.)

4b. In `Config`, after printing Bucket, also print the bucket ID and current backend:
```go
	fmt.Fprintf(out, "Bucket:      %s\n", cfg.Bucket)
	fmt.Fprintf(out, "Bucket ID:   %s\n", cfg.BucketID)
	backend, err := st.GetBackend(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Backend:     %s\n", backend)
```
(Place these right after the existing `Bucket:` line and before `Key ID:`.)

4c. In `Status` (configured branch), add the backend line:
```go
	backend, err := st.GetBackend(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Status: configured\nBackend: %s\nWatched folders: %d\nPending uploads: %d\n",
		backend, len(folders), pending)
```
(Replace the existing single `Fprintf` in the configured branch with this; remove the old one.)

- [ ] **Step 5: Update `cli_test.go` for the rename + new prompt**

In `internal/cli/cli_test.go`, in `TestInitThenConfigMasksSecret`, the `strings.NewReader` answer list must now include the bucket-ID answer in the correct order (after bucket name, before endpoint):
```go
	in := strings.NewReader(strings.Join([]string{
		"0001keyid",                              // keyID
		"K001-this-is-secret",                    // appKey
		"my-bucket",                              // bucket name
		"buck-et-id-123",                         // bucket ID  (NEW)
		"https://s3.us-west-004.backblazeb2.com", // endpoint
		"us-west-004",                            // region
		"",                                       // first folder (skip)
	}, "\n"))
```
No other cli test changes are required (handlers still take the same args).

- [ ] **Step 6: Update `main.go` runUpload field name**

`runUpload` builds `b2.Config` from the store config; `GetConfig` now returns `RemoteConfig` with the same `.Bucket`/`.BucketID` fields. Update the mapping to pass the bucket id too:
```go
	up, err := b2.NewBackend(ctx, "s3", b2.Config{
		Endpoint: cfg.Endpoint, Region: cfg.Region,
		BucketName: cfg.Bucket, BucketID: cfg.BucketID,
		KeyID: cfg.KeyID, AppKey: cfg.AppKey,
	})
```

- [ ] **Step 7: Run tests + build**

Run: `go test ./internal/store/ ./internal/cli/ -v && go build ./... && go test ./...`
Expected: all PASS, whole module builds.

- [ ] **Step 8: Commit**

```bash
git add internal/store/ internal/cli/cli.go internal/cli/cli_test.go main.go
git commit -m "feat: RemoteConfig with bucket ID + backend mode (stored, migrated)"
```

---

## Task 4: B2Backend native client (auth, small upload, download, list, delete, search)

**Files:** Create `internal/b2/native.go`, `internal/b2/native_test.go`

This task implements everything EXCEPT large-file upload (Task 7). Uploads larger than
`b2SmallFileLimit` return `ErrUploadFailed` with a clear "not yet implemented" message until
Task 7 lands.

- [ ] **Step 1: Write failing httptest-based tests**

Create `internal/b2/native_test.go`:
```go
package b2

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"backuprepo/internal/apperr"
)

// b2TestServer simulates the subset of the B2 v2 API the client uses.
func b2TestServer(t *testing.T, store map[string][]byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var base string

	mux.HandleFunc("/b2api/v2/b2_authorize_account", func(w http.ResponseWriter, r *http.Request) {
		if _, _, ok := r.BasicAuth(); !ok {
			w.WriteHeader(401)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"apiUrl":              base,
			"downloadUrl":         base,
			"authorizationToken":  "test-token",
			"recommendedPartSize": 100000000,
		})
	})
	mux.HandleFunc("/b2api/v2/b2_get_upload_url", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"uploadUrl":          base + "/upload",
			"authorizationToken": "upload-token",
		})
	})
	mux.HandleFunc("/upload", func(w http.ResponseWriter, r *http.Request) {
		name := r.Header.Get("X-Bz-File-Name")
		body, _ := io.ReadAll(r.Body)
		store[name] = body
		json.NewEncoder(w).Encode(map[string]any{"fileName": name, "fileId": "id-" + name})
	})
	mux.HandleFunc("/b2api/v2/b2_list_file_names", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Prefix, Delimiter, StartFileName string
		}
		json.NewDecoder(r.Body).Decode(&req)
		type f struct {
			FileName        string `json:"fileName"`
			ContentLength   int64  `json:"contentLength"`
			UploadTimestamp int64  `json:"uploadTimestamp"`
			Action          string `json:"action"`
		}
		var files []f
		for name, data := range store {
			if strings.HasPrefix(name, req.Prefix) {
				files = append(files, f{FileName: name, ContentLength: int64(len(data)), Action: "upload"})
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"files": files, "nextFileName": nil})
	})
	mux.HandleFunc("/b2api/v2/b2_list_file_versions", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Prefix string }
		json.NewDecoder(r.Body).Decode(&req)
		type f struct {
			FileName string `json:"fileName"`
			FileID   string `json:"fileId"`
		}
		var files []f
		for name := range store {
			if name == req.Prefix {
				files = append(files, f{FileName: name, FileID: "id-" + name})
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"files": files, "nextFileName": nil, "nextFileId": nil})
	})
	mux.HandleFunc("/b2api/v2/b2_delete_file_version", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ FileName, FileId string }
		json.NewDecoder(r.Body).Decode(&req)
		delete(store, req.FileName)
		json.NewEncoder(w).Encode(map[string]any{"fileName": req.FileName, "fileId": req.FileId})
	})
	mux.HandleFunc("/file/", func(w http.ResponseWriter, r *http.Request) {
		// path: /file/{bucketName}/{key...}
		parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/file/"), "/", 2)
		if len(parts) != 2 {
			w.WriteHeader(404)
			return
		}
		data, ok := store[parts[1]]
		if !ok {
			w.WriteHeader(404)
			return
		}
		w.Write(data)
	})

	srv := httptest.NewServer(mux)
	base = srv.URL
	t.Cleanup(srv.Close)
	return srv
}

func testB2(t *testing.T, srv *httptest.Server) *B2Backend {
	t.Helper()
	b := newB2Backend(Config{
		BucketName: "my-bucket", BucketID: "bid", KeyID: "k", AppKey: "a",
	})
	b.authURL = srv.URL
	b.http = srv.Client()
	return b
}

func TestB2UploadDownload(t *testing.T) {
	store := map[string][]byte{}
	srv := b2TestServer(t, store)
	b := testB2(t, srv)
	ctx := context.Background()

	if err := b.Upload(ctx, "dir/file.txt", bytes.NewReader([]byte("hello b2")), 8); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if string(store["dir/file.txt"]) != "hello b2" {
		t.Fatalf("server stored %q", store["dir/file.txt"])
	}
	rc, n, err := b.Download(ctx, "dir/file.txt")
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "hello b2" || n != 8 {
		t.Fatalf("download mismatch %q n=%d", got, n)
	}
}

func TestB2DownloadMissing(t *testing.T) {
	srv := b2TestServer(t, map[string][]byte{})
	b := testB2(t, srv)
	if _, _, err := b.Download(context.Background(), "nope"); !errors.Is(err, apperr.ErrObjectNotFound) {
		t.Fatalf("want ErrObjectNotFound, got %v", err)
	}
}

func TestB2ListAndDelete(t *testing.T) {
	store := map[string][]byte{"a.txt": []byte("1"), "p/b.txt": []byte("2"), "p/c.txt": []byte("3")}
	srv := b2TestServer(t, store)
	b := testB2(t, srv)
	ctx := context.Background()

	l, err := b.List(ctx, "p/", true)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(l.Objects) != 2 {
		t.Fatalf("recursive list under p/ = %d objects, want 2", len(l.Objects))
	}
	if err := b.Delete(ctx, "p/b.txt"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := store["p/b.txt"]; ok {
		t.Fatal("p/b.txt should be deleted")
	}
}

func TestB2LargeFileNotYetImplemented(t *testing.T) {
	srv := b2TestServer(t, map[string][]byte{})
	b := testB2(t, srv)
	big := bytes.NewReader(make([]byte, 1)) // size arg drives the branch
	err := b.Upload(context.Background(), "big.bin", big, b2SmallFileLimit+1)
	if !errors.Is(err, apperr.ErrUploadFailed) {
		t.Fatalf("want ErrUploadFailed for oversize, got %v", err)
	}
}

var _ Backend = (*B2Backend)(nil)
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/b2/ -run TestB2 -v`
Expected: build failure — `B2Backend`, `newB2Backend`, `b2SmallFileLimit` undefined.

- [ ] **Step 3: Implement `native.go`**

Create `internal/b2/native.go`:
```go
package b2

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"backuprepo/internal/apperr"
)

// b2SmallFileLimit is the threshold above which uploads must use the large-file
// (multipart) API. Files at or below this size use a single b2_upload_file call.
const b2SmallFileLimit = 100 * 1024 * 1024

const defaultB2AuthURL = "https://api.backblazeb2.com"

// B2Backend talks to Backblaze via the native B2 v2 API.
type B2Backend struct {
	cfg     Config
	http    *http.Client
	authURL string
	auth    *b2Auth
}

type b2Auth struct {
	APIURL              string
	DownloadURL         string
	Token               string
	RecommendedPartSize int64
}

func newB2Backend(cfg Config) *B2Backend {
	return &B2Backend{cfg: cfg, http: http.DefaultClient, authURL: defaultB2AuthURL}
}

// authorize fetches and caches an auth context.
func (b *B2Backend) authorize(ctx context.Context) (*b2Auth, error) {
	if b.auth != nil {
		return b.auth, nil
	}
	if b.cfg.KeyID == "" || b.cfg.AppKey == "" {
		return nil, apperr.ErrInvalidCredentials
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		b.authURL+"/b2api/v2/b2_authorize_account", nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", apperr.ErrAuthFailed, err)
	}
	req.SetBasicAuth(b.cfg.KeyID, b.cfg.AppKey)
	resp, err := b.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", apperr.ErrAuthFailed, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: status %d", apperr.ErrAuthFailed, resp.StatusCode)
	}
	var out struct {
		APIURL              string `json:"apiUrl"`
		DownloadURL         string `json:"downloadUrl"`
		AuthorizationToken  string `json:"authorizationToken"`
		RecommendedPartSize int64  `json:"recommendedPartSize"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("%w: decode: %v", apperr.ErrAuthFailed, err)
	}
	b.auth = &b2Auth{
		APIURL: out.APIURL, DownloadURL: out.DownloadURL,
		Token: out.AuthorizationToken, RecommendedPartSize: out.RecommendedPartSize,
	}
	return b.auth, nil
}

// postJSON calls a B2 API endpoint with a JSON body and the auth token.
func (b *B2Backend) postJSON(ctx context.Context, auth *b2Auth, endpoint string, body, out any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		auth.APIURL+"/b2api/v2/"+endpoint, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", auth.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Upload stores key. Small files go through b2_upload_file; larger files use the
// large-file API (see largefile.go).
func (b *B2Backend) Upload(ctx context.Context, key string, r io.Reader, size int64) error {
	auth, err := b.authorize(ctx)
	if err != nil {
		return err
	}
	if size > b2SmallFileLimit {
		return b.uploadLarge(ctx, auth, key, r, size)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("%w: read %s: %v", apperr.ErrUploadFailed, key, err)
	}

	var up struct {
		UploadURL          string `json:"uploadUrl"`
		AuthorizationToken string `json:"authorizationToken"`
	}
	if err := b.postJSON(ctx, auth, "b2_get_upload_url",
		map[string]string{"bucketId": b.cfg.BucketID}, &up); err != nil {
		return fmt.Errorf("%w: get_upload_url: %v", apperr.ErrUploadFailed, err)
	}

	sum := sha1.Sum(data)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, up.UploadURL, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("%w: %v", apperr.ErrUploadFailed, err)
	}
	req.Header.Set("Authorization", up.AuthorizationToken)
	req.Header.Set("X-Bz-File-Name", encodeFileName(key))
	req.Header.Set("Content-Type", "b2/x-auto")
	req.Header.Set("X-Bz-Content-Sha1", hex.EncodeToString(sum[:]))
	req.Header.Set("Content-Length", strconv.Itoa(len(data)))
	resp, err := b.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: upload %s: %v", apperr.ErrUploadFailed, key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: upload %s: status %d", apperr.ErrUploadFailed, key, resp.StatusCode)
	}
	return nil
}

// Download streams key by name from the download URL.
func (b *B2Backend) Download(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	auth, err := b.authorize(ctx)
	if err != nil {
		return nil, 0, err
	}
	url := auth.DownloadURL + "/file/" + b.cfg.BucketName + "/" + encodeFileName(key)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %v", apperr.ErrDownloadFailed, err)
	}
	req.Header.Set("Authorization", auth.Token)
	resp, err := b.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %v", apperr.ErrDownloadFailed, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, 0, fmt.Errorf("%w: %s", apperr.ErrObjectNotFound, key)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, 0, fmt.Errorf("%w: status %d", apperr.ErrDownloadFailed, resp.StatusCode)
	}
	return resp.Body, resp.ContentLength, nil
}

// Exists reports whether key is present (via a single-name list).
func (b *B2Backend) Exists(ctx context.Context, key string) (bool, error) {
	l, err := b.listRaw(ctx, key, "", 1)
	if err != nil {
		return false, err
	}
	for _, o := range l {
		if o.Key == key {
			return true, nil
		}
	}
	return false, nil
}

type b2File struct {
	FileName        string `json:"fileName"`
	ContentLength   int64  `json:"contentLength"`
	UploadTimestamp int64  `json:"uploadTimestamp"`
	Action          string `json:"action"`
}

// listRaw returns objects under prefix (recursive, no delimiter), handling pagination.
func (b *B2Backend) listRaw(ctx context.Context, prefix, delimiter string, max int) ([]ObjectInfo, error) {
	auth, err := b.authorize(ctx)
	if err != nil {
		return nil, err
	}
	var out []ObjectInfo
	start := ""
	for {
		var resp struct {
			Files        []b2File `json:"files"`
			NextFileName *string  `json:"nextFileName"`
		}
		body := map[string]any{
			"bucketId":      b.cfg.BucketID,
			"prefix":        prefix,
			"startFileName": start,
			"maxFileCount":  1000,
		}
		if delimiter != "" {
			body["delimiter"] = delimiter
		}
		if err := b.postJSON(ctx, auth, "b2_list_file_names", body, &resp); err != nil {
			return nil, fmt.Errorf("%w: list %s: %v", apperr.ErrListFailed, prefix, err)
		}
		for _, f := range resp.Files {
			out = append(out, ObjectInfo{
				Key:      f.FileName,
				Size:     f.ContentLength,
				Modified: msToTime(f.UploadTimestamp),
			})
			if max > 0 && len(out) >= max {
				return out, nil
			}
		}
		if resp.NextFileName == nil || *resp.NextFileName == "" {
			return out, nil
		}
		start = *resp.NextFileName
	}
}

// List groups results folder-like when not recursive.
func (b *B2Backend) List(ctx context.Context, prefix string, recursive bool) (Listing, error) {
	delim := "/"
	if recursive {
		delim = ""
	}
	raw, err := b.listRaw(ctx, prefix, delim, 0)
	if err != nil {
		return Listing{}, err
	}
	var out Listing
	for _, o := range raw {
		// With a delimiter, B2 returns folder entries as keys ending in "/".
		if !recursive && strings.HasSuffix(o.Key, "/") {
			out.Prefixes = append(out.Prefixes, o.Key)
			continue
		}
		out.Objects = append(out.Objects, o)
	}
	return out, nil
}

// Delete removes all versions of key.
func (b *B2Backend) Delete(ctx context.Context, key string) error {
	auth, err := b.authorize(ctx)
	if err != nil {
		return err
	}
	type ver struct {
		FileName string `json:"fileName"`
		FileID   string `json:"fileId"`
	}
	var found int
	start, startID := "", ""
	for {
		var resp struct {
			Files        []ver   `json:"files"`
			NextFileName *string `json:"nextFileName"`
			NextFileID   *string `json:"nextFileId"`
		}
		body := map[string]any{
			"bucketId":      b.cfg.BucketID,
			"prefix":        key,
			"startFileName": start,
			"startFileId":   startID,
			"maxFileCount":  1000,
		}
		if err := b.postJSON(ctx, auth, "b2_list_file_versions", body, &resp); err != nil {
			return fmt.Errorf("%w: list versions %s: %v", apperr.ErrDeleteFailed, key, err)
		}
		for _, v := range resp.Files {
			if v.FileName != key {
				continue
			}
			found++
			if err := b.postJSON(ctx, auth, "b2_delete_file_version",
				map[string]string{"fileName": v.FileName, "fileId": v.FileID}, nil); err != nil {
				return fmt.Errorf("%w: delete %s: %v", apperr.ErrDeleteFailed, key, err)
			}
		}
		if resp.NextFileName == nil || *resp.NextFileName == "" {
			break
		}
		start = *resp.NextFileName
		if resp.NextFileID != nil {
			startID = *resp.NextFileID
		}
	}
	if found == 0 {
		return fmt.Errorf("%w: %s", apperr.ErrObjectNotFound, key)
	}
	return nil
}

// encodeFileName percent-encodes a B2 file name while preserving path slashes.
func encodeFileName(name string) string {
	segs := strings.Split(name, "/")
	for i, s := range segs {
		segs[i] = urlEncodeSegment(s)
	}
	return strings.Join(segs, "/")
}

// urlEncodeSegment encodes a single path segment per RFC 3986 (no slash).
func urlEncodeSegment(s string) string {
	const upper = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(upper[c>>4])
		b.WriteByte(upper[c&0xF])
	}
	return b.String()
}
```

- [ ] **Step 4: Add the `msToTime` helper**

Add to the bottom of `internal/b2/native.go`:
```go
import "time" // merge into the existing import block above, do not add a second import line

func msToTime(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}
```
> Merge `"time"` into the single import block at the top of native.go.

- [ ] **Step 5: Stub `uploadLarge` so the package compiles before Task 7**

Create `internal/b2/largefile.go`:
```go
package b2

import (
	"context"
	"fmt"
	"io"

	"backuprepo/internal/apperr"
)

// uploadLarge handles files above b2SmallFileLimit via the B2 large-file API.
// Implemented in Task 7; until then it returns a clear error rather than
// silently truncating.
func (b *B2Backend) uploadLarge(ctx context.Context, auth *b2Auth, key string, r io.Reader, size int64) error {
	return fmt.Errorf("%w: large-file B2 upload not yet implemented (%s, %d bytes)", apperr.ErrUploadFailed, key, size)
}
```

- [ ] **Step 6: Run b2 tests + full build**

Run: `go test ./internal/b2/ -v && go build ./... && go test ./...`
Expected: all b2 tests PASS (including the not-yet-implemented large-file assertion); module builds; all packages pass.

- [ ] **Step 7: Commit**

```bash
git add internal/b2/native.go internal/b2/largefile.go internal/b2/native_test.go
git commit -m "feat: native B2 backend (auth, small upload, download, list, delete)"
```

---

## Task 5: CLI file-operation handlers

**Files:** Create `internal/cli/files.go`, `internal/cli/files_test.go`

- [ ] **Step 1: Write failing handler tests**

Create `internal/cli/files_test.go`:
```go
package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"backuprepo/internal/b2"
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
```
Add this helper to `internal/cli/files_test.go` (used above):
```go
import "backuprepo/internal/store" // merge into the import block above

func saveMinimalConfig(t *testing.T, st *store.Store) {
	t.Helper()
	err := st.SaveConfig(context.Background(), store.RemoteConfig{
		Bucket: "b", BucketID: "id", KeyID: "k", AppKey: "a",
	})
	if err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
}
```
> Merge `"backuprepo/internal/store"` into the single import block. `newStore` already exists in `cli_test.go` (same package), so reuse it.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/cli/ -run 'TestLs|TestFind|TestGet|TestPut|TestRm|TestBackend' -v`
Expected: FAIL — handlers undefined.

- [ ] **Step 3: Implement `files.go`**

Create `internal/cli/files.go`:
```go
// CLI handlers for manual bucket operations (ls/get/put/rm/find/backend).
// Each takes a b2.Backend (and io for prompts/output) so they are testable.
package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"backuprepo/internal/b2"
	"backuprepo/internal/store"
)

// remoteKey normalizes a user-supplied path to a bucket key (forward slashes,
// no leading slash).
func remoteKey(p string) string {
	return strings.TrimPrefix(filepath.ToSlash(p), "/")
}

// Ls lists a prefix. Folders (common prefixes) are shown with a trailing slash.
func Ls(ctx context.Context, be b2.Backend, path string, recursive bool, out io.Writer) error {
	prefix := path
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	prefix = strings.TrimPrefix(filepath.ToSlash(prefix), "/")
	l, err := be.List(ctx, prefix, recursive)
	if err != nil {
		return err
	}
	for _, p := range l.Prefixes {
		fmt.Fprintf(out, "%s\n", p)
	}
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	for _, o := range l.Objects {
		fmt.Fprintf(tw, "%s\t%d\n", o.Key, o.Size)
	}
	tw.Flush()
	if len(l.Prefixes) == 0 && len(l.Objects) == 0 {
		fmt.Fprintln(out, "(empty)")
	}
	return nil
}

// Find prints keys (optionally under prefix) whose name contains query (case-insensitive).
func Find(ctx context.Context, be b2.Backend, query, prefix string, out io.Writer) error {
	prefix = strings.TrimPrefix(filepath.ToSlash(prefix), "/")
	l, err := be.List(ctx, prefix, true)
	if err != nil {
		return err
	}
	q := strings.ToLower(query)
	n := 0
	for _, o := range l.Objects {
		if strings.Contains(strings.ToLower(o.Key), q) {
			fmt.Fprintf(out, "%s\n", o.Key)
			n++
		}
	}
	fmt.Fprintf(out, "%d match(es)\n", n)
	return nil
}

// Get downloads a single object, or (with recursive) every object under a prefix.
func Get(ctx context.Context, be b2.Backend, remote, local string, recursive bool, out io.Writer) error {
	key := remoteKey(remote)
	if recursive {
		prefix := key
		if prefix != "" && !strings.HasSuffix(prefix, "/") {
			prefix += "/"
		}
		l, err := be.List(ctx, prefix, true)
		if err != nil {
			return err
		}
		base := local
		if base == "" {
			base = "."
		}
		for _, o := range l.Objects {
			rel := strings.TrimPrefix(o.Key, prefix)
			dest := filepath.Join(base, filepath.FromSlash(rel))
			if err := downloadTo(ctx, be, o.Key, dest); err != nil {
				return err
			}
			fmt.Fprintf(out, "downloaded %s\n", o.Key)
		}
		return nil
	}
	dest := local
	if dest == "" {
		dest = filepath.Base(filepath.FromSlash(key))
	}
	if err := downloadTo(ctx, be, key, dest); err != nil {
		return err
	}
	fmt.Fprintf(out, "downloaded %s -> %s\n", key, dest)
	return nil
}

func downloadTo(ctx context.Context, be b2.Backend, key, dest string) error {
	if dir := filepath.Dir(dest); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	rc, _, err := be.Download(ctx, key)
	if err != nil {
		return err
	}
	defer rc.Close()
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, rc)
	return err
}

// Put uploads a single file, or (with recursive) every file under a local directory.
func Put(ctx context.Context, be b2.Backend, local, remote string, recursive bool, out io.Writer) error {
	info, err := os.Stat(local)
	if err != nil {
		return err
	}
	if info.IsDir() {
		if !recursive {
			return fmt.Errorf("%s is a directory (use -r)", local)
		}
		prefix := remoteKey(remote)
		if prefix != "" && !strings.HasSuffix(prefix, "/") {
			prefix += "/"
		}
		return filepath.WalkDir(local, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(local, p)
			key := prefix + filepath.ToSlash(rel)
			if err := uploadFrom(ctx, be, p, key); err != nil {
				return err
			}
			fmt.Fprintf(out, "uploaded %s\n", key)
			return nil
		})
	}
	key := remoteKey(remote)
	if key == "" {
		key = filepath.Base(local)
	}
	if err := uploadFrom(ctx, be, local, key); err != nil {
		return err
	}
	fmt.Fprintf(out, "uploaded %s -> %s\n", local, key)
	return nil
}

func uploadFrom(ctx context.Context, be b2.Backend, local, key string) error {
	f, err := os.Open(local)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	return be.Upload(ctx, key, f, info.Size())
}

// Rm deletes an object, or (with recursive) every object under a prefix. Prompts
// for confirmation unless force is set.
func Rm(ctx context.Context, be b2.Backend, path string, recursive, force bool, in io.Reader, out io.Writer) error {
	if recursive {
		prefix := remoteKey(path)
		if prefix != "" && !strings.HasSuffix(prefix, "/") {
			prefix += "/"
		}
		l, err := be.List(ctx, prefix, true)
		if err != nil {
			return err
		}
		if len(l.Objects) == 0 {
			fmt.Fprintln(out, "nothing to delete")
			return nil
		}
		if !force && !confirm(in, out, fmt.Sprintf("Delete %d object(s) under %q?", len(l.Objects), prefix)) {
			fmt.Fprintln(out, "aborted")
			return nil
		}
		for _, o := range l.Objects {
			if err := be.Delete(ctx, o.Key); err != nil {
				return err
			}
			fmt.Fprintf(out, "deleted %s\n", o.Key)
		}
		return nil
	}
	key := remoteKey(path)
	if !force && !confirm(in, out, fmt.Sprintf("Delete %q?", key)) {
		fmt.Fprintln(out, "aborted")
		return nil
	}
	if err := be.Delete(ctx, key); err != nil {
		return err
	}
	fmt.Fprintf(out, "deleted %s\n", key)
	return nil
}

func confirm(in io.Reader, out io.Writer, prompt string) bool {
	fmt.Fprintf(out, "%s [y/N]: ", prompt)
	line, _ := bufio.NewReader(in).ReadString('\n')
	line = strings.TrimSpace(strings.ToLower(line))
	return line == "y" || line == "yes"
}

// Backend prints (no arg) or sets the stored backend mode.
func Backend(ctx context.Context, st *store.Store, kind string, out io.Writer) error {
	if kind == "" {
		cur, err := st.GetBackend(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Backend: %s\n", cur)
		return nil
	}
	if err := st.SetBackend(ctx, kind); err != nil {
		return err
	}
	fmt.Fprintf(out, "Backend set to %s\n", kind)
	return nil
}
```

- [ ] **Step 4: Run tests + build**

Run: `go test ./internal/cli/ -v && go build ./...`
Expected: all cli tests PASS; module builds.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/files.go internal/cli/files_test.go
git commit -m "feat: manual file commands (ls/get/put/rm/find/backend)"
```

---

## Task 6: main.go dispatch + flags + effective-backend factory

**Files:** Modify `main.go`

- [ ] **Step 1: Add a backend builder + flag parsing helpers**

In `main.go`, add these helpers (after `runUpload`):
```go
// effectiveBackend resolves the backend kind: flag override → stored → "s3".
func effectiveBackend(ctx context.Context, st *store.Store, override string) (string, error) {
	if override != "" {
		if override != "s3" && override != "b2" {
			return "", apperr.ErrInvalidBackend
		}
		return override, nil
	}
	return st.GetBackend(ctx)
}

// buildBackend constructs the selected backend from stored config.
func buildBackend(ctx context.Context, st *store.Store, override string) (b2.Backend, error) {
	kind, err := effectiveBackend(ctx, st, override)
	if err != nil {
		return nil, err
	}
	cfg, err := st.GetConfig(ctx)
	if err != nil {
		return nil, err
	}
	return b2.NewBackend(ctx, kind, b2.Config{
		Endpoint: cfg.Endpoint, Region: cfg.Region,
		BucketName: cfg.Bucket, BucketID: cfg.BucketID,
		KeyID: cfg.KeyID, AppKey: cfg.AppKey,
	})
}
```
Add `"backuprepo/internal/apperr"` to main.go's import block.

- [ ] **Step 2: Replace `runUpload` to honor the stored/overridden backend**

Replace `runUpload` with a version that builds via the factory (default override empty so it uses the stored mode):
```go
func runUpload(ctx context.Context, st *store.Store) error {
	up, err := buildBackend(ctx, st, "")
	if err != nil {
		return err
	}
	return cli.Upload(ctx, st, up, os.Stdout)
}
```

- [ ] **Step 3: Add subcommand dispatch with per-command flags**

In `run`, add these cases to the `switch cmd` (before `default`). Each uses a `flag.FlagSet`:
```go
	case "ls":
		fs := flag.NewFlagSet("ls", flag.ContinueOnError)
		recursive := fs.Bool("r", false, "recursive")
		backend := fs.String("backend", "", "override backend (s3|b2)")
		if err = fs.Parse(rest); err != nil {
			return 1
		}
		be, e := buildBackend(ctx, st, *backend)
		if e != nil {
			return fail(e)
		}
		err = cli.Ls(ctx, be, fs.Arg(0), *recursive, os.Stdout)
	case "find":
		fs := flag.NewFlagSet("find", flag.ContinueOnError)
		backend := fs.String("backend", "", "override backend (s3|b2)")
		if err = fs.Parse(rest); err != nil {
			return 1
		}
		if fs.NArg() < 1 {
			return fail(fmt.Errorf("usage: backuprepo find <query> [prefix]"))
		}
		be, e := buildBackend(ctx, st, *backend)
		if e != nil {
			return fail(e)
		}
		err = cli.Find(ctx, be, fs.Arg(0), fs.Arg(1), os.Stdout)
	case "get":
		fs := flag.NewFlagSet("get", flag.ContinueOnError)
		recursive := fs.Bool("r", false, "recursive")
		backend := fs.String("backend", "", "override backend (s3|b2)")
		if err = fs.Parse(rest); err != nil {
			return 1
		}
		if fs.NArg() < 1 {
			return fail(fmt.Errorf("usage: backuprepo get <remote> [local] [-r]"))
		}
		be, e := buildBackend(ctx, st, *backend)
		if e != nil {
			return fail(e)
		}
		err = cli.Get(ctx, be, fs.Arg(0), fs.Arg(1), *recursive, os.Stdout)
	case "put":
		fs := flag.NewFlagSet("put", flag.ContinueOnError)
		recursive := fs.Bool("r", false, "recursive")
		backend := fs.String("backend", "", "override backend (s3|b2)")
		if err = fs.Parse(rest); err != nil {
			return 1
		}
		if fs.NArg() < 1 {
			return fail(fmt.Errorf("usage: backuprepo put <local> [remote] [-r]"))
		}
		be, e := buildBackend(ctx, st, *backend)
		if e != nil {
			return fail(e)
		}
		err = cli.Put(ctx, be, fs.Arg(0), fs.Arg(1), *recursive, os.Stdout)
	case "rm":
		fs := flag.NewFlagSet("rm", flag.ContinueOnError)
		recursive := fs.Bool("r", false, "recursive")
		force := fs.Bool("f", false, "skip confirmation")
		fs.BoolVar(force, "y", false, "skip confirmation")
		backend := fs.String("backend", "", "override backend (s3|b2)")
		if err = fs.Parse(rest); err != nil {
			return 1
		}
		if fs.NArg() < 1 {
			return fail(fmt.Errorf("usage: backuprepo rm <path> [-r] [-f]"))
		}
		be, e := buildBackend(ctx, st, *backend)
		if e != nil {
			return fail(e)
		}
		err = cli.Rm(ctx, be, fs.Arg(0), *recursive, *force, os.Stdin, os.Stdout)
	case "backend":
		kind := ""
		if len(rest) > 0 {
			kind = rest[0]
		}
		err = cli.Backend(ctx, st, kind, os.Stdout)
```
Add `"flag"` to main.go's import block.

- [ ] **Step 4: Extend the usage text**

In the `usage` function, add the new commands to the printed help (append before the closing backtick):
```go
  backuprepo ls [path] [-r]       List bucket contents (folders shown with trailing /)
  backuprepo get <remote> [local] [-r]   Download an object or (with -r) a folder
  backuprepo put <local> [remote] [-r]   Upload a file or (with -r) a directory
  backuprepo rm <path> [-r] [-f]  Delete an object/folder (confirms unless -f)
  backuprepo find <query> [prefix]  Search object names (substring, case-insensitive)
  backuprepo backend [s3|b2]      Show or set the storage backend
```

- [ ] **Step 5: Build, vet, full test, smoke test**

Run:
```bash
go build ./... && go vet ./... && go test ./... && gofmt -l .
go build -ldflags="-s -w" -o /tmp/br_b .
HOME=$(mktemp -d) /tmp/br_b backend
```
Expected: build/vet/test clean; `gofmt -l` empty; `backend` prints `Backend: s3` (default) and exits 0.

- [ ] **Step 6: Commit**

```bash
git add main.go
git commit -m "feat: dispatch ls/get/put/rm/find/backend with --backend and flags"
```

---

## Task 7: B2 large-file (multipart) upload

**Files:** Modify `internal/b2/largefile.go`, `internal/b2/native_test.go`

- [ ] **Step 1: Extend the B2 test server with large-file endpoints**

In `internal/b2/native_test.go`, add these handlers inside `b2TestServer` (before `srv := httptest.NewServer(mux)`):
```go
	mux.HandleFunc("/b2api/v2/b2_start_large_file", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ FileName string `json:"fileName"` }
		json.NewDecoder(r.Body).Decode(&req)
		json.NewEncoder(w).Encode(map[string]any{"fileId": "large-" + req.FileName, "fileName": req.FileName})
	})
	mux.HandleFunc("/b2api/v2/b2_get_upload_part_url", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"uploadUrl": base + "/uploadpart", "authorizationToken": "part-token"})
	})
	largeParts := map[string][][]byte{}
	mux.HandleFunc("/uploadpart", func(w http.ResponseWriter, r *http.Request) {
		fileID := r.Header.Get("X-Bz-Part-File-Id")
		body, _ := io.ReadAll(r.Body)
		largeParts[fileID] = append(largeParts[fileID], body)
		json.NewEncoder(w).Encode(map[string]any{"partNumber": len(largeParts[fileID])})
	})
	mux.HandleFunc("/b2api/v2/b2_finish_large_file", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			FileID string `json:"fileId"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		name := strings.TrimPrefix(req.FileID, "large-")
		var full []byte
		for _, p := range largeParts[req.FileID] {
			full = append(full, p...)
		}
		store[name] = full
		json.NewEncoder(w).Encode(map[string]any{"fileName": name, "fileId": req.FileID})
	})
```
Note: the test client must send the file id on each part so the stub can group parts. The
implementation sets header `X-Bz-Part-File-Id` (test-only convenience header) in addition to the
real B2 headers.

Then add this test:
```go
func TestB2LargeFileUpload(t *testing.T) {
	store := map[string][]byte{}
	srv := b2TestServer(t, store)
	b := testB2(t, srv)
	b.partSize = 5 // tiny parts so a small payload exercises the multipart path
	ctx := context.Background()

	payload := []byte("abcdefghijklmnop") // 16 bytes → 4 parts of 5,5,5,1
	err := b.uploadLarge(ctx, mustAuth(t, b, ctx), "big.bin", bytes.NewReader(payload), int64(len(payload)))
	if err != nil {
		t.Fatalf("uploadLarge: %v", err)
	}
	if string(store["big.bin"]) != string(payload) {
		t.Fatalf("reassembled = %q want %q", store["big.bin"], payload)
	}
}

func mustAuth(t *testing.T, b *B2Backend, ctx context.Context) *b2Auth {
	t.Helper()
	a, err := b.authorize(ctx)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	return a
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/b2/ -run TestB2LargeFileUpload -v`
Expected: FAIL — `b.partSize` undefined and `uploadLarge` returns the not-implemented error.

- [ ] **Step 3: Add a configurable part size to B2Backend**

In `internal/b2/native.go`, add a `partSize int64` field to the `B2Backend` struct and default it in `newB2Backend`:
```go
type B2Backend struct {
	cfg      Config
	http     *http.Client
	authURL  string
	auth     *b2Auth
	partSize int64
}

func newB2Backend(cfg Config) *B2Backend {
	return &B2Backend{cfg: cfg, http: http.DefaultClient, authURL: defaultB2AuthURL, partSize: b2SmallFileLimit}
}
```

- [ ] **Step 4: Implement `uploadLarge`**

Replace the body of `internal/b2/largefile.go` with:
```go
package b2

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"backuprepo/internal/apperr"
)

// uploadLarge uploads key in parts via the B2 large-file API.
func (b *B2Backend) uploadLarge(ctx context.Context, auth *b2Auth, key string, r io.Reader, size int64) error {
	part := b.partSize
	if part <= 0 {
		part = b2SmallFileLimit
	}

	// 1. Start the large file.
	var start struct {
		FileID string `json:"fileId"`
	}
	if err := b.postJSON(ctx, auth, "b2_start_large_file", map[string]string{
		"bucketId": b.cfg.BucketID, "fileName": encodeFileName(key), "contentType": "b2/x-auto",
	}, &start); err != nil {
		return fmt.Errorf("%w: start_large_file %s: %v", apperr.ErrUploadFailed, key, err)
	}

	// 2. Upload each part, collecting SHA-1s.
	var shas []string
	buf := make([]byte, part)
	partNum := 0
	for {
		n, readErr := io.ReadFull(r, buf)
		if n > 0 {
			partNum++
			sha, err := b.uploadPart(ctx, auth, start.FileID, partNum, buf[:n])
			if err != nil {
				return err
			}
			shas = append(shas, sha)
		}
		if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
			break
		}
		if readErr != nil {
			return fmt.Errorf("%w: read %s: %v", apperr.ErrUploadFailed, key, readErr)
		}
	}

	// 3. Finish.
	if err := b.postJSON(ctx, auth, "b2_finish_large_file", map[string]any{
		"fileId": start.FileID, "partSha1Array": shas,
	}, nil); err != nil {
		return fmt.Errorf("%w: finish_large_file %s: %v", apperr.ErrUploadFailed, key, err)
	}
	return nil
}

func (b *B2Backend) uploadPart(ctx context.Context, auth *b2Auth, fileID string, num int, data []byte) (string, error) {
	var up struct {
		UploadURL          string `json:"uploadUrl"`
		AuthorizationToken string `json:"authorizationToken"`
	}
	if err := b.postJSON(ctx, auth, "b2_get_upload_part_url",
		map[string]string{"fileId": fileID}, &up); err != nil {
		return "", fmt.Errorf("%w: get_upload_part_url: %v", apperr.ErrUploadFailed, err)
	}
	sum := sha1.Sum(data)
	sha := hex.EncodeToString(sum[:])
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, up.UploadURL, bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("%w: %v", apperr.ErrUploadFailed, err)
	}
	req.Header.Set("Authorization", up.AuthorizationToken)
	req.Header.Set("X-Bz-Part-Number", strconv.Itoa(num))
	req.Header.Set("X-Bz-Content-Sha1", sha)
	req.Header.Set("Content-Length", strconv.Itoa(len(data)))
	req.Header.Set("X-Bz-Part-File-Id", fileID) // test-stub grouping; harmless to real B2
	resp, err := b.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: upload part %d: %v", apperr.ErrUploadFailed, num, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: upload part %d: status %d", apperr.ErrUploadFailed, num, resp.StatusCode)
	}
	return sha, nil
}
```

- [ ] **Step 5: Update the not-yet-implemented test**

The earlier `TestB2LargeFileNotYetImplemented` no longer holds (large files now upload). Remove that test from `internal/b2/native_test.go`.

- [ ] **Step 6: Run b2 tests + full build**

Run: `go test ./internal/b2/ -v && go build ./... && go test ./... && gofmt -l .`
Expected: all PASS including `TestB2LargeFileUpload`; `gofmt -l` empty.

- [ ] **Step 7: Commit**

```bash
git add internal/b2/largefile.go internal/b2/native.go internal/b2/native_test.go
git commit -m "feat: B2 native large-file (multipart) upload"
```

---

## Task 8: README + project notes

**Files:** Modify `README.md`, `docs/project_notes/decisions.md`, `docs/project_notes/issues.md`, `docs/project_notes/key_facts.md`, `CLAUDE.md`

- [ ] **Step 1: Update README**

Add a "Storage backends" section to `README.md` documenting:
- Two backends: S3-compatible (default) and native B2.
- `backuprepo backend [s3|b2]` to view/switch; `--backend s3|b2` per-command override.
- The manual file commands `ls`/`get`/`put`/`rm`/`find` with their flags (`-r`, `-f`/`-y`), with a short example session.
- That `init` now also asks for the bucket ID (used by the native B2 API).
Move the daemon/web-UI items in the roadmap section as-is (still unbuilt). (Prose only; verify command names against `main.go`.)

- [ ] **Step 2: Update project notes**

- `docs/project_notes/decisions.md`: append ADR-008 (native B2 backend over stdlib, B2 v2 API, interface-segregation so backup keeps `Uploader`), ADR-009 (stored backend mode + `--backend` override), ADR-010 (B2 addressed by bucket ID for list/upload, bucket name for download).
- `docs/project_notes/issues.md`: add a completed entry for the dual-backend + manual file client work.
- `docs/project_notes/key_facts.md`: add the `backend` config field, the two backends, the manual commands, and that B2 native uses the v2 API + bucket ID.

- [ ] **Step 3: Update CLAUDE.md status note**

In `CLAUDE.md`'s "Implementation status" section, note that a native B2 backend + manual file commands (`ls`/`get`/`put`/`rm`/`find`/`backend`) now exist alongside S3, switchable via stored mode or `--backend`.

- [ ] **Step 4: Final verification**

Run: `go build ./... && go test ./... && go vet ./... && gofmt -l .`
Expected: all clean.

- [ ] **Step 5: Commit**

```bash
git add README.md docs/project_notes/ CLAUDE.md
git commit -m "docs: document dual backends and manual file commands"
```

---

## Self-Review Notes

- **Spec coverage:** Backend interface + 2 impls + factory (Tasks 2,4); B2 native API auth/upload/download/list/delete/search (Task 4) + large-file (Task 7); RemoteConfig + bucket_id/backend + migration + GetBackend/SetBackend (Task 3); init bucket-ID prompt + config/status display (Task 3); all six commands + `--backend`/`-r`/`-f` (Tasks 5,6); new typed errors (Task 1); httptest tests (Tasks 4,7); FakeBackend (Task 2); docs (Task 8). All spec sections mapped.
- **Naming consistency:** `Backend`/`Uploader`/`ObjectInfo`/`Listing`/`Config{BucketName,BucketID}`; `S3Backend`/`B2Backend`/`FakeBackend`/`NewFake`/`NewBackend`; `store.RemoteConfig` (`.Bucket`,`.BucketID`)/`GetBackend`/`SetBackend`; `b2SmallFileLimit`; handler names `Ls/Get/Put/Rm/Find/Backend`. Used consistently across tasks.
- **Interface segregation:** `backup.Service` stays on `b2.Uploader` (no change in Task; confirmed in file table). `Backend` embeds `Uploader`, so the factory's `Backend` is accepted by `cli.Upload` and `backup`.
- **No placeholders:** every code step has complete code; the two "merge the import" notes (native.go `time`, files_test.go `store`) are explicit instructions. The large-file stub in Task 4 is intentional and replaced wholesale in Task 7.
- **Risk:** B2 v2 API field names/headers are the main external-correctness risk (no live test in CI); httptest covers request/response shape. The `X-Bz-Part-File-Id` header is a test-stub grouping aid and is ignored by real B2.
