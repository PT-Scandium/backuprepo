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

## Pending / Next

- **Manual B2 end-to-end test** — Blocked on real credentials. Run `init` with real keyID/applicationKey/bucket name, then `watch` + `upload` twice (expect second run to skip unchanged). The `S3Uploader` path has no automated test (ADR-006).
- **Daemon + web UI (next spec)** — Background watcher (fsnotify + 5-min fallback scan), `serve`/`start`/`stop`, web UI on port 9171. Designed in `CLAUDE.md`; not yet built.
- **Minor follow-ups from final review** — `b2.Uploader.Exists` and the `size` param are unused forward-looking hooks; a couple of cosmetic nits (`copyInto` wrapper, `usage(*os.File)` vs `io.Writer`).
