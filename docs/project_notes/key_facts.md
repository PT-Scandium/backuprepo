# Key Facts

Non-sensitive project configuration and constants for backuprepo. **Never store secrets here** (no keyID/applicationKey, no master key). Real credentials live encrypted in `~/backup_repo/backup.db`; the master key lives in `~/backup_repo/key`.

### Repository

- Module path: `backuprepo`
- **Naming convention (2026-06-16):** all **user-facing** text — CLI usage/errors/prompts (`apperr` messages are prefixed `bb:`), and the web UI pages — says **`bb`** (the installed binary). The Go **module path, package names, project-name doc comments, and the state dir `~/backup_repo`** stay **`backuprepo`** (the project name). Don't "fix" the module name to `bb` — it would break every import. The internal Windows stop-event id (`Local\backuprepo-daemon-stop-<pid>`) is also intentionally left as-is (not user-visible).
- Remote: `github.com:PT-Scandium/backuprepo.git` (default branch: `master`)
- Language/toolchain: **Go 1.25.10+** (`go.mod`: `go 1.25.10`), no CGO
- Direct deps (`go.mod`): `aws-sdk-go-v2` (core, config, credentials, feature/s3/manager, service/s3), `fsnotify`, `golang.org/x/sys`, `golang.org/x/term`, `modernc.org/sqlite`
- License: MIT (PT-Scandium)

### Build

- Build command: `make` → `./bb`, or `CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bb .` (without `CGO_ENABLED=0` a cgo-enabled host produces a dynamically linked binary)
- **Makefile**: `make` builds a single static binary named **`bb`** (short for Backblaze) with `CGO_ENABLED=0 -trimpath -ldflags="-s -w"`; `make install` copies it to `$(PREFIX)` (default `~/.local/bin`). Other targets: `all`/`build`, `version`, `clean`, `uninstall`, `test`, `coverage` (writes `coverage.out`, prints the total), `vet`, `lint` (vet for linux/windows/darwin + fails on unformatted files), `fmt` (rewrites files), `tidy`, `help`, `wintest` (Windows test binaries into `wintest/`, not run), `build-windows` (`bb.exe`) (+ bump targets below). **CI** (2026-11-21, ADR-026): **GoCD on andromeda** (`http://127.0.0.1:8153/go`), pipeline `backuprepo` in group `backuprepo`, config repo `backuprepo` = `.gocd/backuprepo.gocd.yaml` read from the bare mirror `/var/lib/gocd-repos/backuprepo.git` (`edisonch:go`, setgid). This clone's `post-commit`/`post-merge`/`post-rewrite` hooks (`deploy/gocd/mirror-hook`, untracked copies in `.git/hooks`) push `master` there; other branches get no CI. Stage `verify`: `go mod verify`, `make lint`, hook tests, `make coverage` (total printed, no threshold), `make wintest` (Windows test binaries compiled + linked, not run: Linux agent only), `make build` + `make build-windows`. Agent Go = Fedora `/usr/bin/go`. Run history: `curl -H @~/.config/gocd/claude.token … /go/api/pipelines/backuprepo/history`. GitHub Actions removed the same day. `/verify` test lanes: `.gstack/pregate.json` (runner `scripts/pregate-go-test.sh`, which runs `go test` per package); pregate reads the policy from `origin/master`, so it applies once merged. The binary name does not change behavior.
- Stripped binary size: ~22 MB (22,577,312 bytes for v1.0.8–v1.0.11, 22,597,792 for v1.0.12 linux-amd64; see decisions.md ADR-007, which accepted ~14 MB; grew with fsnotify + x/term); statically linked (no CGO), cross-compiles cleanly to Windows.
- Test suite: `go test ./...`

### Versioning

- **Source of truth:** the root `VERSION` file (e.g. `1.0.0`). It is **embedded into the binary** via `//go:embed` in `main.go`; `bb version` (`--version`/`-v`) prints it. Released as git tag `v<VERSION>` (e.g. `v1.0.0`) — first release tagged 2026-06-16.
- **Scheme** (`internal/version`, odometer): `major.minor.patch`; minor and patch run `0..20`, and incrementing past 20 wraps to 0 and carries into the next-higher component — `1.0.20 → 1.1.0`, `1.20.20 → 2.0.0`. **Major is unbounded**: `Bump` never wraps it and `Parse` accepts any non-negative major (fixed 2026-11-18 — `Parse` used to cap major at 20, so a VERSION of `21.0.0` written by a bump could not be bumped again). `internal/version` 100% covered; `cmd/bump` has end-to-end tests (63.2%: `main`/write-error untested).
- **Bump:** `make bump` (patch, with carry), `make bump-minor`, `make bump-major` — or `go run ./cmd/bump [major|minor|patch]`. The tool rewrites `VERSION`; rebuild to bake the new value into the binary.
- **Current release: v1.0.12** (2026-11-21). **Release process:** feature branch → independent review → `make lint coverage` locally → `--no-ff` merge to `master` → `chore: bump version to X` → **passed GoCD `backuprepo` run that built that exact commit** (`material_revisions[0].modifications[0].revision` = its SHA; GoCD folds commits between polls and sees only `master`, ADR-026) → if `*_windows.go` or case-folding code changed: `go test ./...` on Windows → annotated tag `vX` (`vX — <summary>`) → GitHub release (`gh release create vX --verify-tag --latest`) with `bb-vX-linux-amd64`, `bb-vX-windows-amd64.exe`, `SHA256SUMS`, built from the tag with `CGO_ENABLED=0 -trimpath -ldflags="-s -w"` → `docs: vX — README links + notes` → install the published Linux asset to `~/.local/bin/bb` (a local rebuild differs: Go embeds `vcs.revision`).

### Local state layout (`~/backup_repo/`)

- `backup.db` — SQLite (`modernc.org/sqlite`); credential fields AES-256-GCM encrypted; tables: `config` (single row), `folders`, `files`
- `key` — 32-byte random master key, mode `0600`, created on first run
- `daemon.pid` — running daemon's PID (bare number, readable by every version); `daemon.start` — `<pid> <starttime>` on Linux/Windows for PID-reuse protection (ADR-023/024). Both removed on clean shutdown.

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
  - **Upload retry (v1.0.2, ADR-018):** `uploadWithRetry` retries both `b2_upload_file` and `b2_upload_part` up to **`b2MaxUploadAttempts` = 5** times, fetching a **fresh upload URL each attempt** (B2 issues a per-pod URL; a pod can be transiently unreachable). Retryable = connection errors + `408/429/500/502/503/504` + `401` (re-auths); every other upload-POST status fails fast (a failure fetching the upload URL / re-authorizing is always retried). Backoff 200→400→800→1600 ms between the 5 attempts (3 s cap never reached), ctx-aware.
- **Account-level (native only):** `b2.ListBuckets(ctx, cfg)` / `(*B2Backend).ListBuckets` list all buckets (name + ID + type) — needs an account-wide key. `b2.FindBucket(ctx, cfg)` / `(*B2Backend).FindBucket` look up only `cfg.BucketName` by sending `bucketName` in `b2_list_buckets`, which B2 requires for bucket-scoped keys; `init`/`bucket` ID auto-resolve uses it. Deliberately **not** on the `b2.Backend` interface (per-bucket ops only); `ListBuckets` backs `bb buckets` (which falls back to `FindBucket` for the configured bucket when the list is denied: a typed status error matches `apperr.ErrAccessDenied` on 401/403); `FindBucket` backs the `init`/`bucket` ID auto-resolution, which matches names case-insensitively and stores B2's spelling. See ADR-017/025.
- Stored in `backend TEXT` column of `config` table; `NULL`/empty defaults to `"s3"`.
- Switch with `bb backend [s3|b2]`; override for one `ls`/`get`/`put`/`rm`/`find` with `--backend s3|b2` (`upload`/`start` reject it).

### Backblaze B2 configuration

Collected by `bb init`, stored in `backup.db`:
- keyID (access key ID) — *secret-adjacent; stored encrypted, not here*
- applicationKey — *secret; stored encrypted, not here*
- Bucket **name** — used by S3 API and B2 native download (URL path `/file/<bucket-name>/<key>`)
- Bucket **ID** — used by B2 native API for list and upload (`b2_list_file_names`, `b2_get_upload_url`); optional for S3-only users
- S3 endpoint URL, e.g. `https://s3.us-west-004.backblazeb2.com`
- Region = the middle hostname segment, e.g. `us-west-004`

Partial reconfig without a full `init` (all require existing config):
- `buckets` — **list all buckets** the credentials can see (NAME / ID / TYPE; active bucket marked `*`). Always uses the **native B2 API** (`b2_list_buckets`) regardless of the configured backend, since only it returns bucket IDs. Needs a key with account-wide **`listBuckets`** capability — B2 denies an unfiltered list (401) for a bucket-scoped key; such keys can still use `bb bucket <name>` / `init`, which look up just that bucket. See ADR-017.
- `bucket [<name> [<id>]]` — show/switch the destination bucket (name + ID only). With `<name>` **only**, the bucket ID is **auto-resolved** from the account (native `b2_list_buckets`); pass `<id>` to set it explicitly (no lookup). On a failed/not-found lookup it clears the ID (old S3-only behavior). See ADR-017.
- `appkey [<new-keyID>]` — replace the applicationKey, **read from stdin** (never argv/shell history); optional keyID rotates the whole pair. Interactive entry is **no-echo** (via `golang.org/x/term`); piped/non-terminal input reads a line (so `pass show … | bb appkey` and tests work). Empty → `ErrInvalidCredentials`; the secret is never echoed back (masked via `mask`). See ADR-016.

`bb init` reads the applicationKey **without echo** on an interactive terminal (same `term.ReadPassword` path as `appkey`, but through init's shared buffered reader so piped answers stay in order; fixed 2026-11-15, after v1.0.5). `bb init` (v1.0.5+) adds `https://` to a bare endpoint host, derives a blank region from `s3.<region>.backblazeb2.com`, and **switches the backend to `b2`** when endpoint or region end up blank (refuses without a bucket ID). With no interactive stdin it fails with `ErrNoInput`. `bb version`/`help` never touch `~/backup_repo`. See ADR-021.

`bb init` also **auto-resolves the bucket ID** from the bucket name (skipping its manual "Bucket ID" prompt) when the credentials can list buckets; it falls back to prompting on failure/not-found. See ADR-017.

> **Credential gotcha:** the **keyID** (applicationKeyID, ~25 chars, on the App Keys page) is NOT the **Account ID** (12 hex chars, on the account dashboard). Entering the account ID as the keyID makes `b2_authorize_account` return **401 → `ErrAuthFailed`** ("authentication failed: status 401"). `keyName` is a cosmetic B2 label and is **not stored or used** by `bb` — only keyID + applicationKey authenticate. Fix a bad pair with `bb appkey <keyID>` (no full re-init). See bugs.md 2026-07-06.

### Watched folders

- Stored as **absolute, cleaned paths** (`cli.normalizeFolder`: `~`/`~/` expanded, `filepath.Abs`, empty path refused) since 2026-11-15; older entries are kept verbatim (not migrated — remote object names derive from walked paths). `watch` reuses an equivalent stored entry (same cleaned path, case-insensitive on Windows; matching is path-only — no stat, symlinks are distinct); `unwatch` matches the exact stored text first, then any equivalent spelling. Unknown commands (typos) exit 1 before `~/backup_repo` is created. See ADR-023.
- **Object names:** default = absolute path without the leading `/` (`backup.RemoteKey`, ADR-003). A folder watched with `bb watch <dir> --remote P` (column `folders.remote_prefix`, `''` = none, stored ending in `/`) stores files as `P/<path relative to dir>` (`backup.KeyFor`; the **innermost** watched folder decides; nested watched roots are skipped by the outer walk). A record backed up under a key other than the one `KeyFor` now gives counts as changed (a prefix change re-uploads); an empty stored `remote_key` (pre-key records) matches only the default name. Failed SHA-1 lookups fall back to an upload and are counted (`Result.HashLookupsFailed`, shown on the scan line and in `bb upload`). Plain `watch` also refuses names colliding with a prefixed folder and notes re-keyed files. New-to-key files are **adopted** (recorded, not uploaded) when the native B2 `contentSha1` equals the local SHA-1 (`b2.ContentHasher`, B2 backend only). ADR-027.

### Mapped folders / two-way sync (ADR-028)

- `mapto <P>` (cwd ↔ `P/`, creates `P/.bzEmpty` if empty; refuses while the daemon runs — it would sync the new mapping on its next pass before any preview), `unmap [dir]` (restarts an active service), `maps`, `sync [--all|--folder F] [--dry-run] [--refill]` (`--all`: `folders.remote_prefix = "/"` = whole bucket). One syncing process at a time: `~/backup_repo/sync.lock` (OS lock). Package `internal/syncer` (`Decide` pure; `Folder`/`All` execute). Needs backend `b2`; `bb sync` refuses while the daemon runs (not with `--dry-run`).
- Snapshot columns in `files`: `remote_file_id`, `remote_sha1`, `remote_time` (ms). Conflict losers → `~/backup_repo/conflicts/<YYYYMMDD-HHMMSS>/<folder name>/<rel>` (never overwritten: `name (2).ext`). Download temp files `.bb-sync-*.tmp` (skipped by every walk). Uploads set `src_last_modified_millis`.
- Runs on every daemon pass (change events + 5-min scan), `bb upload`, web Upload. One-way uploader + `--delete` touch plain folders only.

### Manual bucket commands

Available once configured; all except `backend` accept `--backend s3|b2`:
- `ls [path] [-r]` — list objects; non-recursive groups folders as prefixes
- `get <remote> [local] [-r]` — download object(s); `-r` downloads all under a prefix
- `put <local> [remote] [-r] [--skip-existing]` — upload file(s); `-r` uploads a directory tree. In `-r`, a failed file is reported + counted but does NOT abort the batch (summary `Uploaded/Skipped/Failed`, non-zero exit if any failed). `--skip-existing` skips files already present remotely (presence-only; for resuming). See ADR-019.
- `rm <path> [-r] [-f|-y]` — delete object(s); confirms unless `-f`/`-y`; `-r` deletes prefix; **bucket only, never local files** — output `deleted (remote) <key>` + `Deleted N object(s) from bucket "<name>"` summary (v1.0.5)
- `find <query> [prefix]` — case-insensitive substring search of object keys
- `backend [s3|b2]` — show or set the stored backend

### Package map (`internal/`)

- `apperr` — typed sentinel errors (imported by all packages)
- `crypto` — AES-256-GCM `Seal`/`Open`
- `config` — `~/backup_repo` paths + master-key file
- `store` — SQLite persistence (encrypted creds, folders, files, backend mode); `SetBucket` switches the destination bucket name + ID; `SetCredentials` re-encrypts a new applicationKey (+ optional keyID) in one statement — all without touching the rest of the config
- `b2` — `Backend` interface (embeds `Uploader`), `S3Backend`, `B2Backend`, `FakeBackend`; `NewBackend` factory
- `backup` — folder walk + change detection + upload orchestration (depends on `b2.Uploader`; optional `b2.Deleter` via `WithDeleter` enables opt-in deletion propagation)
- `daemon` — background watcher (built 2026-06-16): recursive fsnotify watch + 5-min fallback scan + 1s/5s debounce; `start`/`stop` lifecycle (PID file `~/backup_repo/daemon.pid` = bare PID, readable by every version; on Linux and Windows also `daemon.start` = `<pid> <starttime>` (Linux: field 22 of `/proc/<pid>/stat`; Windows: `GetProcessTimes` creation time in 100 ns units), used only when its PID matches; Windows liveness also requires exit code `STILL_ACTIVE`; other OSes incl. macOS: PID only. Where a start time exists, liveness requires it to match and `stop` never signals a mismatched PID — ADR-023). Runs on **Linux + Windows** — OS-specific signal/stop/liveness logic lives in build-tagged `signals_unix.go` (`!windows`) / `signals_windows.go` (ADR-014), and process start time in `proc_linux.go` / `proc_windows.go` / `proc_other.go` (`!linux && !windows` → PID-only) (ADR-023/024); `stop` is graceful on both — Unix via SIGTERM, Windows via a named stop event (forceful `proc.Kill` fallback), see ADR-016. Depends on `store` + `b2.Uploader` via `backup.Service`.
- `cli` — subcommand handlers incl. `Ls`/`Get`/`Put`/`Rm`/`Find`/`Backend`/`Bucket`/`Buckets`/`SetAppKey` + `Start`/`Stop`/`Serve` (io injected for testability). `Init`/`Bucket` take a `BucketLister` (wired to `b2.FindBucket` in main.go; `TestBucketIDLookupWiring` guards that), `Buckets` takes two (`b2.ListBuckets`, `b2.FindBucket`), and `Status` takes the state dir + an optional `ServiceProbe` (main wires `service.Manager.State` on Linux, 2 s timeout) so the shared `resolveBucketID` name→ID lookup is stubbable without network. See ADR-017.
- `service` — `bb service install|uninstall|status` (Linux, built 2026-11-15): renders + manages the systemd **user** unit `~/.config/systemd/user/backuprepo.service` (`$XDG_CONFIG_HOME/systemd/user/` when that is set to an absolute path) (`Type=exec`, `ExecStart="<abs bb>" start`, `Restart=on-failure`, `RestartSec=30`, start limit 5/600 s; install runs `reset-failed` + `restart`, then requires `is-active`) via an injectable `systemctl --user` runner. Install default folder: `~/Documents`. Generated units carry a `# Managed by` marker; hand-written units are only replaced with `--force`. Logs: `journalctl --user -u backuprepo`. `--linger` → `EnableLinger` (`loginctl enable-linger <uid>`, warning on failure); `State` feeds `bb status`. Tests skip the Linux-only fixtures off Linux. See ADR-020/025.
- `web` — localhost web UI (`bb serve`, port 9171): `html/template` page over stdlib `net/http`; lists watched folders' contents with backup state, Upload + Close buttons, 🗑️ delete (local + remote). `127.0.0.1`-only, no auth, Host-header guard + `Origin`/`Referer` CSRF check on POSTs; browsing/deletion confined to watched folders. OS owner via build-tagged `owner_{unix,windows}.go`. See ADR-015.
- `version` — odometer version parse/bump, used only by `cmd/bump` (the binary embeds `VERSION` directly)
- `cmd/bump` — `make bump*` tool that rewrites `VERSION`
- root `main.go` — dispatch (incl. `start`/`stop`/`serve`/`bucket`/`buckets`/`appkey`) + per-command FlagSet + effective-backend factory (`buildBackend`) + `b2ConfigFromStore` helper. `run` validates arguments, flags and backend names (phase 1) before `config.Load`/`store.Open` (phase 2), so command-line errors create no state; `parseFlags` treats everything after `--` as positional (ADR-025).

### Reference docs

- Core design spec: `docs/superpowers/specs/2026-06-06-backuprepo-core-design.md`
- Core implementation plan: `docs/superpowers/plans/2026-06-06-backuprepo-core.md`
- Dual-backend design spec: `docs/superpowers/specs/2026-06-06-backuprepo-backends-design.md`
- Dual-backend implementation plan: `docs/superpowers/plans/2026-06-06-backuprepo-backends.md`
- User guide: `README.md`
