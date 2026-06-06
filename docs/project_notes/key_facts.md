# Key Facts

Non-sensitive project configuration and constants for backuprepo. **Never store secrets here** (no keyID/applicationKey, no master key). Real credentials live encrypted in `~/backup_repo/backup.db`; the master key lives in `~/backup_repo/key`.

### Repository

- Module path: `backuprepo`
- Remote: `github.com:PT-Scandium/backuprepo.git` (default branch: `master`)
- Language/toolchain: **Go 1.25+** (see `go.mod`), no CGO
- License: MIT (PT-Scandium)

### Build

- Build command: `go build -ldflags="-s -w" -o backuprepo .`
- Stripped binary size: ~14 MB (see decisions.md ADR-007)
- Test suite: `go test ./...` (6 internal packages have tests)

### Local state layout (`~/backup_repo/`)

- `backup.db` — SQLite (`modernc.org/sqlite`); credential fields AES-256-GCM encrypted; tables: `config` (single row), `folders`, `files`
- `key` — 32-byte random master key, mode `0600`, created on first run

### Constants

- Multipart upload threshold (S3 backend): **100 MB** (≤100 MB → single PutObject via transfer manager; >100 MB → S3 multipart)
- B2 native small-file limit (`b2SmallFileLimit`): **100 MB** (≤100 MB → single `b2_upload_file`; >100 MB → B2 large-file multipart via `b2_start_large_file` / `b2_upload_part` / `b2_finish_large_file`)
- Web UI port (planned, not built): **9171**
- Fallback full-scan interval (planned daemon): **5 minutes**

### Storage backends

- **`s3`** (default) — S3-compatible Backblaze B2 endpoint via `aws-sdk-go-v2`; addresses bucket by **name**.
- **`b2`** — Native Backblaze B2 **v2 API** (`/b2api/v2/...`) over stdlib `net/http`; addresses bucket by **ID** for list/upload, by **name** for download.
- Stored in `backend TEXT` column of `config` table; `NULL`/empty defaults to `"s3"`.
- Switch with `backuprepo backend [s3|b2]`; override per-command with `--backend s3|b2`.

### Backblaze B2 configuration

Collected by `backuprepo init`, stored in `backup.db`:
- keyID (access key ID) — *secret-adjacent; stored encrypted, not here*
- applicationKey — *secret; stored encrypted, not here*
- Bucket **name** — used by S3 API and B2 native download (URL path `/file/<bucket-name>/<key>`)
- Bucket **ID** — used by B2 native API for list and upload (`b2_list_file_names`, `b2_get_upload_url`); optional for S3-only users
- S3 endpoint URL, e.g. `https://s3.us-west-004.backblazeb2.com`
- Region = the middle hostname segment, e.g. `us-west-004`

### Manual bucket commands

Available once configured; all accept `--backend s3|b2`:
- `ls [path] [-r]` — list objects; non-recursive groups folders as prefixes
- `get <remote> [local] [-r]` — download object(s); `-r` downloads all under a prefix
- `put <local> [remote] [-r]` — upload file(s); `-r` uploads a directory tree
- `rm <path> [-r] [-f|-y]` — delete object(s); confirms unless `-f`/`-y`; `-r` deletes prefix
- `find <query> [prefix]` — case-insensitive substring search of object keys
- `backend [s3|b2]` — show or set the stored backend

### Package map (`internal/`)

- `apperr` — typed sentinel errors (imported by all packages)
- `crypto` — AES-256-GCM `Seal`/`Open`
- `config` — `~/backup_repo` paths + master-key file
- `store` — SQLite persistence (encrypted creds, folders, files, backend mode)
- `b2` — `Backend` interface (embeds `Uploader`), `S3Backend`, `B2Backend`, `FakeBackend`; `NewBackend` factory
- `backup` — folder walk + change detection + upload orchestration (depends only on `b2.Uploader`)
- `cli` — subcommand handlers incl. `Ls`/`Get`/`Put`/`Rm`/`Find`/`Backend` (io injected for testability)
- root `main.go` — dispatch + per-command FlagSet + effective-backend factory

### Reference docs

- Core design spec: `docs/superpowers/specs/2026-06-06-backuprepo-core-design.md`
- Core implementation plan: `docs/superpowers/plans/2026-06-06-backuprepo-core.md`
- Dual-backend design spec: `docs/superpowers/specs/2026-06-06-backuprepo-backends-design.md`
- Dual-backend implementation plan: `docs/superpowers/plans/2026-06-06-backuprepo-backends.md`
- User guide: `README.md`
