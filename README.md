# backuprepo

A cross-platform CLI that watches user-specified folders and uploads changed files to a Backblaze B2 bucket via the S3-compatible API. Credentials are stored encrypted in a local SQLite database. Distributed as a single static binary.

> **Status:** Core CLI is complete. Background daemon, web UI, and `serve`/`start`/`stop` commands are planned follow-ups — see the roadmap section below.

---

## Build

Requires Go 1.25+. No CGO — uses pure-Go SQLite (`modernc.org/sqlite`).

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

## Usage walkthrough

This section walks through a complete first-time setup and a typical backup session. Replace the example paths and credentials with your own.

### 0. Get your Backblaze B2 credentials

In the [Backblaze B2 console](https://secure.backblaze.com/b2_buckets.htm):

1. **Create a bucket** (or pick an existing one). Note its **name** — e.g. `my-backups`. (You do **not** use the numeric bucket ID; see the section further down.)
2. Go to **Application Keys** → **Add a New Application Key**. Give it read/write access to your bucket. Backblaze shows you a **keyID** and an **applicationKey** — copy both now, because the applicationKey is shown only once.
3. On the **Buckets** page, find your bucket's **Endpoint**, e.g. `s3.us-west-004.backblazeb2.com`. Your **region** is the middle segment of that hostname — `us-west-004`.

You now have the five values `init` will ask for: keyID, applicationKey, bucket name, S3 endpoint URL, and region.

### 1. First-time setup — `init`

`init` prompts for each value and saves them (credentials encrypted) to `~/backup_repo/backup.db`. It can also add your first folder at the end.

```text
$ ./backuprepo init
Backblaze keyID (access key ID): 0001abcdef0123456789
Backblaze applicationKey (secret): K001-XXXXXXXXXXXXXXXXXXXXXXXXXXX
Bucket name: my-backups
S3 endpoint URL (e.g. https://s3.us-west-004.backblazeb2.com): https://s3.us-west-004.backblazeb2.com
S3 region (e.g. us-west-004): us-west-004
Configuration saved.
Folder to watch (blank to skip): /home/me/Documents
Watching /home/me/Documents
```

The applicationKey is the only value you must keep secret. To reconfigure later, run `init` again — it overwrites the saved configuration.

### 2. Choose what to back up — `watch` / `unwatch`

A "watched" folder is backed up recursively (all files under it, at any depth). Add or remove folders at any time:

```text
$ ./backuprepo watch /home/me/Pictures
Watching /home/me/Pictures

$ ./backuprepo unwatch /home/me/Documents
Stopped watching /home/me/Documents
```

`watch` requires the directory to already exist; it errors otherwise. The path you pass is the path that's stored, so prefer absolute paths.

### 3. Check what will happen — `status` and `list`

`status` is a quick summary (and the default when you run `backuprepo` with no arguments):

```text
$ ./backuprepo status
Status: configured
Watched folders: 1
Pending uploads: 12
```

`list` shows every watched folder plus each tracked file and when it was last backed up (`never` until its first successful upload):

```text
$ ./backuprepo list
Watched folders:
  /home/me/Pictures

Tracked files:
PATH                          SIZE     LAST BACKUP
/home/me/Pictures/cat.jpg     248913   2026-06-06T19:55:01+07:00
/home/me/Pictures/dog.png     512004   never
```

### 4. Back up — `upload`

`upload` scans every watched folder and uploads only the files that changed since their last backup. It prints a summary:

```text
$ ./backuprepo upload
Uploaded: 12, Skipped: 0, Failed: 0
```

Run it again and unchanged files are skipped — nothing is re-sent:

```text
$ ./backuprepo upload
Uploaded: 0, Skipped: 12, Failed: 0
```

If individual files fail (e.g. a permission error or a network blip), `upload` reports them in `Failed`, keeps going with the rest, and exits with a non-zero status so scripts can detect the problem.

> **Tip:** because `upload` is a safe no-op when nothing changed, you can run it from `cron` or a systemd timer to get periodic backups until the background daemon (see Roadmap) is implemented. For example, every 15 minutes:
>
> ```cron
> */15 * * * * /path/to/backuprepo upload >> ~/backup_repo/upload.log 2>&1
> ```

### 5. Inspect configuration — `config`

`config` prints the current settings. The applicationKey is masked (only its last 4 characters are shown); the keyID is an access-key identifier and is shown in full.

```text
$ ./backuprepo config
Endpoint:    https://s3.us-west-004.backblazeb2.com
Region:      us-west-004
Bucket:      my-backups
Key ID:      0001abcdef0123456789
App Key:     ****XXXX
Watched folders: 1
  /home/me/Pictures
```

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
