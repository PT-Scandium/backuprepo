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

### 2026-06-16 - `appkey` command: rotate the application key without full re-init
- **Status**: Implemented on branch `feat/change-appkey` (off `master`). `go test ./...` green; vet/gofmt clean.
- **Description**: Added `bb appkey [<new-keyID>]` to replace the stored applicationKey (and optionally the keyID) without re-running `init`:
  - **Secret read from stdin** (one line), never from `argv` — keeps it out of shell history, the process list, and (importantly) the Claude `!`-prefix transcript. Supports piping: `pass show … | bb appkey <new-keyID>`.
  - `store.SetCredentials(keyID, appKey)` re-encrypts and updates `app_key_enc` (and `key_id_enc` when keyID given) in a **single statement** so the pair can't end up mismatched. Requires existing config.
  - `cli.SetAppKey` checks `IsConfigured` first, rejects an empty secret (`ErrInvalidCredentials`, leaving the old key intact), and prints only a masked confirmation (`****XXXX`) — never the secret.
  - `main.go` dispatch (`case "appkey"`, ≤1 positional) + `usage` SETUP entry.
  - Tests: change-secret-only (other fields intact, secret not leaked to output), rotate keyID+secret, empty rejected keeps old, requires config.
- **Notes**: Born from the 2026-06-16 key-exposure incident — the stdin-only design is the deliberate fix for "don't put secrets on the command line." No-echo interactive entry (`golang.org/x/term`) is a possible future enhancement; piping is the secure path today. No new ADR (operates within the existing credential model).

### 2026-06-16 - Windows daemon backend (build-tagged lifecycle)
- **Status**: Implemented on branch `feat/windows-daemon` (off `master`). Linux `go test ./...` green; **`GOOS=windows` build + vet pass** and the Windows binary links (`PE32+`).
- **Description**: Made the daemon run on Windows by splitting the OS-specific process-lifecycle pieces out of `daemon.go` into build-tagged files (see ADR-014):
  - `signals_unix.go` (`//go:build !windows`) and `signals_windows.go` (`//go:build windows`) each provide `shutdownSignals()`, `signalStop(proc)`, `processAlive(pid)`.
  - `daemon.go` is now platform-agnostic (dropped its `syscall` import); `Run` uses `shutdownSignals()...`, `Stop` uses `signalStop`.
  - Unix: SIGINT/SIGTERM graceful, SIGTERM stop, signal-0 liveness. Windows: Ctrl-C graceful in foreground, `proc.Kill()` (forceful) for cross-process `stop`, `os.FindProcess` liveness.
  - The file-watching half was already cross-platform (`fsnotify` → `ReadDirectoryChangesW` on Windows); only signals/process control needed splitting.
- **Notes**: Windows `stop` is forceful (no graceful cleanup) — tolerated by the idempotent design (stale PID self-heals, upload retried). README updated with the Linux/Windows stop semantics. No platform-specific tests (can't portably kill self); the cross-compile build is the gate. Graceful Windows stop (named event via `x/sys/windows`) is a possible future enhancement.

### 2026-06-16 - Web UI (`bb serve`, port 9171)
- **Status**: Implemented on branch `feat/web-ui` (off `master`). `go test ./...` green; `GOOS=windows` build passes; live-verified end to end (listing, traversal 403, Host-guard 403, Close shutdown).
- **Description**: Built the localhost web UI — the last spec feature (see ADR-015). New `internal/web` package over stdlib `net/http` + `html/template` (no new dep):
  - `bb serve` runs it in the foreground on `127.0.0.1:9171` until Ctrl-C or the Close button (graceful `srv.Shutdown`). `bb start` left daemon-only.
  - Warm-themed page: header shows OS username + server location; breadcrumb folder navigation; table per the spec (Filename, File Type, File Size, Last Modified, Modified By [OS owner], Last Backup, Actions); Upload + Close buttons.
  - **Security:** `127.0.0.1`-only bind, no auth (per spec), Host-header guard (DNS-rebinding), browsing/deletion **confined to watched folders** (lexical `filepath.Rel`).
  - **Delete = local file + remote object**, unrecoverable, behind a JS `confirm()`; dir delete purges all tracked files' remotes then `RemoveAll`s the tree.
  - OS owner via build-tagged `owner_unix.go` / `owner_windows.go` (Windows stub `—`).
  - Tests (`server_test.go`, httptest): listing, traversal 403, Host-guard 403, Upload backs up, Delete removes local+remote+record.
- **Notes**: Wires `cli.Serve` + `main.go` `case "serve"` + `runServe`. README, key_facts, ADR-015 updated. With this, the project is **feature-complete against `CLAUDE.md`**.

### 2026-06-16 - Graceful Windows stop + no-echo appkey entry
- **Status**: Implemented on branch `feat/graceful-win-stop-noecho` (off `master`). Linux `go test ./...` green; `GOOS=windows` build + vet pass; piped `appkey` smoke-verified.
- **Description**: The two "optional polish" items (see ADR-016):
  - **Graceful Windows `stop`**: daemon creates a per-PID named event (`Local\backuprepo-daemon-stop-<pid>`) and waits on it (`installStopWatcher`, build-tagged); `bb stop` `SetEvent`s it so `Run` returns through normal deferred cleanup (PID file removed). `proc.Kill` kept as fallback. `Run` gained an `extStop` select case; Unix `installStopWatcher` is a no-op (SIGTERM unchanged). Uses `golang.org/x/sys/windows` (already transitive).
  - **No-echo `appkey`**: `cli.readSecret` uses `golang.org/x/term.ReadPassword` when stdin is a terminal, else a line read (keeps piping + tests working). New module `golang.org/x/term`.
- **Notes**: Windows event code verified by cross-compile (no Windows host to run on); existing `appkey` tests still pass via the fallback path. README, key_facts, ADR-016 updated; ADR-014's forceful-stop note marked superseded.

### 2026-06-16 - User-facing text says `bb` (not `backuprepo`)
- **Status**: Done (branch `docs/comments-and-readme`). gofmt/vet clean; `go test ./...` green; `GOOS=windows` build passes.
- **Description**: Swept all `.go` files so user-facing output uses the binary name **`bb`** for consistency (users invoke `bb`, not `backuprepo`):
  - `apperr` sentinel messages reprefixed `backuprepo:` → `bb:`; "(run `backuprepo init`)" → "(run `bb init`)".
  - `main.go` usage/help text + the 8 `usage: backuprepo …` errors → `bb`.
  - `cli` "not configured" message; daemon "use `bb watch`/`bb stop`" errors + command refs in comments; web console line; web UI HTML title/header.
  - Updated `main_test.go` (asserts the usage example, now `bb ls --backend s3`).
- **Notes**: Deliberately **left unchanged**: import paths / module path (`backuprepo` — renaming breaks the build), project-name doc comments, `~/backup_repo` state dir, and the internal Windows stop-event id. See the naming-convention note in `key_facts.md`. Surgical (no global `backuprepo`→`bb`).

### 2026-06-16 - Version tracking (VERSION file + odometer scheme + `bb version`)
- **Status**: Implemented on branch `feat/versioning`. `go test ./...` green (incl. new `internal/version` tests); gofmt/vet clean; `GOOS=windows` build passes.
- **Description**: Added a single-source-of-truth build version with a custom numbering scheme (per user spec):
  - Root **`VERSION`** file (`1.0.0`), **embedded** into the binary via `//go:embed` in `main.go`; new **`bb version`** (`--version`/`-v`) prints it.
  - **`internal/version`** — odometer scheme: `major.minor.patch`, each component `0..20`; passing 20 wraps to 0 and carries up (`1.0.20 → 1.1.0`, `1.20.20 → 2.0.0`); major unbounded. Pure + table-tested (`version_test.go`).
  - **`cmd/bump`** + Makefile targets `version`, `bump` (patch+carry), `bump-minor`, `bump-major`; `make build` echoes `v<VERSION>`.
- **Notes**: Bump rewrites `VERSION`; rebuild bakes the new value in. Release = git tag `v<VERSION>`. See key_facts "Versioning". Carry verified end-to-end via the bump tool (1.0.20→1.1.0, 1.20.20→2.0.0).

### 2026-07-06 - `bb buckets` + name→ID auto-resolution (init & bucket)
- **Status**: Implemented in working tree; `go build`/`go vet`/`gofmt`/`go test ./...` all green. See ADR-017.
- **Description**: Added the ability to list all account buckets and to switch/setup by name without hand-copying bucket IDs:
  - **`bb buckets`** — lists every visible bucket (NAME / ID / TYPE, active marked `*`) via native `b2_list_buckets`; always uses the native B2 API even under the `s3` backend (only it returns bucket IDs). New `b2.BucketInfo`, package-level `b2.ListBuckets(ctx, cfg)`, `(*B2Backend).ListBuckets`; kept **off** the `b2.Backend` interface.
  - Captured **`accountId`** from `b2_authorize_account` into `b2Auth` (was decoded then discarded) — required by `b2_list_buckets`.
  - **Auto-resolve bucket ID** in `bb init` and `bb bucket <name>` via shared `cli.resolveBucketID`; a `cli.BucketLister` func is injected into `Init`/`Bucket` (main.go wires `b2.ListBuckets`) so it's testable without network. Success fills the ID (init skips its ID prompt); failure/not-found falls back (init prompts; `bucket` clears) with a message. Explicit `bb bucket <name> <id>` still wins.
  - `main.go`: new `buckets` case + `b2ConfigFromStore` helper (refactored out of `buildBackend`); help text for `buckets` and the auto-resolving `bucket`.
  - Tests: `TestB2ListBuckets` (+ mock `b2_list_buckets` endpoint), `TestInitBucketIDFallback` (error/not-found/nil-lister), `TestBucketAutoResolvesID` (resolved + clears).
- **Notes**: Live-verified against the user's account (listed 7 buckets: CCTV-KK130, CCTV-KK520, Family-Funeral, Family-Wedding, Rent-Contract-MDTM, SC-OFFICE, Sc-Coding). Surfaced + explained a stored name/ID mismatch (`Scandiumsc` name paired with `SC-OFFICE`'s ID) that `bb bucket <name>` now prevents. **Not yet committed/branched** — changes sit in the working tree.

### 2026-07-06 - Upload retry on transient B2 pod failures (v1.0.2)
- **Status**: Done — merged to `master` (`c612df5`), tagged **v1.0.2**, GitHub release published with linux/windows binaries + SHA256SUMS. `go test ./...`/vet/gofmt green. See ADR-018 + bugs.md.
- **Description**: Fixed `bb put -r` aborting mid-batch when a single Backblaze storage pod was briefly unreachable (`connection refused` to a `pod-*.backblaze.com` upload URL). Added `uploadWithRetry` (native.go): up to `b2MaxUploadAttempts`=5, **fresh upload URL each attempt** (new pod), exponential backoff (200ms→3s, ctx-aware); retryable = conn errors + 408/429/5xx + 401 (re-auth), 400/403 fail fast. Wired into small-file `Upload` and large-file `uploadPart` (dropped its unused `auth` param). Tests: retries-then-succeeds, fails-fast-on-400, gives-up-after-5.
- **Notes**: Discovered live while the user was backing up `MANDALATAMA-SEWA/` to SC-OFFICE. Follow-ups still open: make `put -r` continue-past-failures and skip already-uploaded files (`Exists` check) so re-runs are cheap.

### 2026-07-06 - Release ops: v1.0.1 + v1.0.2 published
- **Status**: Done. `v1.0.1` (bb buckets + auto-resolve) and `v1.0.2` (upload retry) both tagged, released on GitHub with `bb-vX-linux-amd64` + `bb-vX-windows-amd64.exe` + `SHA256SUMS` (cross-compiled `CGO_ENABLED=0 -trimpath -ldflags="-s -w"`), README download links bumped each time. Release flow: branch → commit → `merge --no-ff` → annotated tag → push master + tag → build binaries → `gh release create`.

## Pending / Next

- ~~**`rm` flag ordering**~~ — RESOLVED 2026-06-16: flags now work in any position via `parseFlags` (see work log + bugs.md).
- ~~**Daemon watcher + `start`/`stop`**~~ — BUILT 2026-06-16 (working tree; see work log + ADR-012): fsnotify recursive watch + 5-min fallback scan + 1s/5s debounce, graceful shutdown.
- ~~**Web UI / `serve` (port 9171)**~~ — DONE 2026-06-16 (branch `feat/web-ui`; see work log + ADR-015): localhost-only, Host-guarded, watched-folder-confined; Upload/Close + destructive local+remote delete. **This was the last spec feature — backuprepo is now feature-complete against `CLAUDE.md`.**
- ~~**Windows daemon backend**~~ — DONE 2026-06-16 (branch `feat/windows-daemon`; see work log + ADR-014): build-tagged `signals_{unix,windows}.go`; fsnotify already provided `ReadDirectoryChangesW`, so only signals/process control needed splitting. Windows `stop` is forceful.
- ~~**Deletion propagation**~~ — DONE 2026-06-16 (working tree; see work log + ADR-013): opt-in `--delete` on `upload`/`start`, with an unmounted-folder safety guard.
- **Minor follow-ups from final review** — `b2.Uploader.Exists` and the `size` param are unused forward-looking hooks; a couple of cosmetic nits (`copyInto` wrapper, `usage(*os.File)` vs `io.Writer`).
- **`put -r` batch resilience** (2026-07-06) — `cli.Put` still aborts the whole walk on the first per-file error, and re-uploads already-sent files (no `Exists` skip). ADR-018's upload retry handles *transient* failures; making the batch continue-past-failures (report a summary like `bb upload`) + skip unchanged files would make one genuinely-bad file non-fatal and re-runs cheap. `b2.Uploader.Exists` already exists to support the skip.
