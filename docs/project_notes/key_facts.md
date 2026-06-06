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

- Multipart upload threshold: **100 MB** (≤100 MB → single PutObject; >100 MB → S3 multipart via transfer manager)
- Web UI port (planned, not built): **9171**
- Fallback full-scan interval (planned daemon): **5 minutes**

### Backblaze B2 (S3-compatible) configuration

Collected by `backuprepo init`, stored in `backup.db`:
- keyID (access key ID) — *secret-adjacent; stored encrypted, not here*
- applicationKey — *secret; stored encrypted, not here*
- Bucket **name** (not bucket ID — see ADR-004)
- S3 endpoint URL, e.g. `https://s3.us-west-004.backblazeb2.com`
- Region = the middle hostname segment, e.g. `us-west-004`

### Package map (`internal/`)

- `apperr` — typed sentinel errors (imported by all packages)
- `crypto` — AES-256-GCM `Seal`/`Open`
- `config` — `~/backup_repo` paths + master-key file
- `store` — SQLite persistence (encrypted creds, folders, files)
- `b2` — `Uploader` interface, `FakeUploader`, `S3Uploader`
- `backup` — folder walk + change detection + upload orchestration
- `cli` — subcommand handlers (io injected for testability)
- root `main.go` — dispatch + real-dependency wiring

### Reference docs

- Design spec: `docs/superpowers/specs/2026-06-06-backuprepo-core-design.md`
- Implementation plan: `docs/superpowers/plans/2026-06-06-backuprepo-core.md`
- User guide: `README.md`
