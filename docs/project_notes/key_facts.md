# Key Facts

Non-sensitive project configuration and constants for backuprepo. **Never store secrets here** (no keyID/applicationKey, no master key). Real credentials live encrypted in `~/backup_repo/backup.db`; the master key lives in `~/backup_repo/key`.

### Repository

- Module path: `backuprepo`
- **Naming convention (2026-06-16):** all **user-facing** text — CLI usage/errors/prompts (`apperr` messages are prefixed `bb:`), and the web UI pages — says **`bb`** (the installed binary). The Go **module path, package names, project-name doc comments, and the state dir `~/backup_repo`** stay **`backuprepo`** (the project name). Don't "fix" the module name to `bb` — it would break every import. The internal Windows stop-event id (`Local\backuprepo-daemon-stop-<pid>`) is also intentionally left as-is (not user-visible).
- Remote: `github.com:PT-Scandium/backuprepo.git` (default branch: `master`)
- Language/toolchain: **Go 1.25+** (see `go.mod`), no CGO
- License: MIT (PT-Scandium)

### Build

- Build command: `go build -ldflags="-s -w" -o backuprepo .`
- **Makefile**: `make` builds a single static binary named **`bb`** (short for Backblaze) with `CGO_ENABLED=0 -trimpath -ldflags="-s -w"`; `make install` copies it to `$(PREFIX)` (default `~/.local/bin`). Other targets: `clean`, `uninstall`, `test`, `vet`, `fmt`, `tidy`, `help`. The binary name does not change behavior.
- Stripped binary size: ~21 MB (see decisions.md ADR-007; grew with fsnotify + x/term); statically linked (no CGO), cross-compiles cleanly to Windows.
- Test suite: `go test ./...`

### Versioning

- **Source of truth:** the root `VERSION` file (e.g. `1.0.0`). It is **embedded into the binary** via `//go:embed` in `main.go`; `bb version` (`--version`/`-v`) prints it. Released as git tag `v<VERSION>` (e.g. `v1.0.0`) — first release tagged 2026-06-16.
- **Scheme** (`internal/version`, odometer): `major.minor.patch`, each component `0..20`; incrementing past 20 wraps to 0 and carries into the next-higher component — `1.0.20 → 1.1.0`, `1.20.20 → 2.0.0`. **Major is the top component and unbounded.** Fully unit-tested.
- **Bump:** `make bump` (patch, with carry), `make bump-minor`, `make bump-major` — or `go run ./cmd/bump [major|minor|patch]`. The tool rewrites `VERSION`; rebuild to bake the new value into the binary.

### Local state layout (`~/backup_repo/`)

- `backup.db` — SQLite (`modernc.org/sqlite`); credential fields AES-256-GCM encrypted; tables: `config` (single row), `folders`, `files`
- `key` — 32-byte random master key, mode `0600`, created on first run

### Constants

- Multipart upload threshold (S3 backend): **100 MB** (≤100 MB → single PutObject via transfer manager; >100 MB → S3 multipart)
- B2 native small-file limit (`b2SmallFileLimit`): **100 MB** (≤100 MB → single `b2_upload_file`; >100 MB → B2 large-file multipart via `b2_start_large_file` / `b2_upload_part` / `b2_finish_large_file`)
- Web UI port (`web.Addr`, built): **9171** — localhost-only (`127.0.0.1`), no auth, Host-header guard + `Origin`/`Referer` CSRF check on POSTs; `bb serve`
- Daemon fallback full-scan interval (`daemon.FallbackInterval`): **5 minutes** (built)
- Daemon debounce (defaults in `daemon.New`, tunable via `Daemon` fields): **1 s** quiet window (reset on every event) + **5 s** max-delay cap (starvation guard); re-scan-all granularity
- Deletion propagation: **opt-in** via `--delete` on `upload`/`start` (off by default). Removes remote object + local record for tracked files deleted under a still-present watched folder; **skipped entirely if the watched folder is missing** (unmount guard). See ADR-013.

### Storage backends

- **`s3`** (default) — S3-compatible Backblaze B2 endpoint via `aws-sdk-go-v2`; addresses bucket by **name**.
- **`b2`** — Native Backblaze B2 **v3 API** (`/b2api/v3/...`) over stdlib `net/http`; addresses bucket by **ID** for list/upload, by **name** for download. The `b2_authorize_account` response nests `apiUrl`/`downloadUrl`/`recommendedPartSize` under `apiInfo.storageApi` (v3 shape); its top-level `accountId` is captured into `b2Auth` for account-scoped calls (`b2_list_buckets`). API version lives in `b2APIVersion` in `native.go`.
  - **Upload retry (v1.0.2, ADR-018):** `uploadWithRetry` retries both `b2_upload_file` and `b2_upload_part` up to **`b2MaxUploadAttempts` = 5** times, fetching a **fresh upload URL each attempt** (B2 issues a per-pod URL; a pod can be transiently unreachable). Retryable = connection errors + `408/429/5xx` + `401` (re-auths); `400/403` fail fast. Backoff 200ms→3s, ctx-aware.
- **Account-level (native only):** `b2.ListBuckets(ctx, cfg)` / `(*B2Backend).ListBuckets` list all buckets (name + ID + type). Deliberately **not** on the `b2.Backend` interface (per-bucket ops only); backs `bb buckets` and the `init`/`bucket` ID auto-resolution. See ADR-017.
- Stored in `backend TEXT` column of `config` table; `NULL`/empty defaults to `"s3"`.
- Switch with `bb backend [s3|b2]`; override per-command with `--backend s3|b2`.

### Backblaze B2 configuration

Collected by `bb init`, stored in `backup.db`:
- keyID (access key ID) — *secret-adjacent; stored encrypted, not here*
- applicationKey — *secret; stored encrypted, not here*
- Bucket **name** — used by S3 API and B2 native download (URL path `/file/<bucket-name>/<key>`)
- Bucket **ID** — used by B2 native API for list and upload (`b2_list_file_names`, `b2_get_upload_url`); optional for S3-only users
- S3 endpoint URL, e.g. `https://s3.us-west-004.backblazeb2.com`
- Region = the middle hostname segment, e.g. `us-west-004`

Partial reconfig without a full `init` (all require existing config):
- `buckets` — **list all buckets** the credentials can see (NAME / ID / TYPE; active bucket marked `*`). Always uses the **native B2 API** (`b2_list_buckets`) regardless of the configured backend, since only it returns bucket IDs. Needs a key with account-wide **`listBuckets`** capability; a bucket-scoped key sees only its own bucket. See ADR-017.
- `bucket [<name> [<id>]]` — show/switch the destination bucket (name + ID only). With `<name>` **only**, the bucket ID is **auto-resolved** from the account (native `b2_list_buckets`); pass `<id>` to set it explicitly (no lookup). On a failed/not-found lookup it clears the ID (old S3-only behavior). See ADR-017.
- `appkey [<new-keyID>]` — replace the applicationKey, **read from stdin** (never argv/shell history); optional keyID rotates the whole pair. Interactive entry is **no-echo** (via `golang.org/x/term`); piped/non-terminal input reads a line (so `pass show … | bb appkey` and tests work). Empty → `ErrInvalidCredentials`; the secret is never echoed back (masked via `mask`). See ADR-016.

`bb init` also **auto-resolves the bucket ID** from the bucket name (skipping its manual "Bucket ID" prompt) when the credentials can list buckets; it falls back to prompting on failure/not-found. See ADR-017.

> **Credential gotcha:** the **keyID** (applicationKeyID, ~25 chars, on the App Keys page) is NOT the **Account ID** (12 hex chars, on the account dashboard). Entering the account ID as the keyID makes `b2_authorize_account` return **401 → `ErrAuthFailed`** ("authentication failed: status 401"). `keyName` is a cosmetic B2 label and is **not stored or used** by `bb` — only keyID + applicationKey authenticate. Fix a bad pair with `bb appkey <keyID>` (no full re-init). See bugs.md 2026-07-06.

### Manual bucket commands

Available once configured; all accept `--backend s3|b2`:
- `ls [path] [-r]` — list objects; non-recursive groups folders as prefixes
- `get <remote> [local] [-r]` — download object(s); `-r` downloads all under a prefix
- `put <local> [remote] [-r] [--skip-existing]` — upload file(s); `-r` uploads a directory tree. In `-r`, a failed file is reported + counted but does NOT abort the batch (summary `Uploaded/Skipped/Failed`, non-zero exit if any failed). `--skip-existing` skips files already present remotely (presence-only; for resuming). See ADR-019.
- `rm <path> [-r] [-f|-y]` — delete object(s); confirms unless `-f`/`-y`; `-r` deletes prefix
- `find <query> [prefix]` — case-insensitive substring search of object keys
- `backend [s3|b2]` — show or set the stored backend

### Package map (`internal/`)

- `apperr` — typed sentinel errors (imported by all packages)
- `crypto` — AES-256-GCM `Seal`/`Open`
- `config` — `~/backup_repo` paths + master-key file
- `store` — SQLite persistence (encrypted creds, folders, files, backend mode); `SetBucket` switches the destination bucket name + ID; `SetCredentials` re-encrypts a new applicationKey (+ optional keyID) in one statement — all without touching the rest of the config
- `b2` — `Backend` interface (embeds `Uploader`), `S3Backend`, `B2Backend`, `FakeBackend`; `NewBackend` factory
- `backup` — folder walk + change detection + upload orchestration (depends on `b2.Uploader`; optional `b2.Deleter` via `WithDeleter` enables opt-in deletion propagation)
- `daemon` — background watcher (built 2026-06-16): recursive fsnotify watch + 5-min fallback scan + 1s/5s debounce; `start`/`stop` lifecycle (PID file `~/backup_repo/daemon.pid`). Runs on **Linux + Windows** — OS-specific signal/stop/liveness logic lives in build-tagged `signals_unix.go` / `signals_windows.go` (ADR-014); `stop` is graceful on both — Unix via SIGTERM, Windows via a named stop event (forceful `proc.Kill` fallback), see ADR-016. Depends on `store` + `b2.Uploader` via `backup.Service`.
- `cli` — subcommand handlers incl. `Ls`/`Get`/`Put`/`Rm`/`Find`/`Backend`/`Bucket`/`Buckets`/`SetAppKey` + `Start`/`Stop`/`Serve` (io injected for testability). `Init`/`Bucket` take a `BucketLister` (wired to `b2.ListBuckets` in main.go) so the shared `resolveBucketID` name→ID lookup is stubbable without network. See ADR-017.
- `web` — localhost web UI (`bb serve`, port 9171): `html/template` page over stdlib `net/http`; lists watched folders' contents with backup state, Upload + Close buttons, 🗑️ delete (local + remote). `127.0.0.1`-only, no auth, Host-header guard + `Origin`/`Referer` CSRF check on POSTs; browsing/deletion confined to watched folders. OS owner via build-tagged `owner_{unix,windows}.go`. See ADR-015.
- root `main.go` — dispatch (incl. `start`/`stop`/`serve`/`bucket`/`buckets`/`appkey`) + per-command FlagSet + effective-backend factory (`buildBackend`) + `b2ConfigFromStore` helper

### Reference docs

- Core design spec: `docs/superpowers/specs/2026-06-06-backuprepo-core-design.md`
- Core implementation plan: `docs/superpowers/plans/2026-06-06-backuprepo-core.md`
- Dual-backend design spec: `docs/superpowers/specs/2026-06-06-backuprepo-backends-design.md`
- Dual-backend implementation plan: `docs/superpowers/plans/2026-06-06-backuprepo-backends.md`
- User guide: `README.md`
