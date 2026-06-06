# backuprepo — Dual Backend (B2 Native + S3) & Manual File Client Design

**Date:** 2026-06-06
**Status:** Approved (brainstorming) — ready for implementation plan
**Builds on:** `2026-06-06-backuprepo-core-design.md` (core CLI already implemented & merged)

## 1. Purpose

Add a second storage backend — a **native Backblaze B2 API client** — alongside the existing
S3-compatible client, and expose interactive bucket operations (list, download, upload, delete,
search) over either backend. The user can switch backends and switch back. A single `Backend`
abstraction unifies *all* bucket access, so both the new manual commands and the existing
folder-backup `upload` flow run through whichever backend is selected.

## 2. Decisions (resolved during brainstorming)

| Topic | Decision |
|-------|----------|
| B2 backend | Implement the **real Backblaze B2 Native API** (separate endpoints/auth), not S3-dressed-up. |
| Scope of mode | **Unified** — one `Backend` interface powers all bucket ops, including backup `upload`. |
| Switching | **Stored setting** (`backend` column) switched via a `backend` command, **plus** a per-command `--backend b2\|s3` override. Default `s3` (preserves current behavior). |
| Bucket identity | `init` collects **both** the bucket name (S3 + B2 download) and the bucket ID (B2 list/upload). |
| Folders | **Prefix-based, recursive.** A path ending `/` (or used as a prefix) is a folder; `get`/`rm` over a folder are recursive; `ls` groups immediate children via delimiter. |
| Search | **Case-insensitive substring** match on object key/name, optionally scoped to a prefix. |
| Delete | Removes **all versions**; **confirms** (always for recursive) unless `-f`/`-y`. |
| Command names | New manual ops: `put`, `get`, `ls`, `rm`, `find`, `backend`. Existing `upload` (backup) untouched. |
| B2 dependency | **stdlib only** (`net/http`, `encoding/json`, `crypto/sha1`) — no new third-party B2 SDK. |

## 3. Architecture

### 3.1 The `Backend` interface

Replaces the narrow `b2.Uploader` (`Upload`, `Exists`). New interface in `internal/b2/backend.go`:

```go
type ObjectInfo struct {
    Key      string
    Size     int64
    Modified time.Time
}

type Listing struct {
    Objects  []ObjectInfo // files directly under the prefix (non-recursive) or all (recursive)
    Prefixes []string     // "folders" (common prefixes), non-recursive only
}

type Backend interface {
    Upload(ctx context.Context, key string, r io.Reader, size int64) error
    Download(ctx context.Context, key string) (io.ReadCloser, int64, error)
    List(ctx context.Context, prefix string, recursive bool) (Listing, error)
    Delete(ctx context.Context, key string) error // removes all versions
    Exists(ctx context.Context, key string) (bool, error)
}
```

`Upload` + `Exists` keep their existing signatures, so `backup.Service` (which depends only on
those two methods) works unchanged once it accepts a `Backend`.

### 3.2 Implementations

- **`internal/b2/s3.go` — `S3Backend`** (extends today's `S3Uploader`): GetObject (Download),
  ListObjectsV2 with `Delimiter="/"` + pagination (List), DeleteObject + (if present)
  ListObjectVersions/DeleteObjects (Delete-all-versions), HeadObject (Exists), existing
  PutObject/manager (Upload).
- **`internal/b2/native.go` — `B2Backend`** (new): the Backblaze B2 Native API client (§5).
- **`internal/b2/fake.go` — `FakeBackend`** (was `FakeUploader`): in-memory map implementing the
  full `Backend`, used by `cli` and `backup` tests.

### 3.3 Factory & wiring

```go
// kind is "s3" or "b2"
func NewBackend(ctx context.Context, kind string, cfg Config) (Backend, error)
```

`main.go` resolves the **effective backend** = `--backend` flag → else stored `backend` →
else `"s3"`, builds the `Backend` once, and passes it to the relevant handler. The backup
`upload` path uses the same factory.

`b2.Config` carries everything both backends need:

```go
type Config struct {
    Endpoint   string // S3 endpoint URL
    Region     string // S3 region
    BucketName string // S3 + B2 download-by-name
    BucketID   string // B2 list/upload
    KeyID      string
    AppKey     string
}
```

## 4. Config & schema changes

- Rename `store.S3Config` → **`store.RemoteConfig`** and add fields `BucketID string` and
  `Backend string`. (The `bucket_name` column already exists.)
- `config` table gains columns: `bucket_id TEXT`, `backend TEXT`.
- **Migration:** `store.Open` runs `pragma table_info(config)`; for any of the new columns not
  present it issues `ALTER TABLE config ADD COLUMN ...`. Fresh DBs get the columns from the
  `CREATE TABLE IF NOT EXISTS` schema. `backend` defaults to `'s3'` when null/empty.
- New store methods: `GetBackend(ctx) (string, error)` and `SetBackend(ctx, kind string) error`
  (validates `kind ∈ {"s3","b2"}`, else `ErrInvalidBackend`). `SaveConfig`/`GetConfig` handle the
  two new fields (bucket_id stored plaintext — it is not a secret; credentials remain encrypted).
- `cli.Init` prompts add: "Bucket ID (for native B2 API)" and keep existing prompts.
- `cli.Config` and `cli.Status` print the current backend mode and the bucket ID.

## 5. B2 Native API client (`native.go`)

stdlib `net/http` + `encoding/json` + `crypto/sha1`. A `B2Backend` holds the `Config` and a
lazily-acquired auth context cached for the process lifetime.

### 5.1 Auth
- `GET {baseAuthURL}/b2api/v3/b2_authorize_account` with HTTP Basic `keyID:appKey`.
  Base URL: `https://api.backblazeb2.com`. Response gives `apiUrl`, `downloadUrl`,
  `authorizationToken` (cached). Failure → `ErrAuthFailed`.

### 5.2 Upload
- **Small files** (size ≤ `largeFileThreshold`, default 100 MB):
  `b2_get_upload_url` (per bucketID) → POST body to the returned `uploadUrl` with headers
  `Authorization`, `X-Bz-File-Name` (URL-encoded key), `Content-Type: b2/x-auto`,
  `Content-Length`, `X-Bz-Content-Sha1` (hex SHA-1 of the content). The reader is buffered to
  compute SHA-1 and length first.
- **Large files** (> threshold): `b2_start_large_file` → for each ~100 MB part
  `b2_get_upload_part_url` + `b2_upload_part` (with per-part SHA-1) → `b2_finish_large_file`
  with the ordered SHA-1 list. (Riskiest sub-part; isolated as a late plan task — §8.)
- Failure → `ErrUploadFailed`.

### 5.3 Download
- `GET {downloadUrl}/file/{bucketName}/{urlEncodedKey}` with `Authorization` header. Returns the
  body stream + `Content-Length`. 404 → `ErrObjectNotFound`; other failure → `ErrDownloadFailed`.

### 5.4 List / Search
- `b2_list_file_names` (POST {apiUrl}/b2api/v3/...) with `{bucketId, prefix, delimiter,
  startFileName, maxFileCount}`. `delimiter:"/"` when non-recursive (folders come back as
  `folder/` entries); omitted when recursive. Loop on `nextFileName` for pagination. Map each
  `fileName/contentLength/uploadTimestamp` to `ObjectInfo`. Failure → `ErrListFailed`.
- Search = recursive `List(prefix)` then client-side case-insensitive `strings.Contains` on the
  portion of the key after the prefix.

### 5.5 Delete (all versions)
- `b2_list_file_versions` filtered to the exact `fileName` (paginated) → for each version
  `b2_delete_file_version {fileName, fileId}`. No versions found → `ErrObjectNotFound`.
  Failure → `ErrDeleteFailed`.

## 6. New CLI commands

New handlers in `internal/cli/files.go`. Each takes a `b2.Backend`, an `io.Writer` (and `rm`
takes an `io.Reader` for confirmation), so they are testable with `FakeBackend`.

| Command | Flags | Behavior |
|---------|-------|----------|
| `ls [path]` | `-r` | `List(path, recursive)`. Folders printed with trailing `/`; files show size + modified. Empty path = bucket root. |
| `get <remote> [local]` | `-r` | Single object → write to `local` (default: basename in cwd). With `-r` and a folder/prefix `<remote>`, recursively download every object, recreating the prefix structure under `local`. |
| `put <local> [remote]` | `-r` | Single file → upload to `remote` (default: basename). Directory + `-r` → walk and upload each file under the `remote` prefix. |
| `rm <path>` | `-r`, `-f`/`-y` | Single object delete (all versions). With `-r`, `List(path, recursive)` then delete each. Confirms (count shown) unless `-f`. |
| `find <query> [prefix]` | — | Case-insensitive substring search over keys (optionally under `prefix`); prints matches. |
| `backend [b2\|s3]` | — | No arg: print current mode. With arg: `SetBackend` (persisted). |

Global per-command override: `--backend b2|s3` (parsed before building the backend). Unknown
backend value → `ErrInvalidBackend`. Remote-key normalization reuses `backup.RemoteKey`
semantics (forward slashes, no leading `/`).

### 6.1 main.go dispatch
Add cases `ls`, `get`, `put`, `rm`, `find`, `backend`. Use `flag.FlagSet` per subcommand for
`-r`/`-f`/`--backend`. `backend`/`config`/`status` do not require a built backend; the file ops
build the effective backend via the factory from stored `RemoteConfig`. Exit-code contract
(0/1, stderr) unchanged.

## 7. Errors & testing

- New `internal/apperr` sentinels: `ErrAuthFailed`, `ErrObjectNotFound`, `ErrDownloadFailed`,
  `ErrDeleteFailed`, `ErrListFailed`, `ErrInvalidBackend`. (`ErrUploadFailed` stays.)
- **B2 native client:** unit-tested against an `httptest.Server` that simulates
  authorize/get_upload_url/upload/list_file_names/list_file_versions/download/delete, asserting
  request shape (headers, JSON bodies, SHA-1) and exercising pagination, not-found, and auth
  failure. No live Backblaze required.
- **`FakeBackend`:** full in-memory `Backend`; drives `cli` (put/get/ls/rm/find/backend) and
  `backup` tests, including recursive and confirmation paths.
- **`S3Backend`:** new methods compile-checked + interface-asserted; live S3 still verified
  manually (unchanged from core).
- **store:** migration test (open an old-schema DB lacking the new columns → `Open` adds them →
  config round-trips with bucket_id/backend), `GetBackend`/`SetBackend` validation.
- TDD per unit, matching the core plan's style.

## 8. Scope & phasing

One spec; the implementation plan sequences the **B2 large-file (multipart) upload** as an
isolated late task. If it expands beyond estimate, everything else — S3 full backend, B2
auth/small-upload/list/download/delete/search, mode switching, schema migration, and all six CLI
commands — can ship first, with B2 large-file upload as a fast follow. Until then, B2-mode
uploads of files larger than the threshold return a clear `ErrUploadFailed` ("large-file B2
upload not yet implemented") rather than silently truncating.

## 9. Out of scope

- The background daemon and web UI (still deferred to their own spec).
- Sync/mirroring semantics between local and bucket beyond the existing change-detection backup.
- B2 application-key management, bucket creation, lifecycle rules.
