# Architectural Decisions

Architectural Decision Records (ADRs) for backuprepo. Numbered sequentially; include date, context, decision, alternatives, and consequences.

> Note: several of these deliberately deviate from the original spec in `CLAUDE.md`, which describes the aspirational design. Where they conflict, **these ADRs reflect what was actually built.**

### ADR-001: Pure-Go SQLite + AES-256-GCM field encryption instead of SQLCipher (2026-06-06)

**Context:**
- `CLAUDE.md` specified SQLCipher (`go-sqlcipher`) for an encrypted DB.
- SQLCipher requires CGO, which conflicts with three other stated goals: single static binary, <10 MB size, and easy cross-compilation to Windows.

**Decision:**
- Use `modernc.org/sqlite` (pure Go, no CGO) for the database.
- Encrypt only the credential fields (keyID, applicationKey) with AES-256-GCM before insertion, in `internal/crypto`.

**Alternatives Considered:**
- `go-sqlcipher` (whole-DB encryption) → Rejected: CGO breaks static binary / small size / cross-compile.
- Pure-Go SQLCipher-format library → Rejected: less mature/maintained.

**Consequences:**
- ✅ Single static binary, no CGO, trivial cross-compile.
- ✅ Credentials never stored in plaintext (verified by `TestConfigEncryptedAtRest`, which reads raw DB bytes).
- ❌ Only credential fields are encrypted, not the whole DB (folder paths/metadata are plaintext).

### ADR-002: Master key stored in a 0600 key file (2026-06-06)

**Context:**
- The daemon (future) must start silently with no passphrase prompt, so the AES key must be available without interaction.

**Decision:**
- Store a random 32-byte key at `~/backup_repo/key` with mode `0600`, created on first run and reused thereafter.

**Alternatives Considered:**
- OS keyring (Credential Manager / GNOME Keyring / Keychain) → Rejected for now: extra dependency; revisit if stronger at-rest protection is needed.
- Machine-derived key (KDF over host attributes) → Rejected: predictable inputs, weak.
- Passphrase each start → Rejected: incompatible with a silent daemon.

**Consequences:**
- ✅ Silent start; simple; fully offline.
- ❌ Anyone who can read the user's home dir can read the key — protects against casual at-rest access only.

### ADR-003: Per-file uploads instead of per-folder tar+gzip (2026-06-06)

**Context:**
- `CLAUDE.md` mentioned tar+gzip compression of watched folders.

**Decision:**
- Upload one local file → one S3 object (`backup.RemoteKey` maps the absolute path to a stable key).

**Alternatives Considered:**
- tar+gzip per folder → Rejected: obscures per-file backup state, which the `list`/`status` commands (and the future web UI's per-file table) depend on.

**Consequences:**
- ✅ Meaningful per-file `last_backup` / `remote_key` tracking and change detection.
- ❌ No cross-file compression; many small files = many objects.

### ADR-004: Address B2 buckets by NAME, not bucket ID (2026-06-06)

> **Superseded in part by ADR-010** (bucket ID also stored, for native B2 list/upload). _(Note added 2026-11-18.)_

**Context:**
- B2 has two identifiers: bucket name and numeric bucket ID. The user initially had the bucket ID.

**Decision:**
- `init` collects and stores the bucket **name** (plus S3 endpoint + region), because the S3-compatible API addresses buckets by name.

**Consequences:**
- ✅ Works with the aws-sdk-go-v2 S3 client directly.
- ❌ Users must look up the bucket name in the B2 console (documented in README).

### ADR-005: Typed errors in `internal/apperr`, not a root `errors.go` (2026-06-06)

**Context:**
- The spec named a root `errors.go` as the typed-error catalog. Go forbids `internal/` packages from importing the root `main` package.

**Decision:**
- Put the shared sentinel errors in `internal/apperr` so every package can import them. Errors wrap these with `%w`.

**Consequences:**
- ✅ `errors.Is(err, apperr.X)` works from any package including `main`.
- ✅ Preserves the "errors are typed, never raw strings" invariant.

### ADR-006: Uploader behind an interface with an in-memory fake (2026-06-06)

**Context:**
- The real B2/S3 upload path can't be unit-tested without live credentials.

**Decision:**
- Define `b2.Uploader` (`Upload`, `Exists`); `S3Uploader` is the real aws-sdk-go-v2 impl, `FakeUploader` is an in-memory map used in tests. `backup.Service` depends on the interface.

**Consequences:**
- ✅ All change-detection/backup logic is unit-tested with the fake.
- ❌ `S3Uploader` itself has no automated test — correctness verified by compilation + manual B2 run (see `issues.md`).

### ADR-007: Accept ~14 MB binary (over the <10 MB goal) (2026-06-06)

**Context:**
- `CLAUDE.md` targets a <10 MB binary. `aws-sdk-go-v2` + `modernc.org/sqlite` are large.

**Decision:**
- Ship the stripped (`-ldflags="-s -w"`) ~14 MB binary for now; do not trade away the SDK or pure-Go SQLite to hit the target.

**Consequences:**
- ✅ Robust, well-maintained S3 client and no-CGO SQLite.
- ❌ Binary exceeds the size goal — flagged for possible future trimming (e.g. a leaner S3 client).
- **Update 2026-11-21:** size accepted at ~22 MB with measured breakdown; see ADR-025.

### ADR-008: Native B2 backend implemented over stdlib net/http, not a B2 SDK (2026-06-06)

**Context:**
- Adding a native Backblaze B2 API client alongside the existing S3-compatible client. Existing SDKs (e.g. `blazer`) would add a significant dependency; the B2 v2 API is simple enough to drive directly.

**Decision:**
- Implement `B2Backend` in `internal/b2/native.go` and `internal/b2/largefile.go` using only stdlib `net/http`, `encoding/json`, and `crypto/sha1`. Target the **B2 v2 API** (`/b2api/v2/...` endpoints). _(Superseded by ADR-011: migrated to v3.)_
- Introduce a unified `b2.Backend` interface (embedding the existing `b2.Uploader`) that both `S3Backend` and `B2Backend` satisfy. `backup.Service` continues to depend only on the narrow `b2.Uploader` view (interface segregation) — it has no need for `Download`, `List`, or `Delete`; manual file commands depend on the wider `Backend`.
- `FakeBackend` (in-memory map) replaces the old `FakeUploader` and implements the full `Backend` interface for tests.

**Alternatives Considered:**
- Third-party B2 Go SDK — Rejected: extra dependency, import cycle risk, maintenance burden.
- Reuse aws-sdk-go-v2 for B2 native path — Not applicable: B2 native API is not S3-compatible.

**Consequences:**
- ✅ Zero new third-party dependencies; small code surface easy to audit.
- ✅ Interface segregation keeps `backup` package unchanged.
- ❌ B2 API field names/headers must match Backblaze's docs exactly — httptest-based tests cover shape but not live auth.

### ADR-009: Backend mode stored in `backend` column; default s3; `--backend` flag for per-command override (2026-06-06)

**Context:**
- Users may want to switch between S3-compatible and native B2 globally, or try one backend for a single command without changing the stored default.

**Decision:**
- Add a `backend TEXT` column to the `config` table. `store.GetBackend`/`store.SetBackend` read and write it; `NULL`/empty defaults to `"s3"` so existing databases behave unchanged.
- `backuprepo backend [s3|b2]` shows or persists the backend.
- Every manual file command (`ls`, `get`, `put`, `rm`, `find`) and `upload` accept a `--backend s3|b2` flag that overrides the stored value for that invocation only.
- Resolution order: `--backend` flag → stored `backend` → `"s3"`.

**Consequences:**
- ✅ Backward-compatible: old DBs without the `backend` column default to `s3`.
- ✅ Per-command override keeps the stored default clean.
- ❌ Two code paths (S3 vs B2) must be kept in sync for any new bucket operation.

### ADR-010: B2 addressed by bucket ID for list/upload, bucket name for download (2026-06-06)

**Context:**
- The Backblaze B2 v2 native API uses **bucket ID** (`BucketID` string) for `b2_list_file_names`, `b2_get_upload_url`, and `b2_start_large_file`, but uses **bucket name** in the download URL path (`/file/<bucket-name>/<key>`). ADR-004 originally stored only the bucket name (sufficient for S3).

**Decision:**
- Extend `store.RemoteConfig` with a `BucketID string` field, persisted to a new `bucket_id TEXT` column in the `config` table.
- `backuprepo init` now prompts for the bucket ID (after the bucket name); it is optional (blank is allowed) so existing S3-only users are not forced to re-init.
- **Update 2026-11-18:** the native client moved to the v3 API (ADR-011), and `init` now auto-resolves the ID from the bucket name, prompting only as a fallback (ADR-017).
- `b2.Config` carries both `BucketName` (S3 + B2 download) and `BucketID` (B2 list/upload); each backend uses only what it needs.
- A schema migration in `store.Open` adds `bucket_id` and `backend` columns to pre-existing databases via `ALTER TABLE config ADD COLUMN ...`.

**Consequences:**
- ✅ Both backends work from the same stored config without separate init flows.
- ✅ Migration is safe for existing users (column defaults to empty string).
- ❌ Users must look up the bucket ID in the B2 console (documented in README and init prompts).

### ADR-011: Migrate native B2 client from v2 to v3 API (2026-06-16)

**Context:**
- The native B2 client (ADR-008) was implemented against the **v2** API (`/b2api/v2/...`), but the dual-backend design spec (`docs/superpowers/specs/2026-06-06-backuprepo-backends-design.md`) specified **v3**. Code and spec disagreed; v3 is Backblaze's current recommended version.

**Decision:**
- Migrate `B2Backend` to the **v3** API. Introduce `b2APIVersion = "v3"` in `native.go` and build all endpoint paths from it (single point of change for future bumps).
- Update `authorize` to parse the v3 `b2_authorize_account` response, where `apiUrl`/`downloadUrl`/`recommendedPartSize` are nested under `apiInfo.storageApi` (only `authorizationToken` stays top-level).
- All other endpoints (`b2_get_upload_url`, `b2_upload_file`, `b2_list_file_names`, `b2_list_file_versions`, `b2_delete_file_version`, the large-file calls, download-by-name) are byte-for-byte compatible between v2 and v3 — only the path segment changed.

**Alternatives Considered:**
- Edit the two spec lines to say v2 (docs → code) → Rejected: the user chose to modernize to v3, the supported current version.

**Consequences:**
- ✅ Code, spec, and key_facts now agree on v3.
- ✅ httptest tests updated to the v3 auth shape and all pass; vet/gofmt clean.
- ❌ Live B2 verification with real credentials still pending (httptest cannot exercise real auth) — folded into the existing "Manual B2 end-to-end test" in `issues.md`. A wrong nested-mapping would fail loudly at authorize, not silently.

### ADR-012: Add fsnotify for the watcher daemon, paired with a fallback scan (2026-06-16)

**Context:**
- The background daemon (roadmap) needs real-time filesystem change detection. `CLAUDE.md` names `fsnotify` (Linux/macOS) and `ReadDirectoryChangesW` (Windows).
- The project leans toward minimal/stdlib dependencies (the native B2 client was hand-rolled over `net/http`, ADR-008) and already exceeds its <10 MB binary goal (~14 MB, ADR-007). Adding any runtime dependency deserves a conscious decision.

**Decision:**
- Add `github.com/fsnotify/fsnotify` (v1.10.1) as the cross-platform filesystem-event source for the daemon's real-time path, in a new `internal/daemon` package, rather than driving OS syscalls directly.
- Pair the event watcher with a 5-minute full-scan fallback (`daemon.FallbackInterval`) so correctness never depends on event delivery: inotify can drop events on queue overflow, on the race before a new dir's watch is added, and entirely while the daemon is down. The fast path gives low latency; the scan guarantees eventual consistency.
- Both paths funnel into the existing `backup.Service.UploadChanged` so change detection has one definition. Event bursts are coalesced by a 1s-quiet / 5s-max-delay debouncer (recorded in `key_facts.md`).

**Alternatives Considered:**
- Hand-roll `golang.org/x/sys/unix` inotify + `ReadDirectoryChangesW` → Rejected: substantial, error-prone per-platform syscall code (watch-descriptor bookkeeping, event coalescing, rename semantics) that reinvents a well-maintained, widely-used library. Unlike ADR-008 — where the B2 API was simple JSON-over-HTTP — the OS event APIs are gnarly enough that the dependency earns its place.
- Poll-only (periodic full scan, no events) → Rejected: a 5-minute latency floor defeats the spec's "catch changes early, keep uploads incremental" goal.

**Consequences:**
- ✅ Cross-platform event watching from one small, pure-Go dependency — no CGO, static binary preserved, binary stays ~14 MB (ADR-007 unchanged in practice).
- ✅ Reuses `UploadChanged`; the daemon is ~one package of glue, not a second change-detection engine.
- ❌ First third-party *runtime* dependency added since the aws-sdk/sqlite baseline — a small supply-chain surface increase.
- ❌ inotify is not recursive: subdirectories are watched individually and new dirs re-watched at runtime (`daemon.addRecursive` + the Create handler). Windows `ReadDirectoryChangesW` is natively recursive, so platform parity needs build-tagged code later (tracked in `issues.md`). **Update 2026-11-18:** not so in practice — fsnotify v1.10.1 disables recursive watches outside its own tests (`enableRecurse = false`), and `addRecursive` adds each directory, so Windows is watched per-directory exactly like Linux (see ADR-014); no build-tagged watcher was needed.

### ADR-013: Deletion propagation is opt-in, scoped to present watched folders, via an optional Deleter (2026-06-16)

**Context:**
- `UploadChanged` was upload-only: a locally deleted file kept its remote object and DB record forever. Roadmap/`issues.md` flagged deletion propagation.
- ADR-006/008 deliberately kept `backup.Service` depending only on the narrow `b2.Uploader` (no `Delete`).

**Decision:**
- Deletion propagation is **opt-in, never default** — `bb upload --delete` and `bb start --delete`. The default keeps retaining backups when local files vanish (the backup-appropriate conservative default; mirrors rsync requiring an explicit `--delete`).
- Add a narrow `b2.Deleter` interface (`Delete` only); `Backend`/`FakeBackend` already satisfy it. `backup.Service` keeps its `b2.Uploader` dependency and gains an **optional** `del b2.Deleter` (nil = off), set via `WithDeleter`. This preserves ADR-008 segregation: backup needs `Delete` only when deletion is enabled.
- Detection (after the upload walk): for each tracked file whose path is under a **currently-existing** watched folder and is now absent on disk (`os.IsNotExist`), delete the remote object by `remote_key`, then remove the local record. Uncertain stat errors (permission/I/O) never trigger deletion. An already-absent remote (`ErrObjectNotFound`) counts as success so retries still clear the record.
- **Safety guard:** skip deletions under any watched folder not currently present as a directory — an unmounted drive must not be read as "the user deleted everything." Covered by `TestDeletionPropagationSkipsMissingFolder`.

**Alternatives Considered:**
- Default-on deletion → Rejected: destroys a backup's core value (recovering deleted files); a footgun.
- Widen `backup.Service` to the full `b2.Backend` → Rejected: breaks ADR-008 segregation; the optional `Deleter` is the minimal extension.
- Drive deletes from fsnotify `Remove` events in the daemon → Rejected for now: scan-based reconciliation is one source of truth for both `upload` and the daemon; an event path could drift, and the fallback scan already converges.

**Consequences:**
- ✅ Safe default preserved; no surprise data loss. Interface segregation intact (optional `Deleter`).
- ✅ Unmounted-drive guard prevents catastrophic mass-deletion; identical behavior for `upload --delete` and `start --delete` (both via `UploadChanged`).
- ❌ Reconciliation is O(tracked files) `Lstat` calls per run — fine at current scale, optimizable later.
- ❌ On a versioned bucket, a propagated delete purges ALL versions (existing backend `Delete` semantics) — irreversible remotely.
- ❌ `unwatch` leaves records that are no longer under a watched folder, so they are never deletion candidates — those remote objects persist (intended: unwatch ≠ delete).
- **Update 2026-11-21:** with `--delete`, daemon Remove events now *schedule a flush*; the scan still decides deletions (ADR-025).

### ADR-014: Cross-platform daemon lifecycle via build-tagged signal files; forceful stop on Windows (2026-06-16)

**Context:**
- The daemon (ADR-012) was Linux-shaped in its *process lifecycle*: `signal.NotifyContext(…, syscall.SIGTERM)`, `bb stop` via `proc.Signal(syscall.SIGTERM)`, and a `processAlive` liveness probe via signal 0 — none valid on Windows.
- The *file-watching* half was already cross-platform: `fsnotify` wraps inotify on Linux and `ReadDirectoryChangesW` on Windows, both per-directory, so `addRecursive` applies on both. The gap was signals/process control, not events.

**Decision:**
- Extract the three OS-specific pieces — `shutdownSignals()`, `signalStop(proc)`, `processAlive(pid)` — into build-tagged files: `signals_unix.go` (`//go:build !windows`) and `signals_windows.go` (`//go:build windows`). `daemon.go` stays platform-agnostic and calls them.
- **Unix:** graceful — Run catches SIGINT/SIGTERM; `signalStop` sends SIGTERM so deferred cleanup runs; liveness via signal 0.
- **Windows:** `shutdownSignals` = `{os.Interrupt}` (foreground Ctrl-C is graceful); cross-process `signalStop` = `proc.Kill()` (forceful `TerminateProcess`); liveness via `os.FindProcess` (fails for dead PIDs on Windows).

**Alternatives Considered:**
- `golang.org/x/sys/windows` named event / job object for graceful Windows stop → Rejected for now: new dependency + more code. The daemon already tolerates abrupt termination (stale PID self-heals via `writePID`, uploads are idempotent), so forceful stop is acceptable for v1. Revisit if graceful Windows `stop` is required.
- `GenerateConsoleCtrlEvent` → Rejected: only works within the same console group; unreliable cross-process.

**Consequences:**
- ✅ `go build`/`go vet` pass for both `GOOS=linux` and `GOOS=windows`; the right file is selected per platform from one codebase. Windows binary links (`PE32+`).
- ✅ Foreground Ctrl-C is graceful on both OSes.
- ❌ `bb stop` on Windows is forceful: the in-flight upload is aborted (retried next run) and the PID file is left stale (overwritten on next start). Documented in README. _(Superseded by ADR-016: Windows `stop` is now graceful via a named event; `proc.Kill` is only the fallback.)_
- ❌ Windows `processAlive` is coarser than Unix's signal-0 (handle-open ≈ alive) — adequate for the double-start guard. **Update 2026-11-18:** stop was superseded by ADR-016 (graceful named stop event); liveness by ADR-024 (`OpenProcess` + exit code `STILL_ACTIVE` + creation-time identity).

### ADR-015: Web UI is localhost-only, unauthenticated, confined to watched folders; delete is local+remote with confirmation (2026-06-16)

**Context:**
- `CLAUDE.md` specifies a port-9171 web UI with **no authentication** ("the user is already authenticated at first launch"), a warm color scheme, a per-file table, breadcrumb folder navigation, a force-upload button, a close button, and a trash-can delete action. "No auth" + a delete button is a security-sensitive combination.

**Decision:**
- New `internal/web` package over **stdlib `net/http` + `html/template`** (no new dependency). `bb serve` runs it in the foreground; `bb start` stays daemon-only (the spec pairs them, but keeping `serve` standalone avoids changing freshly-shipped `start` behavior — user choice, 2026-06-16).
- **Bind `127.0.0.1` only** and reject any request whose `Host` header isn't `localhost`/`127.0.0.1` (DNS-rebinding guard) — the spec's "no auth" is acceptable only because the surface is local.
- **CSRF defense on POST endpoints** (added 2026-06-16 after a security review): the Host guard does NOT stop a cross-site form POST (the browser sends those with the real Host), and with no auth the server trusts any localhost request — so `sameOrigin` requires each POST's `Origin` (or `Referer` fallback) host to equal the server's, failing closed when absent. Browser forms send a matching Origin; an attacker page sends its own and is rejected.
- **Confine browsing and deletion to watched folders** (lexical `filepath.Rel` check, same idea as `get -r`'s `underBase`). A no-auth local server must never list or delete arbitrary paths (`/etc`, `$HOME`).
- The table reads the **local filesystem** (name/type/size/mtime/owner) joined with **backup state** from the store (`Last Backup`). OS owner via build-tagged `owner_unix.go` (uid → `user.LookupId`) / `owner_windows.go` (stub `—`).
- **Delete = local file AND remote object**, unrecoverable (user choice, 2026-06-16). Guarded by a JS `confirm()`; for a directory it purges the remote object of every tracked file beneath it, then removes the local tree. Remote delete happens before local so a failure leaves the file intact.
- **Close** button gracefully shuts the server down (POST `/close` → `srv.Shutdown`), so `bb serve` exits cleanly.

**Alternatives Considered:**
- Add auth / a token → Rejected: contradicts the spec; localhost binding + Host guard is the intended trust model.
- A JS SPA / templating dependency → Rejected: a single server-rendered `html/template` page keeps it dependency-free and tiny.
- Delete remote-only or local-only → user chose local+remote (the most destructive, literal reading); mitigated with a confirm dialog.

**Consequences:**
- ✅ Feature-complete vs the spec; no new dependency; localhost-only, path-confined, Host-guarded.
- ✅ Handlers unit-tested via `httptest` (listing, traversal 403, Host-guard 403, upload, local+remote delete); live-verified end to end.
- ❌ Delete is irreversible by design — the confirm dialog is the only safety net (no trash/undo).
- ❌ No auth at all — safe only on a trusted local machine; anyone with localhost access (or local code running there) can use it.
- CSRF is defended by the `Origin`/`Referer` same-origin check on POSTs (the Host guard alone does NOT cover CSRF — that was an initial gap caught by automated review; see bugs.md 2026-06-16).

### ADR-016: Graceful Windows daemon stop (named event) and no-echo secret entry (2026-06-16)

**Context:**
- Two polish items from earlier ADRs: ADR-014 left Windows `bb stop` *forceful* (`proc.Kill`), losing the daemon's deferred cleanup; and `bb appkey` (the partial-reconfig credential command) read the secret from stdin with terminal **echo** during interactive entry.

**Decision:**
- **Graceful Windows stop via a named event.** On startup the daemon creates a per-PID manual-reset event `Local\backuprepo-daemon-stop-<pid>` and waits on it in a goroutine (`installStopWatcher`, build-tagged). `bb stop` opens that event by the PID from the PID file and `SetEvent`s it; the daemon's `Run` selects on the resulting channel and returns through its normal deferred cleanup (PID file removed, watcher closed). `proc.Kill` remains only as a fallback when the event can't be opened. Unix `installStopWatcher` is a no-op (SIGTERM already graceful). Uses `golang.org/x/sys/windows` — already a transitive dep, no new module. (**Update 2026-11-18:** this change itself promoted `x/sys` to a direct requirement in `go.mod`.)
- **No-echo secret entry.** `cli.readSecret` reads without echo via `golang.org/x/term.ReadPassword` when stdin is an interactive terminal (`term.IsTerminal`), and falls back to a line read otherwise — preserving piping (`pass show … | bb appkey`) and unit tests (which pass a non-`*os.File` reader). Adds the small official `golang.org/x/term` module.

**Alternatives Considered:**
- Windows `GenerateConsoleCtrlEvent` for stop → Rejected: only works within a shared console/process group; the named event is reliable cross-process.
- A localhost control socket / pipe for stop → Rejected: heavier and a wider surface than a kernel event object.
- Hand-rolled termios/console-mode toggling for no-echo → Rejected: `x/term` is the maintained, cross-platform answer for ~one import.

**Consequences:**
- ✅ `bb stop` is graceful on both OSes; interactive `bb appkey` no longer leaks the secret to screen/scrollback. `GOOS=windows` build + vet pass; Linux tests (incl. the existing `appkey` ones via the line-read fallback) green.
- ✅ No new *module* for the Windows stop (`x/sys` already present); one small new module (`x/term`) for no-echo.
- ❌ The graceful-stop and no-echo paths are exercised only on real Windows / real TTY respectively — unit tests cover the fallbacks; the cross-compile build is the gate for the Windows event code.

### ADR-017: Bucket listing (`bb buckets`) + name→ID auto-resolution in `init`/`bucket` (2026-07-06)

**Context:**
- The config stores a **single active bucket** (name + ID). Users with several B2 buckets had no way to *see* them from `bb`, and switching required hand-copying the 24-hex bucket ID from the B2 console into `bb bucket <name> <id>` — error-prone and the source of a real name/ID mismatch (stored name `Scandiumsc` paired with `SC-OFFICE`'s ID). The S3 API can enumerate bucket *names* but not B2 bucket *IDs*, so it can't drive this.

**Decision:**
- **New `bb buckets` command** lists every bucket the credentials can see (NAME / ID / TYPE), marking the active one with `*`. It **always uses the native B2 API** (`b2_list_buckets`) regardless of the configured backend, because only that API returns bucket IDs — the whole point of the command.
- **Kept off the `b2.Backend` interface.** Listing buckets is an *account-level* operation, not a per-bucket one, so it's a standalone `(*B2Backend).ListBuckets` + package-level `b2.ListBuckets(ctx, cfg)`; `FakeBackend`/`S3Backend` are untouched and the `Backend` interface stays clean. Added `b2.BucketInfo{Name,ID,Type}`.
- **Captured `accountId` from `b2_authorize_account`** (previously decoded and discarded) into `b2Auth` — `b2_list_buckets` is account-scoped and requires it.
- **Auto-resolve the bucket ID from its name** in both `bb init` and `bb bucket <name>` via a shared `cli.resolveBucketID`. A `cli.BucketLister` func type is injected into `Init`/`Bucket` (main.go wires `b2.ListBuckets`) so the lookup is stubbable in tests **without network access**. On success the ID is filled automatically (init skips its manual "Bucket ID" prompt); on failure or not-found it **falls back** to the prior behavior (init prompts; `bucket` clears the ID) with an explanatory message. An explicit `bb bucket <name> <id>` always wins — no lookup.

**Alternatives Considered:**
- Add `ListBuckets` to the `Backend` interface → Rejected: forces a meaningless method onto `S3Backend`/`FakeBackend`; account-level op doesn't belong on a per-bucket interface.
- Use S3 `ListBuckets` when the S3 backend is active → Rejected: returns names only, no bucket IDs — useless for `bb bucket`/native upload.
- Call `b2.ListBuckets` directly inside `Init` → Rejected: would fire a live HTTPS request to Backblaze during unit tests; the injected `BucketLister` seam keeps tests hermetic (mirrors the existing `b2.Backend`-injection pattern; no global state).
- Hard-fail init when the bucket name isn't found → Rejected: a bucket-scoped key or transient outage shouldn't block setup; fall back to manual entry.

**Consequences:**
- ✅ Users can browse all buckets (`bb buckets`) and switch in one word (`bb bucket <name>`), with the ID resolved and kept consistent with the name — closing the mismatch failure mode.
- ✅ `Backend` interface unchanged; `var _ Backend = (*B2Backend)(nil)` still holds. New tests: `TestB2ListBuckets`, `TestInitBucketIDFallback`, `TestBucketAutoResolvesID`; full suite + vet + gofmt green.
- ⚠️ `bb buckets` needs a key with account-wide **`listBuckets`** capability. (As first written, this line also claimed a **bucket-scoped** key "sees only its own bucket (still enough to resolve *that* bucket's ID during init)" — that was wrong:) **Update 2026-11-18:** per Backblaze's `b2_list_buckets` docs, a bucket-restricted token must include that bucket's `bucketId` or `bucketName` "or the request will be denied" — `ListBuckets` sends only `accountId`, so with a scoped key auto-resolve most likely failed and init fell back to the manual prompt. **Fixed 2026-11-18:** `init`/`bucket` now use `b2.FindBucket`, which sends `bucketName` in `b2_list_buckets` (works for scoped and account-wide keys); `bb buckets` still lists everything via `b2.ListBuckets` and still needs an account-wide key. See bugs.md 2026-11-18.
- ⚠️ `bb bucket <name>` (no id) changed meaning: it now **auto-resolves** the ID rather than clearing it. It still clears on a failed/not-found lookup, so the old S3-only "clear the ID" path remains reachable when the name isn't in the account.
- **Update 2026-11-21:** lookups fold case and store B2's spelling; `bb buckets` falls back to the configured bucket on an access denial (ADR-025).

### ADR-018: Retry B2 native uploads with a fresh upload URL on transient failures (2026-07-06)

**Context:**
- Backblaze's native API issues a **per-pod upload URL** from `b2_get_upload_url` / `b2_get_upload_part_url`; the file is then POSTed directly to that pod. An individual pod can be briefly unreachable (TCP connection refused/reset/timeout) or return 5xx. The client made a **single attempt**, so one dead pod failed the file — and because `cli.Put`'s `WalkDir` callback returns the error, a whole `bb put -r` batch aborted. Observed live: `pod-050-1046-12.backblaze.com` refused connections while the main API was healthy; re-running intermittently hit the same pod. (S3 SDKs hide pod routing and retry internally, so the `s3` backend never exposed this.)

**Decision:**
- Add **`uploadWithRetry`** (`internal/b2/native.go`) used by both the small-file `Upload` (`b2_upload_file`) and large-file `uploadPart` (`b2_upload_part`). Up to **`b2MaxUploadAttempts` = 5** attempts; **a fresh upload URL is fetched before every attempt** (`getURL` closure) so a retry lands on a new — usually healthy — pod, per Backblaze's documented remedy.
- **Retryable**: transport errors from `http.Do`, and statuses `408/429/500/502/503/504`, plus `401` (an expired upload/auth token — the cached `b.auth` is cleared so the next `getURL` re-authorizes). **Non-retryable** (`400/403`, etc.) fail immediately.
- **Backoff** (`b2Backoff`): 200ms, 400ms, 800ms … capped at 3s, waiting on a `select` over `ctx.Done()` so Ctrl-C aborts instantly.
- `uploadPart` dropped its now-unused `*b2Auth` param (it re-authorizes via `b.authorize` inside `getURL`, enabling the 401→re-auth path). `start_large_file`/`finish_large_file` are left single-attempt (small JSON control calls, not the pod-routed data path).

**Alternatives Considered:**
- Re-POST to the **same** upload URL on failure → Rejected: a dead pod stays dead; only a *new* URL escapes it. This is why `getURL` is inside the retry loop.
- Retry everything including `400/403` → Rejected: client errors can't be fixed by retrying; hammering the server is wrong. Split retryable vs fail-fast, pinned by tests.
- Make `cli.Put` continue past failures / skip already-uploaded files → Deferred (still pending): retry addresses the *transient* cause; batch-resilience + `Exists`-skip are a separate robustness improvement.
- Configurable retry count / backoff → Rejected for now: fixed sane defaults; revisit if needed.

**Consequences:**
- ✅ A single flaky pod no longer fails a file or aborts a `put -r` batch; uploads self-heal transparently. Matches B2's recommended client behavior.
- ✅ Deterministic tests via a flaky-upload httptest server: retries-then-succeeds, fails-fast on 400, gives-up after 5. Full suite + vet + gofmt green; shipped as **v1.0.2**.
- ⚠️ A hard-down account (every pod failing) now takes up to 5 attempts + ~3s of backoff before surfacing the error, instead of failing instantly — an acceptable trade for resilience.
- ⚠️ `bb put` still re-uploads already-sent files on a re-run (no `Exists` skip yet) — harmless overwrite; tracked as a follow-up. **Update 2026-11-18:** both deferred items (continue-past-failures and `--skip-existing`) shipped in v1.0.3 — see ADR-019.

### ADR-019: Resilient `put -r` — continue past failures + opt-in `--skip-existing` (2026-07-06)

**Context:**
- `cli.Put`'s recursive walk returned the first per-file upload error straight out of the `WalkDir` callback, **aborting the whole batch** — one bad/failed file left the rest un-uploaded (surfaced live during a large `bb put -r`). ADR-018's retry fixed *transient* pod failures, but a genuinely bad file (permissions, vanished mid-walk, non-retryable status) still stranded the batch. Re-running also re-uploaded everything, since `bb put` has no change tracking (unlike `bb upload`, which diffs against stored file metadata).

**Decision:**
- **Continue-past-failures is the default for `-r`.** An upload failure is reported (`FAILED <key>: <err>`), counted, and the callback returns `nil` so the walk continues; the run ends with an `Uploaded: X, Skipped: Y, Failed: Z` summary and returns `ErrUploadFailed` (non-zero exit) if any failed. A **filesystem** walk error (the callback's `err` param) still aborts — that's an environment problem, not a per-file one.
- **`--skip-existing` (opt-in)** skips files whose remote object already exists via `be.Exists`. **Presence-only** — it does NOT detect locally-edited files — so it's opt-in and the default stays overwrite; its purpose is cheaply **resuming an interrupted upload**. Applies to both the single-file and recursive paths.
- `FakeBackend` gained a `FailUpload func(key) error` hook so per-file failure is testable in-memory.

**Alternatives Considered:**
- Make `--skip-existing` the default → Rejected: presence-only skipping would silently not re-upload a changed file, giving false "backed up" confidence. Safe default = always overwrite.
- Content-aware skip (size/mtime/sha) for `put` → Deferred: that's what `bb upload` + the `files` table already do; `put` is the stateless manual primitive. Revisit only if a stateful manual sync is wanted.
- Abort on first failure but add `--keep-going` → Rejected: with retry (ADR-018) already absorbing transient blips, a surviving failure is genuinely bad and abandoning the batch is the surprising choice; continue-by-default + non-zero exit is better.

**Consequences:**
- ✅ One bad file no longer strands a `put -r` batch; `--skip-existing` makes re-runs cheap (skip the already-sent). Mirrors `bb upload`'s Uploaded/Skipped/Failed summary style.
- ✅ Tests: `TestPutRecursiveContinuesPastFailure` (2 uploaded, 1 FAILED, error returned), `TestPutRecursiveSkipExisting` (1 skipped, 1 uploaded). Full suite + vet + gofmt green; shipped as **v1.0.3**.
- ⚠️ `put -r` now exits non-zero when *any* file failed even though others succeeded — callers/scripts should read the summary, not just the exit code, to know what got through.

### ADR-020: `bb service` — run the daemon as a generated systemd *user* unit (2026-11-15)

**Context:**
- Users wanted backups of `~/Documents` to run automatically in the background. The README only offered a hand-copied `backuprepo.service` snippet; the daemon (`bb start`, ADR-012/014) runs in the foreground and reads the watch list **once at startup**.

**Decision:**
- New `bb service install [--folder DIR] [--delete] [--force] | uninstall | status` (Linux only; other OSes get `ErrService`). New package `internal/service` renders the unit and drives `systemctl --user` through an injectable `Runner` (faked in tests); `internal/cli/service.go` does bb-specific pre-flight; new sentinel `apperr.ErrService`; new `daemon.RunningPID` liveness helper.
- **User unit, not system unit** — all state (`~/backup_repo` key + DB, ADR-002) belongs to the login user. Unit name stays `backuprepo.service` (what the README already taught). `ExecStart` = absolute, symlink-resolved path of the installing binary, double-quoted with only `%`→`%%` (systemd neither expands nor un-doubles `$` in the executable path). Paths systemd can't run as an executable (`"`, `'`, `\`, control chars — a newline would inject a directive), relative paths and `go run` temp binaries are refused. `Type=exec` so an unexecutable binary fails `restart`.
- **Install order — all checks before any mutation:** OS → configured → no manual daemon holding the PID lock (unless the live daemon *is* the active unit) → resolve folder (default `~/Documents`, `~`-expanded in Go, `Abs`+`Clean`) and require it to be a directory → refuse `--delete` if any stored folder is relative (warn otherwise; a user unit's cwd is `$HOME`, ADR-013 data-loss risk) → `Manager.Preflight` (binary path, unit ownership/`--force`, `systemctl --user show-environment` bus probe) → add to watch list (reusing an equivalent stored entry, since remote keys derive from the stored path string) → write unit atomically → `daemon-reload`, `enable`, `reset-failed`, **`restart`** → wait 2 s and require `is-active` = `active`, else fail with a journalctl pointer.
- **Restart policy:** `Restart=on-failure` (graceful `bb stop`/SIGTERM exits 0 and stays stopped), `RestartSec=30`, `StartLimitBurst=5` in `StartLimitIntervalSec=600` (> 5×30 s so it actually trips). Startup does no network I/O (`b2.NewBackend` is lazy), so startup failures are mostly persistent (not configured / no folders) and giving up is correct. The limit also counts **manual** starts, hence `reset-failed` before every install's `restart`. Known transient exception: a stale PID file whose PID was reused by another process makes the daemon refuse to start ("already running") — see the follow-up below.
- **Ownership marker:** generated units start with `# Managed by \`bb service install\``; install overwrites only marked units (else needs `--force`), uninstall removes only marked units.
- Lingering (`loginctl enable-linger`) is **printed as a hint, not run** — it changes system login policy.

**Alternatives Considered:**
- System unit with `User=` → Rejected: needs root, and HOME/state discovery under a system unit is fragile.
- `Restart=always` → Rejected: would resurrect the daemon after a deliberate `bb stop`.
- No start limit / infinite retries → Rejected: an unconfigured DB would crash-loop forever; startup does no network I/O, so the only transient startup failure is the stale/reused PID case (tracked as a follow-up), not enough to justify it. **Update 2026-11-18:** the reused-PID case is resolved by ADR-023 (`daemon.start` identity; `writePID` overwrites a stale pair). Residual: a `daemon.pid` with no `daemon.start` (left by bb ≤ 1.0.6) still falls back to a plain liveness probe.
- Detach/fork inside `bb start` → Rejected: the service manager should own process lifecycle; foreground + `Type=exec` sends stdout to journald for free.

**Consequences:**
- ✅ One command (`bb service install`) gives automatic background backup of `~/Documents` at every login; logs via `journalctl --user -u backuprepo`.
- ✅ Tests: `internal/service` 88.8% coverage (render/quoting, systemctl call order incl. reset-failed + health check, preflight/bus failure, hand-written-unit guard, refused exe paths, dropped-`--delete` note, unsupported OS, status/uninstall paths); `cli` pre-flight cases (nothing mutated on any failed check, relative-folder guard); `main.TestParseServiceArgs`; `daemon.TestRunningPID`. Units rendered by the Go code for plain, space, `$` and `%` paths all pass `systemd-analyze --user verify` (systemd 262).
- ⚠️ Follow-up (pre-existing, sharpened by unattended starts): PID liveness is a signal-0 probe only, so a reused PID can block startup and make `bb stop` signal an unrelated process. Fix: on Linux also check `/proc/<pid>/exe` is bb. The install error now names the PID file instead of prescribing `bb stop`. Also: `bb watch` still stores relative paths verbatim — consider storing `filepath.Abs`. _(Resolved by ADR-023: PID start-time identity on Linux — not `/proc/<pid>/exe` — and absolute watch paths.)_
- ⚠️ Folders added later with `bb watch` need `systemctl --user restart backuprepo` (daemon reads folders once). Without lingering the service stops at logout.
- ⚠️ Not exercised against a live user manager in CI/tests (would back up real files); verified only via fake runner + `systemd-analyze verify` + sandboxed-HOME CLI runs.
- **Update 2026-11-21:** opt-in `bb service install --linger` runs `loginctl enable-linger <uid>` (warns on failure); the hint shows only while lingering is off (ADR-025).

### ADR-021: `init` picks a working backend; state-free `version`/`help`; `rm` names the bucket (2026-11-15)

**Context:**
- Field use of v1.0.4 (see bugs.md 2026-11-15): `init` saved an empty endpoint/region on the default `s3` backend (uploads failed); `init` without a terminal gave a misleading "required" error; `bb version` created `~/backup_repo`; `bb rm` output was mistaken for local deletions.

**Decision:**
- **`init` validates against the backend, before saving.** Blank endpoint *or* region (after deriving region from `https://s3.<region>.backblazeb2.com`, and adding a missing `https://`) → store backend `b2`, which needs only keyID/appKey/bucket ID; with no bucket ID either, refuse (`ErrInvalidCredentials`) without saving. A supplied endpoint+region leaves the stored backend unchanged (default `s3`) — **except** when the stored backend is `b2` and this init has no bucket ID: `SaveConfig` keeps the backend column, so that would leave an unusable `b2`; init switches to `s3` and says so (review finding S1). Endpoints are normalised to `scheme://host[:port]` (path dropped) and region matching is case-insensitive. This supersedes the README's old "leave them blank, then run `bb backend b2`" two-step.
- **Input exhaustion ≠ blank answer.** `init` records EOF; missing required answers after EOF → new sentinel `apperr.ErrNoInput` ("input ended before setup was complete (run `bb init` in an interactive terminal)"). A terminal check (`term.IsTerminal`) was rejected because piping answers into `init` is a legitimate scripted setup (and how the tests drive it).
- **`version`/`help` exit before `config.Load`/`store.Open`.** Other commands still create state on first run (by design, ADR-002).
- **`cli.Rm` takes the bucket name** (from `store.GetConfig`) purely for messages: prompt, `deleted (remote) <key>` lines, a closing summary that says local files were not touched, and a partial-failure count in the error.

**Alternatives Considered:**
- Re-prompt for endpoint/region until valid → Rejected: B2-only users legitimately have no S3 values; picking `b2` is the right outcome, not an error.
- Default every new config to `b2` → Rejected: changes behaviour for existing S3 users and ADR-009's default.
- Keep `deleted <key>` and only add a summary → Rejected: the per-line text is what was misread.

**Consequences:**
- ✅ The README's "leave endpoint/region blank" advice now produces a working config; the Windows quick start's `bb backend b2` step becomes a check.
- ⚠️ `rm` output text changed (`deleted (remote) …` + summary) — scripts grepping `^deleted ` still match the prefix, but exact-line matches would not.
- ⚠️ Re-running `init` with blank endpoint/region on an existing S3 setup switches it to `b2` (announced in the output).

### ADR-022: `bb init` reads the applicationKey without echo, through its shared reader (2026-11-15)

**Context:**
- ADR-016 made `bb appkey` no-echo, but `bb init` still read the applicationKey with its plain line prompt, so the secret appeared on screen and in scrollback during first-time setup (observed in real use; logged as a v1.0.5 follow-up).
- `Init` reads all answers through one `bufio.Reader`. The existing `readSecret(prompt, in, out)` builds its own reader, which on piped input finds nothing (the first reader has already buffered every line) and shifts every later answer.

**Decision:**
- New `cli.readSecretFrom(prompt, r *bufio.Reader, in, out)`: if `in` is an `*os.File` terminal (and `r.Buffered() == 0`), read via `readHidden` → `term.ReadPassword` (no echo); otherwise read the next line from `r`. On piped input it returns `io.EOF` with whatever was read, so `Init` keeps its input-ended detection (`ErrNoInput`). `readSecret` (used by `appkey`) is now a wrapper that treats EOF-after-answer as success.
- The `r.Buffered() == 0` check is **defensive**: a canonical-mode terminal returns one line per `read()`, so `r` almost never holds typeahead there (pasting all answers at once still goes through `ReadPassword`, in order — verified on a pty). If bytes ever are buffered, they're read from `r` rather than skipped.
- **Ctrl-C at the hidden prompt** (review finding S1): `term.ReadPassword` restores echo in a `defer` that never runs if SIGINT kills the process, leaving the terminal/console not echoing. `readHidden` saves `term.GetState`, and on `os.Interrupt` during the read restores it and exits 130 (the blocked `read()` can't be cancelled). Also fixes the same latent issue in `bb appkey`, and restores the Windows console mode.

**Alternatives Considered:**
- Call `readSecret(…, in, out)` from `Init` → Rejected: desyncs piped answers (second buffered reader).
- Drop the shared reader and read the fd byte-by-byte everywhere → Rejected: more code, and tests drive `Init` with `strings.Reader`.

**Consequences:**
- ✅ The **terminal path is verified only by a manual pty check** (Python `pty` driving a scratch `cli.Init` harness; no automated test, because test hooks would need package-level state): keyID echoed, secret hidden and stored in full (20/20 chars); old code printed the secret; Ctrl-C → exit 130 with echo restored (was: killed, echo left off); paste-ahead → answers in order. Unit tests cover the **piped** path: answer ordering (a mutation calling `readSecret` from Init fails `TestInitPipedSecretStaysInOrder`), EOF handling, and `readSecret`.
- ⚠️ A pasted multi-line block that includes the secret is still shown (echoed by the terminal at paste time) — unavoidable without raw-mode input.
- ⚠️ On a Unix terminal, **Ctrl-D at the hidden prompt is ignored** (x/term returns `(0, nil)` on tty EOF and keeps reading), so it does not produce `ErrNoInput`; that error remains a non-interactive-stdin signal (ADR-021). Same as `bb appkey` before.
- ⚠️ `bb appkey`: an `io.EOF` from the terminal read (possible on Windows) now yields "empty applicationKey" instead of a bare EOF error; non-EOF errors from piped reads are now returned instead of ignored. Windows console behaviour is vetted/built, not run.

### ADR-023: Absolute watch paths, PID-file start-time identity, no state for unknown commands (2026-11-15)

**Context:**
- `bb watch` stored its argument verbatim; a relative entry means a different folder to the daemon/systemd service (cwd = `$HOME`), and `bb unwatch` needed the exact stored text (a user had to type a trailing `/`).
- The PID file held only the PID; liveness was "some process has that PID" (signal 0). After an unclean shutdown and PID reuse, `bb start` refused to run, and **`bb stop` would SIGTERM an unrelated process**.
- Unknown commands opened config + store (creating `~/backup_repo`) before printing usage.

**Decision:**
- **Folders:** `cli.normalizeFolder` (expand `~`/`~/`/`~\`, `filepath.Abs`; **empty or blank input is rejected** — `filepath.Abs("")` is the cwd, so `bb watch "$UNSET"` would otherwise watch the current directory, review finding S1) for `watch`, `unwatch`, init's folder prompt (via `Watch`) and `service install` (shared `expandHome`; `ensureWatched` folded into `Watch`). `Watch` reuses an equivalent stored entry, and if a **legacy relative entry** resolves to the same folder from the cwd it reports "Already watching <legacy>" with conversion instructions instead of adding the absolute twin (S3). `Unwatch` tries the exact stored text, then any equivalent spelling of an absolute entry (legacy relative entries match only by their exact text), and needn't stat the folder. **Existing entries are not migrated** — object names derive from walked paths: a trailing-slash twin only walks the same files twice, but relative → absolute changes every object name (full re-upload).
- **PID identity:** `daemon.pid` stays the **bare PID** (the format every bb version parses), and a separate `daemon.start` holds `<pid> <starttime>` — starttime is field 22 of `/proc/<pid>/stat` (parsed after the last `)` since the comm field may contain spaces/parens). The start time is used only when its PID matches `daemon.pid`, so a `daemon.pid` rewritten by an older bb is never paired with a stale start time. (A first draft put both in `daemon.pid`; review finding S2 showed older binaries then call it corrupt — `bb stop` fails and an old `bb start` could launch a second daemon.) `alive()` = process exists AND (no matching/readable start time OR start times match). `writePID` writes `daemon.start` then `daemon.pid` and overwrites stale files; `RunningPID` ignores them; **`Stop` removes a stale pair and returns `ErrDaemon` instead of signalling**. Build-tagged `proc_linux.go` / `proc_other.go`.
- **Unknown commands:** `main.isCommand` allowlist checked before `config.Load` (a function, not a package var, per the no-global-state rule). `TestIsCommandMatchesDispatch` parses `run`'s `switch cmd` and fails if a dispatched command isn't allowlisted (mutation-checked).

**Alternatives Considered:**
- Check `/proc/<pid>/exe` or cmdline is `bb` → Rejected: `make install` replaces the binary under a running daemon (`exe` reads `… (deleted)`), and argv[0] varies (`bb`, full path, `backuprepo`). Start time is the standard PID-reuse guard.
- Migrate stored relative folders to absolute → Rejected: changes remote object names; `service install` already warns/refuses (`--delete`) for relative entries (ADR-020).
- Lazily open the store only for commands that need it → Rejected for now: larger refactor of `run`; the allowlist fixes typos with a drift test.

**Consequences:**
- ✅ A stale PID file can no longer block `bb start` or make `bb stop` kill another program (Linux). Tests: `TestReusedPIDIsNotTheDaemon`, `TestStopNeverSignalsReusedPID` (the reused PID is the test process itself, so a regression would kill the test binary), `TestStopSignalsMatchingProcess` (real child `sleep` is SIGTERMed), `TestStartFileForOtherPIDIgnored`, `TestParseStatStartTime`, `TestLegacyPIDFileStillRecognised`; `TestWritePIDRecordsStartTime` asserts `daemon.pid` is still parseable by older versions. Manually: a v1.0.6 binary's `bb stop` correctly stopped a process recorded by the new layout.
- ⚠️ Tiny race: `Stop` may delete the files of a daemon that starts in the instant between its liveness check and the removal (same class as the existing read-check-write race in `writePID`).
- ✅ `bb watch docs/` / `~/Documents` store absolute paths; `bb unwatch` accepts any spelling. Tests in `cli_test.go`; sandboxed CLI run confirmed.
- ⚠️ **macOS and Windows keep the old PID behaviour** _(Windows superseded by ADR-024)_ (no start time recorded → signal-0 / `FindProcess` liveness). Windows `stop` falls back to `proc.Kill` if the named event can't be opened, so PID reuse there could still hit another process — process creation time via `GetProcessTimes` would fix it (follow-up).
- ⚠️ Not exercised with a live `bb start` (would contact Backblaze); `writePID`/`Stop` are unit-tested.
- **Update 2026-11-21:** usage/flag errors no longer create state either: `run` validates before `config.Load` (ADR-025).

### ADR-024: Windows PID identity via creation time; case-insensitive (path-only) folder matching (2026-11-15)

**Context:**
- ADR-023 left two Windows gaps: no process start time (so a reused PID could still be treated as the daemon — and Windows `bb stop` falls back to `proc.Kill`), and case-sensitive folder matching (`C:\Users\X` vs `c:\users\x` → duplicate entry, double upload).
- Found while doing it: Windows `processAlive` = "`os.FindProcess` succeeded", but a Windows process object outlives the process while any handle to it is open, so an exited daemon could look alive.

**Decision:**
- `proc_windows.go`: `procStartTime` = creation time from `GetProcessTimes` (opened with `PROCESS_QUERY_LIMITED_INFORMATION`), formatted in 100 ns units — the same `daemon.start` mechanism as Linux (ADR-023). `proc_other.go` now covers only `!linux && !windows` (macOS).
- Windows `processAlive`: `OpenProcess` + `GetExitCodeProcess == STILL_ACTIVE (259)` (constant declared locally; not in x/sys). No new dependency (`golang.org/x/sys/windows` already direct, ADR-016).
- Folder matching (`cli.findWatched`, `findLegacyRelative`): entries match if their cleaned paths are equal — `strings.EqualFold` on Windows (`sameFolder`, OS passed in for testability). **Path-only**: works for folders that no longer exist (so `unwatch` of a deleted folder works), never stats, and never equates a symlink with its target.
- `readPIDFile` rejects PIDs above `math.MaxUint32` (they would wrap to an unrelated PID in Windows' uint32 process APIs).

**Alternatives Considered:**
- Fold case on macOS too → Rejected: APFS can be case-sensitive, so case-insensitive macOS volumes keep exact matching (same as before).
- Also match by `os.SameFile` (first draft) → **Rejected after review**: (B1) a stored symlink root isn't descended by `filepath.WalkDir`, so it backs up nothing — and SameFile then answered "Already watching <link>" to `bb watch <real>`, blocking the workaround (silent backup gap); (S1) Windows file IDs aren't guaranteed unique on ReFS / some redirectors, so two folders could be equated and `unwatch` could remove the wrong one; plus a stat of every stored folder could hang on a dead network mount.
- Windows: verify the image name (`QueryFullProcessImageName`) is bb → Rejected for the same reasons as `/proc/<pid>/exe` in ADR-023 (binary replaced on upgrade; names vary).

**Consequences:**
- ✅ PID-reuse protection (start/stop never act on another program) now on Linux and Windows; exited-but-referenced Windows processes are no longer "alive".
- ✅ No duplicate watch entries from case differences on Windows. Regression test `TestWatchRealFolderWhenSymlinkStored` keeps symlink and target distinct (B1).
- ⚠️ **Windows code was not executed** — no Windows machine/Wine/CI here. Verified only by `GOOS=windows go vet`, compiling the Windows test binaries (`go test -c`) and review against Win32 docs. On a Windows `go test`, the Windows-only tests (`proc_windows_test.go`: creation time, exited-but-referenced process not alive; `cli_windows_test.go`: case-insensitive watch/unwatch incl. after the folder is removed) run, and the shared start-time tests (`TestWritePIDRecordsStartTime`, `TestReusedPIDIsNotTheDaemon`, `TestStopNeverSignalsReusedPID` — which would kill the test binary on regression — and `TestStopSignalsMatchingProcess` if `sleep` is on PATH) stop skipping.
- ⚠️ `STILL_ACTIVE` (259) is ambiguous for a process that genuinely exits with code 259 — benign here (PID not reused while a handle is open, and start-time check still applies).
- Follow-ups: web UI delete uses an exact-case prefix check (`web.deletePath`) though `resolveWatched` is case-insensitive on Windows — a mixed-case path could delete locally but leave remote objects/records (old code, only via a crafted POST past the CSRF guard); `EqualFold` ≠ NTFS upcase table and Windows per-directory case-sensitivity isn't honoured; a Windows CI job would close the execution gap.
- **Update 2026-11-21:** the web-delete case follow-up is fixed, and Windows code now runs in CI (ADR-025).

### ADR-025: Validate before state; scoped-key and case-folding follow-ups; open items closed by decision (2026-11-21)

**Context:**
- The 2026-11-18 audit and the v1.0.8 wrap-up left open items. Some needed code (below); the rest were "decide, then record".
- Backblaze's Buckets page: "Bucket names are not case sensitive, even though they can include upper-case letters." The API pages don't say whether the `b2_list_buckets` `bucketName` filter folds case.

**Decision:**
- **State only for a valid command line.** `run` checks arguments, flags and `--backend`/`bb backend` values first, and only then loads config and opens the store. Usage and flag errors exit 1 without creating `~/backup_repo` (extends ADR-021/023). Errors found only while the command runs (e.g. `bb watch /missing`) still come after state exists. Everything after `--` is positional.
- **Bucket names fold case.** `resolveBucketID` matches B2's bucket list case-insensitively and stores B2's spelling, which the S3 API needs. If B2's `bucketName` filter is exact, a wrongly-cased name simply isn't found, which is the previous behaviour.
- **`bb buckets` with a scoped key.** A typed B2 status error matches the new `apperr.ErrAccessDenied` on 401/403. Only on that error does `bb buckets` fall back to `FindBucket` for the configured bucket, and it says why (ADR-017 update).
- **`bb status`** adds the daemon state (from the PID file) and, on Linux, the service state. Both are best-effort, and the service probe has a 2 s timeout because bare `bb` runs status.
- **`bb service install --linger`** runs `loginctl enable-linger <uid>`, but only after a successful install, so a failed install leaves login policy alone. It is opt-in, and a failure only prints a warning. Without the flag, the login hint shows only while lingering is off (ADR-020 update: lingering is still never changed without being asked). `bb status`'s service line accepts only systemd state words; anything else, such as a "Failed to connect to user scope bus" message, reads "unknown".
- **Daemon Remove events** schedule a flush when `--delete` is on. The reconciliation scan still decides what is deleted, so ADR-013's single source of truth stands. The event only makes the scan happen sooner. Trade-off: the grace period before an irreversible all-versions purge shrinks from "next event or 5 min" to the 1 s quiet window. A tool that unlinks a file and recreates it more than about 1 s later gets the old versions purged, then re-uploads the file as new. This is acceptable because `--delete` is opt-in and already documented as destructive.
- **S3 `DeleteObjects`** goes in batches of 1000 keys, and any per-key `Errors` entry is an `ErrDeleteFailed`. It stops at the first failed batch, so with more than 1000 versions a failure after the first batch can leave the key resolving to an older version. That failure is reported, and a retry finishes the job.
- **The web delete** matches records the way the OS compares paths (case-insensitively on Windows) and uses each record's stored `RemoteKey` (ADR-024 follow-up). A record that matches only by folding must also name the same directory or file on disk (`os.SameFile`). In a per-directory case-sensitive folder, deleting `Docs` therefore never purges the backups of a sibling `docs`, and an unconfirmable match keeps its backup. Unlike ADR-024's rejected use, `SameFile` only *confirms* a name match here, so a failed check errs toward keeping data. The remaining edge is a ReFS file-ID collision reporting two folders as one.
- **Closed by decision, no code:**
  - **Binary size:** accepted at about 22 MB (ADR-007 update). Measured symbol sizes are about 4.5 MB for aws-sdk-go-v2 and about 2.0 MB for modernc SQLite; the rest is mostly the stdlib (net/http, TLS, crypto). Getting under 10 MB would mean dropping the S3 backend or the pure-Go SQLite (ADR-001/002), which isn't worth it.
  - **Windows per-directory case sensitivity** (`fsutil … setCaseSensitiveInfo`, WSL folders) and the NTFS upcase table are not honoured. Folder matching folds case on all Windows paths, and `EqualFold` is close to NTFS's own folding for real-world names. Accepted limitation.
  - **Bare `bb` prints `status`** and setup is always `bb init`. This is intended: a double-click or a typo must never start interactive setup or a daemon. It records the divergence CLAUDE.md flagged as having no ADR. `bb status` now reports daemon state, which closes the other half of that gap.
  - **`b2.Uploader.Exists`** stays: `put --skip-existing` uses it, and `backup.Service` doesn't need it because it uses its own size+mtime+hash state. The `copyInto` wrapper was inlined.

**Alternatives Considered:**
- Delete directly from fsnotify Remove events → Rejected again for the reasons in ADR-013.
- Fall back in `bb buckets` on any error → Rejected: it would hide network and credential errors behind a partial listing.
- Run `loginctl enable-linger` on every install → Rejected: it changes login policy (ADR-020).

**Consequences:**
- ✅ Usage, flag and backend-name mistakes leave no state behind. Bucket names typed in any case resolve. Scoped keys get a useful `bb buckets`.
- ✅ The Windows-only code paths are now run by CI (`windows-latest`). The first run found Linux-only fixtures in the `service` tests; they now skip off Linux.
  - **Update 2026-11-21:** superseded by ADR-026 — CI moved to the local GoCD; the Windows tests are compiled there but no longer run.
- ⚠️ Still not covered: whether B2's `bucketName` filter folds case, and a real scoped-key run (the user's key is account-wide).
- ⚠️ `bb backend` now rejects more than one argument. It used to ignore the extras.

### ADR-026: CI on the local GoCD (andromeda) instead of GitHub Actions; Windows tests compiled, not run (2026-11-21)

**Context:**
- The owner doesn't use GitHub for CI. andromeda already runs GoCD (server + one Linux agent) for the Sentral apps and rentalmdl, building from bare mirrors under `/var/lib/gocd-repos` (pattern: rentalmdl `deploy/gocd/README.md`).
- ADR-025 added a `windows-latest` job; it found real Windows-only test failures (bugs.md 2026-11-21). GoCD has no Windows agent.

**Decision:**
- `.gocd/backuprepo.gocd.yaml` (config repo `backuprepo`, own pipeline group `backuprepo`, rule limited to that group) is the only CI; `.github/workflows/ci.yml` is deleted. `origin` (GitHub) and GitHub releases are unchanged.
- One `verify` stage, through Makefile targets so CI = local (only `go mod verify` and the hook-test script are called directly): `make lint` (vet linux/windows/darwin + gofmt), `make coverage`, `make wintest` (`GOOS=windows go test -c -o wintest/ ./...`: compiles **and links** every Windows test binary), `make build` + `make build-windows`. No `-race` (needs cgo; the build is `CGO_ENABLED=0`). No coverage threshold (none existed).
- The mirror follows `master` via one tracked hook script (`deploy/gocd/mirror-hook`) installed as post-commit/post-merge/post-rewrite; it only ever warns and exits 0, and only this clone, only `master`, writes the mirror (force-push, single writer).
- Release gate: a passed GoCD run that **built** the exact commit being tagged, i.e. `material_revisions[0].modifications[0].revision` (GoCD folds commits landing between polls into one run and lists them all; only `[0]` was built). Replaces `gh run list --commit`.

**Alternatives Considered:**
- Keep a Windows-only GitHub Actions job → Rejected by the owner (no GitHub CI).
- Add a Windows GoCD agent now → Deferred: no Windows host for it; can be added later as a job with a `windows` resource.
- Inline build commands like rentalmdl → Rejected: Makefile targets keep one source of truth and avoid GoCD's literal-hash parse trap.

**Consequences:**
- ✅ CI runs locally, with no external service; every check is reproducible with the same `make` targets.
- ⚠️ **Windows tests no longer execute anywhere** — only compiled/linked and vetted. Regressions like the 2026-11-21 NTFS case-folding test failure would not be caught. Run `go test ./...` on a Windows machine before a release that touches build-tagged Windows code (`internal/daemon/*_windows.go`, `internal/web/owner_windows.go`, `cli_windows_test.go`) or case-folding logic.
- ⚠️ Feature branches get no CI (only `master` is mirrored); the release gate moved from "CI on the branch before merge" to "GoCD on the commit being tagged".
- ⚠️ The mirror and the hooks are machine-local; a fresh clone needs `deploy/gocd/README.md` steps 1–2 again.

### ADR-027: Per-folder bucket prefix (`watch --remote`) and adopting identical remote objects (2026-11-21)

**Context:**
- ADR-003 names every object after the file's absolute path (`home/me/Documents/X/a.pdf`). The owner needs real-time backup of `~/Documents/MANDALATAMA-SEWA` into an existing bucket folder `SC-OFFICE/MANDALATAMA-SEWA/` (88 files, Object Lock on, keep all versions), alongside `~/Documents` (Obsidian) in the same bucket. `bb put <local> <remote> -r` can target a prefix but is one-shot, and its `--skip-existing` misses edited files.
- With Object Lock, every unneeded upload is a locked version that can't be deleted, so the 88 files must not be uploaded again after a `bb get` into the local folder.

**Decision:**
- New column `folders.remote_prefix TEXT NOT NULL DEFAULT ''` (migrated with a generalized `addMissingColumns`). `bb watch <dir> --remote P` stores it normalized (`/`-separated, no leading `/`, trailing `/`; empty, `.`/`..`, control chars refused); on a watched folder it changes the prefix. A prefix overlapping another folder's prefix or its default `RemoteKey(folder)/` names is refused (objects would overwrite each other).
- `backup.KeyFor(folders, path)`: the **innermost** watched folder containing path decides; prefix → `P + rel`, no prefix → `RemoteKey(path)` (unchanged, so existing users see no re-upload). The walk skips nested watched roots so each file is visited once. Used by upload, `PendingCount` and the web delete fallback.
- A record counts as backed up only under the key `KeyFor` gives now (an empty stored key, from before keys were recorded, matches only the default `RemoteKey(path)`). A prefix change therefore re-uploads under the new names; old objects are kept. `watch --remote`, a plain `watch` nested in a prefixed folder, and a nested `unwatch` print that files will be uploaded again; a plain `watch` whose default names collide with a prefixed folder is refused too. Nested roots are recognized by absolute path (legacy relative entries too), case-insensitively on Windows.
- **Adopt:** for a file not backed up under its key, if the backend implements the optional `b2.ContentHasher` (native B2: `contentSha1` from `b2_list_file_names`) and the remote SHA-1 equals the local file's SHA-1, record it as backed up without uploading (`Result.Adopted`). Any doubt → upload: no capability (S3), not found, `none`/`unverified:` hashes, lookup error. A failed lookup is counted (`Result.HashLookupsFailed`) and shown on the daemon scan line and in `bb upload` — uploading (a possibly needless locked version) was preferred over retrying later (a key without `listFiles` would otherwise never back up new files). Edits to a file already backed up under its key always upload.

**Alternatives Considered:**
- A timed `bb put -r` job → Rejected: not real-time; either misses edits or re-uploads everything (new locked versions each run).
- Size-only "already there" check → Rejected: an edited file of equal size would never be backed up.
- Make `bb get` register downloaded files in the DB → Not chosen: only covers files fetched by `bb`, and couples manual commands to the watch DB; the hash check covers it and the prefix-change case.
- Outermost folder wins for nested folders → Rejected: the inner `--remote` would be ignored for exactly the folder it was set on.

**Consequences:**
- ✅ `~/Documents/MANDALATAMA-SEWA` syncs in real time into `MANDALATAMA-SEWA/`; `bb get` + watch adopts the 88 existing objects (probe 2026-11-21: all 88 report a 40-hex `contentSha1`, and a download matched it).
- ✅ No behavior change for folders without a prefix (same keys, same records).
- ⚠️ Adopting costs one `b2_list_file_names` (class C) call per file that is new to its key; `PendingCount` still counts such files (telling them apart would need that call too).
- ⚠️ Large files uploaded through the large-file API report `contentSha1` "none", so they are uploaded again once instead of adopted.
- ⚠️ Path matching uses `filepath.Rel` (case-insensitive on Windows); Windows tests are compiled, not run (ADR-026) — run `go test ./...` on Windows before releasing this.

### ADR-028: Mapped folders sync both ways (`mapto`, `sync --all/--folder`); newest wins, never delete, daemon pulls every 5 min (2026-11-22)

**Context:**
- The owner wants `bb mapto <bucket-folder>` (current folder ↔ bucket folder, creating it if missing), `bb sync --all` (whole bucket ↔ current folder) and `bb sync --folder F`, with remote changes (colleagues using the B2 web UI) coming down too. Owner's choices (2026-11-21): **newest wins, never delete**; `sync --all` is a two-way mirror; **the daemon pulls every 5 minutes**; `mapto` + `unmap` + `maps`, keeping `watch --remote`; whole bucket mirrored in `~/SC-OFFICE`; one release. SC-OFFICE has Object Lock **with a default retention** (owner, 2026-11-22), so every needless upload is a locked version.
- Probe (read-only, 2026-11-22): SC-OFFICE holds 140 current objects (2025-SEWA/ 52, MANDALATAMA-SEWA/ 88), all with `fileId` + 40-hex `contentSha1`, none with `src_last_modified_millis`, largest 45 MB.

**Decision:**
- A folder with a `remote_prefix` (ADR-027) is a **mapped** folder kept in two-way sync by new package `internal/syncer`; `store.RootPrefix` ("/") maps a folder to the whole bucket (`KeyFor` → relative path). The one-way uploader (`backup.UploadChanged`) and `--delete` now handle **plain folders only**; ADR-027's adopt stays for them.
- Per-file **snapshot** in `files` (new columns `remote_file_id`, `remote_sha1`, `remote_time`): local changed = size/mtime then SHA-256 differ; remote changed = `fileId` differs (else SHA-1, else upload time vs last backup). A pure `Decide` maps (snapshot, local, remote) to None/Record/Upload/Download/KeepLocal/KeepRemote/Forget: one-sided change copies; both changed → newer wins (`localWins`; remote time = `src_last_modified_millis` if set, else upload time); same bytes → record only; missing on one side → restore (never delete); gone on both → forget.
- A losing local version is **moved to `~/backup_repo/conflicts/<time>/<rel>`** (a local edit never uploaded isn't in B2's history). Uploads set `src_last_modified_millis` (`b2.ModTimeUploader`: header / large-file fileInfo / S3 metadata); downloads get the remote time via `Chtimes`, so they don't look newer.
- **Safety:** downloads stream into `.bb-sync-*.tmp` beside the target (walks skip these), verify the listed SHA-1, and are renamed only if the local file didn't change/appear since the decision; a missing mapped root is never re-created; remote names that are absolute or contain `..`/`\` are refused; `.bzEmpty` markers are never transferred. Daemon passes run under a mutex (event flushes and the ticker could overlap before); `bb sync` refuses while the daemon runs (except `--dry-run`).
- **Mapping rules:** no two mapped folders share bucket objects; a whole-bucket mirror can't coexist with other mapped folders (two local copies would fight); plain folders coexist and their key spaces are excluded from the mirror in both directions, as are nested watched folders. `mapto` creates `<P>/.bzEmpty` when the bucket folder is empty and restarts an active systemd service. Sync requires the `b2` backend (S3 lists no fileId/SHA-1).
- **When sync runs:** daemon passes (each debounced change event and the 5-minute scan = pull), `bb upload`, the web UI's Upload button, and `bb sync`.

- **After independent review (2026-11-22), all reproduced and fixed with regression tests (mutation-checked):** (1) conflicts go to `conflicts/<time>/<folder name>/<rel>` and `setAside` never overwrites (`name (2).ext`); (2) a watched folder nested in a mapped one is excluded remotely too (`prefix + rel(nested)/`), not only skipped locally; (3) a cross-process **sync lock** (`~/backup_repo/sync.lock`, flock / LockFileEx, released on exit): `bb sync` fails with "busy", `bb upload`/web/daemon skip; (4) **`bb mapto` no longer restarts the service** — deviates from the owner's 2026-11-21 choice because the restarted daemon performed the first sync at once, defeating the `--dry-run` preview (since 2026-11-23 `mapto` refuses while the daemon runs, see below; `unmap` still restarts); `mapto`/`sync` refuse re-mapping an already mapped folder and any folder containing `~/backup_repo`, and refuse the S3 backend; (5) **refill guard:** if every snapshot file of a mapped folder is missing locally the run is refused unless `bb sync --refill`. Also: the time fallback of `remoteChanged` uses the upload time (a colleague's old source time no longer hides an upload); SHA-1 values other than 40-hex (`none`, `unverified:`) are treated as unknown, and bb's own not-yet-listed upload with an unknown hash is matched by size (no download-back of large files); Windows-invalid names (`:` etc.) are refused; the web UI prints sync results to the `bb serve` terminal. Delta re-review: `freeName` is bounded (any stat error other than not-exist fails; over-long names are shortened; 1000 tries max) — it could loop forever on a 255-byte name and hang the daemon under its lock; the refill guard fires only when some snapshot file still exists remotely (files gone on both sides are forgotten); `bb watch <dir> --remote P` now goes through `mapto`'s guards (no in-place re-map — supersedes ADR-027's "changes the prefix"); the S3 "not synced" warning prints once at daemon start. **Second delta re-review (2026-11-23):** fix (4) was incomplete — the daemon re-reads mapped folders on every pass (`SyncMapped` → `ListWatched`), so a mapping made while it ran was synced within seconds/5 minutes, before any preview; `MapTo` (hence `mapto` and `watch --remote`) now **refuses while the daemon runs** (`daemon.RunningPID(stateDir)`), like `bb sync`. `MapTo` also lists the bucket folder before storing the mapping (a bucket error leaves nothing behind). Regression tests (real daemon via `cli.Start`, plain-watched refusal, List failure, S3 warning, `main.go` routing AST check, multi-byte/long-extension `freeName`) — 7 mutations, all caught. Not fixed (accepted, logged in issues.md): the refill guard can be bypassed when every synced object was renamed remotely (no data loss); `--refill` applies to every mapped folder.

**Alternatives Considered:**
- Keep both versions as `name (conflict).ext` inside the folder → Rejected by the owner (newest wins); the conflicts folder outside the synced tree keeps the loser without syncing it.
- Mirror deletions → Rejected by the owner (never delete).
- Pull only on `bb sync` → Rejected by the owner (daemon pulls every 5 min).
- Compare by SHA-1 only → Rejected: `fileId` is exact per version, free in the listing, and present on every object (large files report SHA-1 `none`).

**Consequences:**
- ✅ `~/SC-OFFICE` can mirror SC-OFFICE both ways; edits sync within seconds up and ~5 min down.
- ⚠️ Deleting a file on one side doesn't stick (restored at the next sync); delete both sides together (web UI trash can); on SC-OFFICE (default retention) B2 refuses to delete locked versions, so a synced file can't be removed until its retention ends.
- ⚠️ Each daemon pass lists every mapped prefix (class C; ~1 call per 1000 objects); heavy editing triggers more passes.
- ⚠️ Newest-wins compares clocks of different machines; `localWins` is a plain `After` pending the owner's tie/skew policy (TODO in `internal/syncer/newest.go`).
- ⚠️ A file bb uploaded is matched by size until its next listing when B2 reports no SHA-1 (large files): a colleague's same-size upload in that short window would be taken for bb's own and missed.
- ⚠️ Objects uploaded before v1.0.13 have no source time, so for them "newer" means "uploaded later".
- ⚠️ Not executed on Windows (ADR-026); conflicts folder copy fallback (cross-filesystem) has little test coverage.

