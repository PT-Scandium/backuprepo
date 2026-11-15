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
- ❌ inotify is not recursive: subdirectories are watched individually and new dirs re-watched at runtime (`daemon.addRecursive` + the Create handler). Windows `ReadDirectoryChangesW` is natively recursive, so platform parity needs build-tagged code later (tracked in `issues.md`).

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
- ❌ Windows `processAlive` is coarser than Unix's signal-0 (handle-open ≈ alive) — adequate for the double-start guard.

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
- **Graceful Windows stop via a named event.** On startup the daemon creates a per-PID manual-reset event `Local\backuprepo-daemon-stop-<pid>` and waits on it in a goroutine (`installStopWatcher`, build-tagged). `bb stop` opens that event by the PID from the PID file and `SetEvent`s it; the daemon's `Run` selects on the resulting channel and returns through its normal deferred cleanup (PID file removed, watcher closed). `proc.Kill` remains only as a fallback when the event can't be opened. Unix `installStopWatcher` is a no-op (SIGTERM already graceful). Uses `golang.org/x/sys/windows` — already a transitive dep, no new module.
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
- ⚠️ `bb buckets` and auto-resolve need a key with account-wide **`listBuckets`** capability; a **bucket-scoped** key sees only its own bucket (still enough to resolve *that* bucket's ID during init).
- ⚠️ `bb bucket <name>` (no id) changed meaning: it now **auto-resolves** the ID rather than clearing it. It still clears on a failed/not-found lookup, so the old S3-only "clear the ID" path remains reachable when the name isn't in the account.

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
- ⚠️ `bb put` still re-uploads already-sent files on a re-run (no `Exists` skip yet) — harmless overwrite; tracked as a follow-up.

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
- No start limit / infinite retries → Rejected: an unconfigured DB would crash-loop forever; startup does no network I/O, so the only transient startup failure is the stale/reused PID case (tracked as a follow-up), not enough to justify it.
- Detach/fork inside `bb start` → Rejected: the service manager should own process lifecycle; foreground + `Type=exec` sends stdout to journald for free.

**Consequences:**
- ✅ One command (`bb service install`) gives automatic background backup of `~/Documents` at every login; logs via `journalctl --user -u backuprepo`.
- ✅ Tests: `internal/service` 88.8% coverage (render/quoting, systemctl call order incl. reset-failed + health check, preflight/bus failure, hand-written-unit guard, refused exe paths, dropped-`--delete` note, unsupported OS, status/uninstall paths); `cli` pre-flight cases (nothing mutated on any failed check, relative-folder guard); `main.TestParseServiceArgs`; `daemon.TestRunningPID`. Units rendered by the Go code for plain, space, `$` and `%` paths all pass `systemd-analyze --user verify` (systemd 262).
- ⚠️ Follow-up (pre-existing, sharpened by unattended starts): PID liveness is a signal-0 probe only, so a reused PID can block startup and make `bb stop` signal an unrelated process. Fix: on Linux also check `/proc/<pid>/exe` is bb. The install error now names the PID file instead of prescribing `bb stop`. Also: `bb watch` still stores relative paths verbatim — consider storing `filepath.Abs`. _(Resolved by ADR-023: PID start-time identity on Linux — not `/proc/<pid>/exe` — and absolute watch paths.)_
- ⚠️ Folders added later with `bb watch` need `systemctl --user restart backuprepo` (daemon reads folders once). Without lingering the service stops at logout.
- ⚠️ Not exercised against a live user manager in CI/tests (would back up real files); verified only via fake runner + `systemd-analyze verify` + sandboxed-HOME CLI runs.

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
- ⚠️ **macOS and Windows keep the old PID behaviour** (no start time recorded → signal-0 / `FindProcess` liveness). Windows `stop` falls back to `proc.Kill` if the named event can't be opened, so PID reuse there could still hit another process — process creation time via `GetProcessTimes` would fix it (follow-up).
- ⚠️ Not exercised with a live `bb start` (would contact Backblaze); `writePID`/`Stop` are unit-tested.
