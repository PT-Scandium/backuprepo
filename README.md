# backuprepo

A cross-platform CLI that watches user-specified folders and uploads changed files to a Backblaze B2 bucket via the S3-compatible API. Credentials are stored encrypted in a local SQLite database. Distributed as a single static binary.

> **Status:** Core CLI is complete. Background daemon, web UI, and `serve`/`start`/`stop` commands are planned follow-ups — see the roadmap section below.

---

## Build

Requires Go 1.21+. No CGO — uses pure-Go SQLite (`modernc.org/sqlite`).

```sh
go build -ldflags="-s -w" -o backuprepo .
```

The stripped binary is approximately **14 MB** (aws-sdk-go-v2 + modernc.org/sqlite account for most of the size; below the <10 MB original target but noted for future trimming).

---

## Quick start

```sh
# 1. Configure credentials and bucket
./backuprepo init

# 2. Add a folder to back up
./backuprepo watch /path/to/folder

# 3. Upload all changed files now
./backuprepo upload
```

After `upload`, run it again: files that have not changed are skipped automatically (`Uploaded: 0, Skipped: N`).

---

## Subcommands

| Command | Description |
|---------|-------------|
| `backuprepo` (no args) | Alias for `status` |
| `backuprepo init` | Interactive setup — prompts for B2 credentials, bucket name, S3 endpoint, region, and an optional first folder to watch |
| `backuprepo watch <dir>` | Add an existing directory to the watch list |
| `backuprepo unwatch <dir>` | Remove a directory from the watch list |
| `backuprepo list` | List watched folders and all tracked files with last-backup timestamps |
| `backuprepo status` | Show whether configured, how many folders are watched, and how many files are pending upload |
| `backuprepo upload` | Scan watched folders and upload every file that has changed since the last backup; no-op if nothing has changed |
| `backuprepo config` | Show current configuration (endpoint, region, bucket, key ID, and masked app key) |
| `backuprepo help` | Print usage |

Exit codes: `0` on success, `1` on error (message written to stderr).

---

## B2 configuration — bucket name, not bucket ID

Backblaze B2 has two identifiers for every bucket: a human-readable **name** (e.g. `my-backups`) and a numeric **ID** (e.g. `abc123456789`). The S3-compatible API addresses buckets by **name**. `backuprepo init` asks for the name.

`init` prompts for:

| Prompt | Example value |
|--------|---------------|
| Backblaze keyID (access key ID) | `0001abcdef0123456789` |
| Backblaze applicationKey (secret) | `K001-...` |
| Bucket name | `my-backups` |
| S3 endpoint URL | `https://s3.us-west-004.backblazeb2.com` |
| S3 region | `us-west-004` |

Find your endpoint and region in the Backblaze B2 console under Buckets → Endpoint. The region is the subdomain segment between `s3.` and `.backblazeb2.com`.

---

## Local state layout

All state lives under `~/backup_repo/`:

```
~/backup_repo/
  backup.db   SQLite database — credentials (AES-256-GCM encrypted at rest),
              watched folders, and per-file backup metadata
  key         32-byte random master key (mode 0600); created on first run
              and reused on every subsequent launch
```

The `key` file is the only secret that lives in plaintext on disk; protect it like a password. Credential fields inside `backup.db` are encrypted with this key before being written, so the raw database file does not contain usable credentials.

---

## Change detection

Before uploading, `backuprepo upload` checks each file against its stored record:

1. If size **and** mtime are unchanged since the last backup — skip.
2. If either differs — compute a SHA-256 content hash.
3. If the hash matches the stored hash — update metadata, skip the upload.
4. Otherwise — upload the file and record the new hash, size, mtime, and backup timestamp.

Files larger than 100 MB are sent via S3 multipart upload automatically; files up to 100 MB use a single `PutObject` call.

---

## Roadmap (not yet implemented)

The following features are designed and planned but not yet built:

- **Background daemon** — filesystem event watcher (`fsnotify` on Linux/macOS, `ReadDirectoryChangesW` on Windows) with a 5-minute full-scan fallback. Will run uploads silently in the background.
- **`backuprepo start`** — start daemon + web UI together.
- **`backuprepo stop`** — gracefully stop the running daemon.
- **`backuprepo serve`** — start only the web UI (port 9171) without the daemon.
- **Web UI (port 9171)** — localhost-bound interface showing folder contents with file metadata, last-backup timestamps, delete actions, and a force-upload button.

Design details for all of the above are in `docs/superpowers/`.

---

## License

MIT — see [LICENSE](LICENSE).
