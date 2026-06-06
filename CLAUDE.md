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
backuprepo                     # First run: interactive setup (credentials, server, folder)
                               # Subsequent runs: start daemon/watcher in foreground
backuprepo init                # Re-run first-time setup (reconfigure credentials/server/folder)
backuprepo watch /path/to/dir  # Add a folder to watch list
backuprepo unwatch /path/to/dir # Remove a folder from watch list
backuprepo list                # List all watched folders and their status
backuprepo status              # Show daemon status, last scan time, pending uploads
backuprepo upload              # Force-upload all changed files now (no-op if nothing changed)
backuprepo serve               # Start web UI on port 9171 (without starting daemon)
backuprepo start               # Start daemon + web UI together
backuprepo stop                # Stop the running daemon gracefully
backuprepo config              # Show current configuration (server URL, watched folders)
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

This repository is in early development. Licensed under MIT (PT-Scandium).

## Build/Test/Lint Commands

- Build: `go build -ldflags="-s -w" -o backuprepo .` (Go 1.25+, no CGO)
- Test: `go test ./...`
- Vet/format: `go vet ./...` and `gofmt -l .`

## Implementation status (read before relying on the spec above)

The spec above is the original aspirational design. The **core CLI is implemented and merged** (`internal/{apperr,crypto,config,store,b2,backup,cli}` + `main.go`; subcommands `init`/`watch`/`unwatch`/`list`/`status`/`upload`/`config`). The **daemon and web UI (port 9171) are NOT yet built.**

A **native Backblaze B2 backend** (`B2Backend`, B2 v2 API over stdlib `net/http`) now exists alongside the S3-compatible backend (`S3Backend`), behind a unified `b2.Backend` interface. Six **manual bucket commands** are implemented: `ls`, `get`, `put`, `rm`, `find`, and `backend`. Switch backends persistently with `backuprepo backend [s3|b2]` or per-command with `--backend s3|b2`. The stored backend defaults to `s3`.

Several invariants above were deliberately changed in implementation — see `docs/project_notes/decisions.md` for the authoritative record. Notably: DB uses **pure-Go SQLite + AES-256-GCM field encryption, NOT SQLCipher** (ADR-001); uploads are **per-file, not tar+gzip** (ADR-003); `init` collects **both bucket name and bucket ID** (bucket name for S3 + B2 download, bucket ID for B2 list/upload — ADR-010 supersedes ADR-004); typed errors live in `internal/apperr`, not a root `errors.go` (ADR-005).

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
