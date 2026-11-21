# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project: backuprepo

A cross-platform file backup tool that watches user-specified folders and silently uploads changed files to a Backblaze B2 server via S3-compatible API. Distributed as a single binary.

## Architecture

- Public API: backup.go, config.go, errors.go
- B2 client wraps aws-sdk-go-v2 with S3-compatible endpoint (Backblaze B2)
- Compression: stdlib archive/tar + compress/gzip
- Multipart threshold: 100 MB (all uploads use S3-compatible PutObject, including large files via multipart)

### Local Database

- Location: `~/backup_repo/` (login user's home directory)
- Format: SQLite3, encrypted with **SQLCipher** (via `go-sqlcipher`) to prevent tampering
- The DB file is created automatically on first execution of the backuprepo binary
- Stores:
  - User credentials and authentication token (collected on first run)
  - Destination server URL (Backblaze B2, S3-compatible)
  - List of watched folders
  - File metadata and backup state for all watched files

### First-Run Flow

1. Binary is launched (double-click on Windows, CLI on Linux/GNOME/KDE)
2. Prompt user for credentials (username/password or API key) — saved encrypted in SQLite DB
3. Ask user for the **destination server URL** (Backblaze B2 endpoint)
4. Ask user which **folder to watch** — all children under that folder are automatically included
5. Start the daemon/watcher process

### Daemon / File Watcher

- **Primary detection:** Filesystem events via `fsnotify` (Linux/macOS) and `ReadDirectoryChangesW` (Windows) for real-time change detection — keeps incremental uploads as small as possible
- **Fallback:** Full directory scan every **5 minutes** as a safety net
- On detecting a changed file, immediately upload the file to the server via S3-compatible API
- Uploads happen silently in the background while the user works on files
- Goal: minimize upload size by catching changes early and uploading incrementally

### Upload Rules

- All uploads use **S3-compatible PutObject** (Backblaze B2 endpoint)
- Files ≤ 100 MB: single PutObject call
- Files > 100 MB: S3-compatible **multipart upload**
- If a file has not changed since last backup, skip it (no redundant uploads)

### Web Interface (port 9171)

- **No authentication** on the web UI — user is already authenticated at first binary launch (credentials stored in DB)
- Localhost-bound web server on port **9171**
- **Color scheme:** warm colors
- **Top of page:** display username and server location
- **Main content:** table listing folder contents with columns:
  | Column | Description |
  |--------|-------------|
  | Filename | Name of file or folder |
  | File Type | Extension or folder |
  | File Size | Human-readable size |
  | Last Modified | Timestamp of last local modification |
  | Modified By User | OS-level file owner |
  | Last Backup | Timestamp of last successful upload to server |
  | Actions | Trash can icon — delete the file/folder |
- **Navigation:** clicking a folder name refreshes the page and drills into that folder (breadcrumb-style, level by level)
- **Bottom of page — two buttons:**
  - **Upload** — force-upload all changed files now; if nothing has changed, do nothing
  - **Close** — close the web interface page

### CLI Interface

The primary user interface. All interaction via a single `backuprepo` binary with subcommands. No external dependencies or frameworks — use Go's `flag` package or minimal argument parsing to keep the binary small (**target < 10 MB**).

```
bb                       # First run: interactive setup (credentials, server, folder)
                         # Subsequent runs: start daemon/watcher in foreground
bb init                  # Re-run first-time setup (reconfigure credentials/server/folder)
bb watch /path/to/dir    # Add a folder to watch list
bb unwatch /path/to/dir  # Remove a folder from watch list
bb list                  # List all watched folders and their status
bb status                # Show daemon status, last scan time, pending uploads
bb upload                # Force-upload all changed files now (no-op if nothing changed)
bb serve                 # Start web UI on port 9171 (without starting daemon)
bb start                 # Start daemon + web UI together
bb stop                  # Stop the running daemon gracefully
bb config                # Show current configuration (server URL, watched folders)
```

- Output is plain text to console — no colors, no spinners, no TUI libraries
- Each command completes and exits (except `start`/`serve` which run until stopped)
- Exit codes: 0 = success, 1 = error (with message to stderr)
- Keep binary small: avoid heavy dependencies, prefer stdlib, use `go build -ldflags="-s -w"` to strip debug info

## Key Invariants

- All public funcs accept `context.Context` as first arg
- Errors are typed (see errors.go), never raw strings
- No global state; always pass Config → Client
- SQLite DB must always be opened with SQLCipher encryption key
- Never store credentials in plaintext outside the encrypted DB

## Platform Support

- **Windows:** double-click executable, filesystem events via OS API
- **Linux / GNOME / KDE:** command-line execution, filesystem events via `fsnotify`
- Single binary distribution for each platform

---

This repository is feature-complete against the spec above (see Implementation status). Licensed under MIT (PT-Scandium).

## Build/Test/Lint Commands

- Build: `make` → `./bb` (`make install` → `~/.local/bin/bb`), or `CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bb .` (Go 1.25.10+ per `go.mod`; `CGO_ENABLED=0` is required for the static binary). CI is **GoCD on andromeda** (`http://127.0.0.1:8153/go`, pipeline `backuprepo`, `.gocd/backuprepo.gocd.yaml`; setup `deploy/gocd/README.md`; ADR-026): every commit on `master` reaches it through the mirror hooks and runs `make lint`, `make coverage`, `make wintest` (Windows test binaries compiled, not run) and `make build`/`make build-windows`. No GitHub Actions. `make coverage` (coverage.out + total) and `make lint` (vet on linux/windows/darwin + gofmt check). `/verify` test lanes: `.gstack/pregate.json` + `scripts/pregate-go-test.sh`
- Test: `go test ./...`
- Vet/format: `go vet ./...` and `gofmt -l .`

## Project verification

- Unit tests: `make test`; coverage: `make coverage`; lint: `make lint`; mirror-hook tests: `bash deploy/gocd/mirror-hook_test.sh`
- CI: GoCD pipeline `backuprepo` on andromeda (see above); a change is CI-verified only by a passed run that built its exact commit (`modifications[0].revision`)
- Coverage policy: **no threshold is configured** (neither CI nor Makefile gates it) — report the measured total and the gaps
- Notes: `docs/project_notes/` (bugs, decisions, key_facts, issues) — this repo has no `project-notes.md`

## Implementation status (read before relying on the spec above)

The spec above is the original aspirational design. **The project has been feature-complete against this spec since 2026-06-16; current release: v1.0.12 (2026-11-21)** — everything below is implemented and merged to `master` (Go packages under `internal/{apperr,crypto,config,store,b2,backup,daemon,web,cli,service,version}` + `main.go`):

- **Core CLI** — `init`, `watch`, `unwatch`, `list`, `status`, `upload`, `config`, `version`. Watched folders are stored as absolute paths (`~` expanded; case-insensitive matching on Windows); unknown commands, `version` and `help` never create `~/backup_repo` (ADR-021/023/024), nor does `bb <command> -h` (bugs.md 2026-11-19).
- **Dual backend** — `S3Backend` (aws-sdk) and a **native `B2Backend`** (B2 **v3** API over stdlib `net/http`) behind a unified `b2.Backend` interface. Switch with `bb backend [s3|b2]` (default `s3`) or, for one `ls`/`get`/`put`/`rm`/`find`, `--backend s3|b2`.
- **Manual bucket commands** — `ls`, `get`, `put`, `rm`, `find`, `backend`.
- **Setup** — `init` reads the applicationKey without echo, picks the `b2` backend when no S3 endpoint/region is given, and fails clearly without an interactive terminal (ADR-021/022). `buckets` lists buckets; the bucket ID is auto-resolved from the name (ADR-017).
- **Partial reconfig (no full `init`)** — `bucket [<name> [<id>]]` switches the destination bucket; `appkey [<new-keyID>]` rotates the application key (secret read from stdin, never argv; no echo).
- **Background daemon** — `start` / `stop`: real-time `fsnotify` watcher + 1s/5s debounce + 5-minute fallback scan, PID-file lifecycle (`daemon.pid` + `daemon.start` start-time identity on Linux/Windows, so `stop` never signals a reused PID — ADR-023/024). **Runs on Linux and Windows** (OS-specific logic in build-tagged `internal/daemon/{signals,proc}_*.go`; `stop` is graceful on both — Windows via a named event, ADR-016).
- **systemd user service (Linux)** — `service install [--folder DIR] [--delete] [--force] [--linger] | uninstall | status` runs the daemon as `~/.config/systemd/user/backuprepo.service` (default folder `~/Documents`; ADR-020).
- **Opt-in deletion propagation** — `upload --delete` / `start --delete` removes remote objects whose local files were deleted (off by default; unmounted-folder safety guard).
- **Web UI (port 9171)** — `serve`: localhost-only (`127.0.0.1`), no auth per spec but with a Host-header + `Origin`/`Referer` CSRF guard, browsing/deletion confined to watched folders; warm-themed folder table, Upload + Close buttons, destructive (local + remote) delete behind a confirm.

Several invariants above were deliberately changed in implementation — see `docs/project_notes/decisions.md` for the authoritative record (ADRs 001–026). Notably: DB uses **pure-Go SQLite + AES-256-GCM field encryption, NOT SQLCipher** (ADR-001); uploads are **per-file, not tar+gzip** (ADR-003); `init` collects **both bucket name and bucket ID** (bucket name for S3 + B2 download, bucket ID for B2 list/upload — ADR-010 supersedes ADR-004); typed errors live in `internal/apperr`, not a root `errors.go` (ADR-005); the daemon adds an `fsnotify` dependency (ADR-012); `bb start` runs the daemon only — the web UI is the separate `bb serve` (ADR-015). Also unlike the CLI sketch above (decided in ADR-025): bare `bb` (and a Windows double-click) prints `status` rather than running setup or the daemon — setup is always `bb init`; `bb status` reports configured/backend/folders/pending uploads plus daemon state and (Linux) service state, not a last-scan time. Accepted limitations (ADR-025): ~22 MB binary (over the <10 MB target) and no Windows per-directory case sensitivity. Open follow-ups live in `docs/project_notes/issues.md`.

**Releasing:** bump with `make bump` on `master`, wait for a **passed GoCD `backuprepo` run that built that exact commit** (its `material_revisions[0].modifications[0].revision` = `git rev-parse HEAD`; a commit merely listed in a folded run doesn't count; never "the latest run" — see `deploy/gocd/README.md` Checks); if the release touches `*_windows.go` or case-folding code, also run `go test ./...` on a Windows machine (GoCD doesn't run Windows tests, ADR-026); tag `vX.Y.Z` (annotated), publish a GitHub release with `bb-vX-linux-amd64`, `bb-vX-windows-amd64.exe` and `SHA256SUMS` (`CGO_ENABLED=0 -trimpath -ldflags="-s -w"`, built from the tag), then commit the README/Windows-guide download links (`docs: vX — README links + notes`).

## Project Memory System

This project maintains institutional knowledge in `docs/project_notes/` for consistency across sessions.

### Memory Files

- **bugs.md** — Bug log with dates, root causes, solutions, and prevention notes
- **decisions.md** — Architectural Decision Records (ADRs) with context and trade-offs
- **key_facts.md** — Project configuration, ports, paths, constants, package map
- **issues.md** — Work log of completed/pending work

### Memory-Aware Protocols

- **Before proposing architectural changes:** check `docs/project_notes/decisions.md` for existing decisions; if a change conflicts, acknowledge the prior ADR and explain why revisiting it is warranted.
- **When encountering errors/bugs:** search `docs/project_notes/bugs.md` for similar issues and apply known fixes; document new bugs and solutions when resolved.
- **When looking up configuration:** check `docs/project_notes/key_facts.md` (ports, paths, constants) before assuming.
- **When completing work:** log it in `docs/project_notes/issues.md`.
- **When the user requests a memory update:** update the appropriate file following its established format (bullet lists, dates, concise entries).

### Style Guidelines for Memory Files

- Prefer bullet lists over tables; keep entries concise (1–3 lines).
- Always include dates; include URLs for tickets/PRs/docs where available.
- Manual cleanup of old entries is expected (not automated).
