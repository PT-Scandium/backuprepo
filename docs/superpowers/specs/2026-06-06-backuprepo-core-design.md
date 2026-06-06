# backuprepo — Core Foundation Design

**Date:** 2026-06-06
**Status:** Approved (brainstorming) — ready for implementation plan
**Scope:** Core foundation only. Daemon file-watcher and web UI are deferred to a follow-up spec.

## 1. Purpose

`backuprepo` is a cross-platform CLI tool that uploads changed files from user-specified
folders to a Backblaze B2 bucket via the S3-compatible API. This spec covers the testable
core: configuration, an encrypted local database, the B2/S3 uploader, change detection, and
the CLI subcommands that drive them. The background daemon (`fsnotify` + 5-minute fallback
scan) and the localhost web UI on port 9171 are intentionally out of scope here and will be
built on top of this foundation in a later pass.

## 2. Key decisions (resolved during brainstorming)

| Topic | Decision | Rationale |
|-------|----------|-----------|
| DB engine | `modernc.org/sqlite` (pure Go, no CGO) | Preserves single static binary, small size, easy Windows cross-compile. The spec's literal `go-sqlcipher` requires CGO, which conflicts with those goals. |
| Encryption | Field-level **AES-256-GCM** on credential columns | Honors the intent of the SQLCipher requirement (no plaintext creds at rest, tamper-evident) without CGO. |
| Master key | Random 32-byte key in `~/backup_repo/key`, mode `0600` | Daemon must start silently (no passphrase prompt). Key file is the simplest silent-start option. Protects against casual at-rest access. |
| Credentials | B2 S3 **keyID + applicationKey + bucket name + endpoint/region** | These are what `aws-sdk-go-v2` S3 needs. The S3 API addresses buckets by **name**, not the bucket *ID*. |
| Upload format | **Per-file** uploads (1 file → 1 object), no tar+gzip | Keeps per-file backup state meaningful for `list`/`status` (and the future web UI's per-file columns). |
| Testing | Uploader behind an interface with an in-memory fake | Logic is verifiable without real B2 credentials. |
| Build sequence | Core foundation first, daemon + web UI later | Reduces risk; foundation is independently testable. |

## 3. Architecture

Single Go module `backuprepo`, building one binary. Logic lives in `internal/` packages so
each unit is small, has one purpose, and is testable in isolation.

```
backuprepo/
├── go.mod
├── main.go                  # CLI entrypoint: arg parsing, subcommand dispatch
├── errors.go                # typed error catalog (shared)
├── internal/
│   ├── config/              # Config struct; key-file load/create (~/backup_repo/key, 0600); paths
│   ├── crypto/              # AES-256-GCM seal/open for credential fields
│   ├── store/              # SQLite open + schema + queries; encrypted credential fields
│   ├── b2/                  # Uploader interface + aws-sdk-go-v2 S3 impl + in-memory fake
│   ├── backup/              # Orchestration: walk folders, diff vs store, upload changed files
│   └── cli/                 # Subcommand handlers
└── docs/superpowers/specs/  # this document
```

Rationale for `internal/` over the flat `backup.go`/`config.go` files mentioned in CLAUDE.md:
this is a single binary, not a library to be imported, so the real public surface is the CLI.
`internal/` enforces clean boundaries. `errors.go` stays at the root as the shared catalog.

## 4. Invariants (from CLAUDE.md "Key Invariants")

- Every exported function takes `context.Context` as its first argument.
- Errors are typed values defined in `errors.go`, wrapped with `%w`; never raw strings.
- No global state. `Config` is loaded once in `main` and passed down. The `b2.Uploader`
  interface is injected into `backup`.
- Credentials are AES-GCM sealed before being written to SQLite and only opened in memory
  when needed for an upload. They are never written in plaintext anywhere.

## 5. Data model (SQLite)

```sql
CREATE TABLE config (
  id           INTEGER PRIMARY KEY CHECK (id = 1),  -- single-row table
  s3_endpoint  TEXT,    -- e.g. https://s3.us-west-004.backblazeb2.com
  s3_region    TEXT,    -- e.g. us-west-004
  bucket_name  TEXT,    -- bucket NAME (not the B2 bucket ID)
  key_id_enc   BLOB,    -- AES-GCM(keyID)
  app_key_enc  BLOB,    -- AES-GCM(applicationKey)
  created_at   INTEGER
);

CREATE TABLE folders (
  path      TEXT PRIMARY KEY,  -- absolute path to a watched folder
  added_at  INTEGER
);

CREATE TABLE files (
  path         TEXT PRIMARY KEY,  -- absolute local file path
  size         INTEGER,
  mod_time     INTEGER,           -- local mtime (unix seconds)
  sha256       TEXT,              -- content hash for change detection
  last_backup  INTEGER,           -- unix time of last successful upload (NULL = never)
  remote_key   TEXT               -- object key within the bucket
);
```

**Change detection:** compare `size` + `mod_time` first (cheap). If either differs from the
stored row (or the row is absent), compute `sha256`; upload only if the hash differs from
what was last backed up. Unchanged files are skipped (no redundant uploads).

**Remote key scheme:** the object key mirrors the file's absolute path with a leading-slash
strip and OS separators normalized to `/`, so the bucket layout is predictable and stable
across runs.

## 6. Uploader

```go
type Uploader interface {
    Upload(ctx context.Context, key string, r io.Reader, size int64) error
    Exists(ctx context.Context, key string) (bool, error)
}
```

- Real implementation wraps `aws-sdk-go-v2` S3 with a custom endpoint resolver pointing at
  the B2 S3 endpoint.
- Files ≤ 100 MB: single `PutObject`.
- Files > 100 MB: `feature/s3/manager`.Uploader (S3 multipart upload).
- `fakeUploader` stores objects in an in-memory map for unit tests.

## 7. CLI surface (this scope)

```
backuprepo                      # if unconfigured: run interactive init; else: print status hint
backuprepo init                 # interactive setup: keyID, appKey, bucket name, endpoint, region, first folder
backuprepo watch /path/to/dir   # add a folder to the watch list
backuprepo unwatch /path/to/dir # remove a folder from the watch list
backuprepo list                 # list watched folders + per-file backup status
backuprepo status               # summary: configured?, folder count, pending (changed) file count
backuprepo upload               # force-scan all watched folders and upload changed files (no-op if none)
backuprepo config               # print current config (server URL, bucket, watched folders); secrets masked
```

Deferred to the follow-up spec: `serve`, `start`, `stop` (daemon + web UI).

- Output is plain text. Exit 0 on success, 1 on error (message to stderr).
- `config` masks the applicationKey (e.g. shows only last 4 chars); never prints the secret.
- Commands that need configuration return `ErrNotConfigured` if `init` has not been run.

## 8. Typed errors (`errors.go`)

`ErrNotConfigured`, `ErrAlreadyConfigured`, `ErrInvalidCredentials`, `ErrFolderNotFound`,
`ErrFolderNotWatched`, `ErrUploadFailed`, `ErrStore` (DB failure), `ErrCrypto` (seal/open
failure). All other errors wrap one of these with `%w` and added context.

## 9. Error handling

Typed errors bubble up to `main`, which prints the message to stderr and exits 1. Transient
S3/network errors are retried with bounded backoff (default SDK retryer); exhausted retries
surface as `ErrUploadFailed`. A single file's upload failure during `upload` is reported but
does not abort the remaining files; the command exits 1 if any file failed.

## 10. Testing strategy

Implemented test-first (TDD) per unit:

- `crypto`: AES-GCM seal/open round-trip; tampering detection; wrong-key failure.
- `store`: schema creation; config upsert; folder add/remove; file upsert; and an
  encryption-at-rest assertion (raw DB bytes do not contain the plaintext key material).
- `backup`: change detection matrix (new / unchanged / size-changed / mtime-changed /
  content-changed) driving a `fakeUploader`; verifies skip-vs-upload and `last_backup` updates.
- `cli`: argument parsing and exit-code behavior for each subcommand using a temp HOME and
  the fake uploader.

## 11. Build

```
go build -ldflags="-s -w" -o backuprepo .
```

Targets a small static binary (<10 MB goal). No CGO. Cross-compiles to Windows/Linux/macOS
via `GOOS`/`GOARCH`.

## 12. Out of scope (follow-up spec)

- Background daemon: `fsnotify` real-time watching + 5-minute fallback full scan.
- `start` / `stop` / `serve` commands and daemon lifecycle/PID management.
- Web UI on localhost:9171 (warm color scheme, folder navigation, per-file table, Upload/Close).
- Windows `ReadDirectoryChangesW` integration (fsnotify covers it, but native path is noted).
