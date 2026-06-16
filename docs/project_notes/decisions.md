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
- ❌ `bb stop` on Windows is forceful: the in-flight upload is aborted (retried next run) and the PID file is left stale (overwritten on next start). Documented in README.
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
