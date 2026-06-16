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

## Pending / Next

- **`rm` flag ordering** — Go's `flag` stops at the first positional, so `rm <path> -f` silently ignores `-f` (prompts, then aborts under no TTY). Flags must precede the path (`rm -f <path>`). Consider reordering args or manual flag parsing so `-f`/`-r`/`-y` work in any position. Minor UX bug, not data-affecting.
- **Daemon + web UI (next spec)** — Background watcher (fsnotify + 5-min fallback scan), `serve`/`start`/`stop`, web UI on port 9171. Designed in `CLAUDE.md`; not yet built.
- **Minor follow-ups from final review** — `b2.Uploader.Exists` and the `size` param are unused forward-looking hooks; a couple of cosmetic nits (`copyInto` wrapper, `usage(*os.File)` vs `io.Writer`).
