# Issues / Work Log

Quick log of completed work. Brief entries; link to tickets/PRs where available.

### 2026-06-06 - Core CLI foundation
- **Status**: Completed
- **Description**: Implemented and merged the core CLI: `internal/{apperr,crypto,config,store,b2,backup,cli}` + `main.go`. Subcommands: `init`, `watch`, `unwatch`, `list`, `status`, `upload`, `config`, `help`. Built via brainstorm → spec → plan → subagent-driven TDD (9 tasks, each spec+quality reviewed; final integration review: READY TO MERGE).
- **Notes**: Merged to `master` (no-ff) and pushed. All 6 test packages pass. See spec + plan under `docs/superpowers/`.

### 2026-06-06 - README usage walkthrough
- **Status**: Completed
- **Description**: Added a full usage walkthrough to `README.md` (B2 key setup → init → watch → status/list → upload → config) with verified example output; corrected Go version requirement to 1.25+.
- **Notes**: Committed directly to `master`.

### 2026-06-06 - Dual backends + manual file client
- **Status**: Completed, merged to `feat/dual-backend`
- **Description**: Implemented and merged the dual-backend + manual file client feature:
  - `b2.Backend` interface (superset of `b2.Uploader`) with `S3Backend`, `B2Backend`, and `FakeBackend` implementations.
  - Native Backblaze B2 v2 API client (`internal/b2/native.go`) — authorize, small-file upload, download, list, delete, exists — plus large-file multipart upload (`internal/b2/largefile.go`) with 100 MB part threshold.
  - Six new CLI commands: `ls`, `get`, `put`, `rm`, `find`, `backend`; all accept `--backend s3|b2` override flag; `rm` confirms unless `-f`/`-y`; folder ops are prefix-recursive with `-r`.
  - Schema migration: added `bucket_id TEXT` and `backend TEXT` columns to the `config` table; `store.Open` auto-migrates existing databases. `init` now also prompts for bucket ID.
  - `store.GetBackend`/`store.SetBackend` with `"s3"` default; `main.go` factory resolves effective backend (flag → stored → `"s3"`).
  - httptest-based tests for the B2 native client (including large-file multipart); all packages pass `go test ./...`.
- **Notes**: Commits on `feat/dual-backend` (Tasks 1–7 completed; Task 8 this entry). See spec `docs/superpowers/specs/2026-06-06-backuprepo-backends-design.md` and plan `docs/superpowers/plans/2026-06-06-backuprepo-backends.md`.

### 2026-06-16 - Migrate native B2 client v2 → v3
- **Status**: Completed
- **Description**: Reconciled the v2/v3 discrepancy between the dual-backend spec (said v3) and the shipped code (was v2) by migrating `B2Backend` to the **v3** API. Added `b2APIVersion = "v3"` in `native.go` (single source for the path segment); reworked `authorize` to parse the v3 `b2_authorize_account` response (`apiUrl`/`downloadUrl`/`recommendedPartSize` now nested under `apiInfo.storageApi`). Updated `native_test.go` httptest server to v3 paths + nested auth shape. Updated `key_facts.md`; added ADR-011 (supersedes the v2 note in ADR-008).
- **Notes**: `go test ./...` all pass, `go vet`/`gofmt` clean. All non-auth endpoints are identical between v2 and v3, so only the path segment + auth-response parsing changed. Live B2 verification still pending (see below).

### 2026-06-16 - Live B2 (v3) end-to-end verification
- **Status**: Completed
- **Description**: Ran the first live test of the native `b2` backend (v3) against bucket SC-OFFICE. Verified authorize (v3 nested `apiInfo.storageApi` parsing confirmed against the real API), `ls`, `put`, `get` (byte-identical round-trip), `find`, and `rm`. Surfaced and fixed a pre-existing delete bug (see bugs.md 2026-06-16): `b2_list_file_versions` 400 from sending an empty `startFileId`.
- **Notes**: v3 migration (ADR-011) is now live-verified, not just httptest-verified. `rm` flag-ordering gotcha noted below.

### 2026-06-16 - Makefile + `bb` binary
- **Status**: Completed
- **Description**: Added a `Makefile` that builds a single **static** binary named `bb` (`CGO_ENABLED=0`, `-trimpath -ldflags="-s -w"`) for easy PATH install on Linux. Targets: `build` (default), `install` (→ `$(PREFIX)`, default `~/.local/bin`), `uninstall`, `clean`, `test`, `vet`, `fmt`, `tidy`, `help`. Added `/bb` to `.gitignore`.
- **Notes**: Verified `make` output is `not a dynamic executable` (fully static), 14 MB, runs. Binary name is cosmetic — all subcommands behave identically.

### 2026-06-16 - Flags accepted in any position + README guide
- **Status**: Completed
- **Description**: Fixed the flag-ordering footgun (stdlib `flag.Parse` stops at the first positional, so trailing `-r`/`-f`/`--backend` were silently dropped on `ls`/`get`/`put`/`rm`/`find`). Added `parseFlags` in `main.go` that resumes parsing after each positional, returning positionals; all five commands now use it (`argAt` for safe indexing). Added `TestParseFlagsAnyOrder`. Updated `README.md` into a fuller user guide: `make`/`bb` build+install section, binary-name note, and a flag-position note.
- **Notes**: Verified live — `bb ls <path> --backend bogus` now errors (flag parsed) where it previously didn't. See bugs.md 2026-06-16 (flag ordering).

### 2026-06-16 - Background watcher daemon (fsnotify + debounce)
- **Status**: Implemented on working tree (uncommitted). `go test ./...` green incl. `-race`; `go vet`/`gofmt` clean. Not yet committed/merged.
- **Description**: Built the first roadmap item — the background file-watcher daemon. New `internal/daemon` package:
  - Recursive fsnotify watch setup (`addRecursive`) — inotify is not recursive, so every subdir is watched and newly created dirs are re-watched at runtime (Create-on-dir handler).
  - Event loop reacting to `Create|Write|Rename` (Rename covers atomic-save editors; `Chmod`/`Remove` ignored — the backup flow is upload-only), a 5-minute fallback full scan, PID-file lifecycle (`~/backup_repo/daemon.pid`), and graceful shutdown via `signal.NotifyContext` (SIGINT/SIGTERM).
  - Both the event path and the ticker funnel into the existing `backup.Service.UploadChanged`, so change-detection semantics have one definition.
  - Debounce: a two-timer state machine — **1 s** quiet window (reset on every event) + **5 s** max-delay cap (starvation guard) — with re-scan-all granularity. Windows are `Daemon` fields defaulted in `New` (tunable; set small in tests).
  - Wired `start`/`stop` into `main.go` dispatch + `usage`; added typed `apperr.ErrDaemon`; thin `cli.Start`/`cli.Stop` wrappers.
  - Tests (`internal/daemon/daemon_test.go`): burst coalescing → 1 flush, max-delay forces a flush under a trickle, cancel exits the goroutine, `addRecursive` watches root+subdirs, PID guard refuses a second start.
  - Added dependency `github.com/fsnotify/fsnotify v1.10.1` (see ADR-012).
- **Notes**: Linux-focused — `syscall.SIGTERM` and non-recursive inotify handling assume Unix; Windows (`ReadDirectoryChangesW`, natively recursive) needs build-tagged variants. Binary still ~14 MB. Still pending: web UI / `serve` (port 9171); deletion propagation (daemon is upload-only); commit/merge; README still says the daemon is not built.

### 2026-06-16 - Deletion propagation (opt-in)
- **Status**: Implemented on working tree (uncommitted). `go test ./...` green incl. `-race`; vet/gofmt clean.
- **Description**: Made `UploadChanged` able to remove remote objects whose local files were deleted, **opt-in** via `--delete` on `upload` and `start` (see ADR-013):
  - New `b2.Deleter` interface (`Delete` only); `backup.Service` keeps its narrow `b2.Uploader` dep and gains an optional deleter via `WithDeleter` (nil = off, the default).
  - `backup.propagateDeletions` runs after the upload walk: deletes remote + local record for tracked files now absent under a **currently-present** watched folder. Unmounted-folder safety guard (skip if the folder is missing); uncertain stat errors never delete; `ErrObjectNotFound` treated as success.
  - `store.RemoveFile`; `Result.Deleted`; `cli.Upload`/`cli.Start` widened to `b2.Backend` + `deleteRemoved`; `daemon.EnableDeletions` + flush reports deletions; `main.go` `--delete` FlagSets for `upload`/`start`; usage updated.
  - Tests: disabled-by-default, removes-remote+record (keeps others), skips-missing-folder.
- **Notes**: Opt-in only — default still retains backups. Versioned-bucket deletes purge all versions (irreversible). Daemon deletion uses scan reconciliation, not fsnotify `Remove` events. `unwatch`ed folders' objects persist by design.

### 2026-06-16 - `bucket` command: switch buckets without full re-init
- **Status**: Implemented on branch `feat/set-bucket` (off `master`). `go test ./...` green; vet/gofmt clean.
- **Description**: Added `bb bucket [<name> [<id>]]` to change the destination bucket without re-running `init` (which re-prompts for credentials/endpoint/region):
  - `store.SetBucket(name, id)` updates only `bucket_name` + `bucket_id` (requires existing config; empty id clears it for S3-only buckets).
  - `cli.Bucket` — no args shows the current bucket; `<name> [<id>]` switches. Mirrors the `backend [s3|b2]` show/set pattern.
  - `main.go` dispatch (`case "bucket"`, ≤2 positionals) + `usage` SETUP entry.
  - Tests: switch keeps credentials/endpoint/region intact + no-arg show; set without config → `ErrNotConfigured`.
- **Notes**: Bucket-only change — the stored key must already have access to the new bucket. A bucket-scoped key (see security note) won't authorize a different bucket; for that, re-run `init`. No new ADR (operates within ADR-010's name+ID model). Merged after the daemon PR (#4); see PR #5.

## Pending / Next

- ~~**`rm` flag ordering**~~ — RESOLVED 2026-06-16: flags now work in any position via `parseFlags` (see work log + bugs.md).
- ~~**Daemon watcher + `start`/`stop`**~~ — BUILT 2026-06-16 (working tree; see work log + ADR-012): fsnotify recursive watch + 5-min fallback scan + 1s/5s debounce, graceful shutdown.
- **Web UI / `serve` (port 9171)** — still not built. Localhost interface: folder table, last-backup times, delete actions, force-upload button (designed in `CLAUDE.md`).
- **Windows event backend** — `ReadDirectoryChangesW` (build-tagged) to replace the Linux-only `syscall.SIGTERM` / non-recursive inotify handling in `internal/daemon`.
- ~~**Deletion propagation**~~ — DONE 2026-06-16 (working tree; see work log + ADR-013): opt-in `--delete` on `upload`/`start`, with an unmounted-folder safety guard.
- **Minor follow-ups from final review** — `b2.Uploader.Exists` and the `size` param are unused forward-looking hooks; a couple of cosmetic nits (`copyInto` wrapper, `usage(*os.File)` vs `io.Writer`).
