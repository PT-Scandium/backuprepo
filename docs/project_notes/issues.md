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

## Pending / Next

- **Manual B2 end-to-end test** — Blocked on real credentials. Run `init` with real keyID/applicationKey/bucket name/bucket ID, then `watch` + `upload` twice (expect second run to skip unchanged). Also test `ls`/`get`/`put`/`rm`/`find` against a live bucket with `--backend b2`.
- **Daemon + web UI (next spec)** — Background watcher (fsnotify + 5-min fallback scan), `serve`/`start`/`stop`, web UI on port 9171. Designed in `CLAUDE.md`; not yet built.
- **Minor follow-ups from final review** — `b2.Uploader.Exists` and the `size` param are unused forward-looking hooks; a couple of cosmetic nits (`copyInto` wrapper, `usage(*os.File)` vs `io.Writer`).
