# backuprepo (`bb`)

A cross-platform CLI that backs up your files to a Backblaze B2 bucket — either by **watching local folders** and uploading changed files, or by **manually pushing/pulling** individual files and folders. Credentials are stored encrypted in a local SQLite database, and it ships as a single static binary named **`bb`**.

> **Status:** feature-complete against the original spec — core CLI, dual backend, the background daemon (`bb start`/`stop`, Linux + Windows), and the localhost web UI (`bb serve`, port 9171) are all built.

---

## Install

Requires **Go 1.25+**. No CGO (pure-Go SQLite), so the build is a single **statically linked** binary that runs on any Linux host with no shared-library dependencies.

```sh
make install        # build and copy `bb` to ~/.local/bin
```

Make sure `~/.local/bin` is on your `PATH`. Other options:

```sh
make                # just build ./bb in the repo (then run it as ./bb)
sudo make install PREFIX=/usr/local/bin    # install system-wide
make clean | uninstall | test | help       # housekeeping targets
```

No `make`? `go build -ldflags="-s -w" -o bb .` produces the same binary. The stripped binary is ~14 MB.

> This guide uses the command **`bb`** (the installed name). If you only ran `make`, call it as `./bb` from the repo directory.

---

## User guide

There are two ways to use `bb`, sharing one setup:

- **Folder backup** *(mode 1)* — watch folders; `upload` sends whatever changed.
- **Manual file operations** *(mode 2)* — `put`/`get`/`ls`/`rm`/`find` act directly on the bucket.

Follow steps 1–3 once, then use mode 1, mode 2, or both.

### 1. Get your Backblaze B2 credentials

In the [Backblaze B2 console](https://secure.backblaze.com/b2_buckets.htm):

1. **Pick or create a bucket.** Note its **name** (e.g. `my-backups`) and its **bucket ID** (alphanumeric, shown in the bucket list).
2. **Application Keys → Add a New Application Key**, with read/write on that bucket. Backblaze shows a **keyID** and an **applicationKey** — copy both now; the applicationKey is shown only once. *(A bucket-scoped key is safer than your account master key.)*
3. *(S3 backend only)* On the **Buckets** page note the **Endpoint** (e.g. `s3.us-west-004.backblazeb2.com`); the **region** is its middle segment (`us-west-004`).

### 2. Configure — `bb init`

`init` prompts for each value and saves them (credentials encrypted) to `~/backup_repo/backup.db`:

```text
$ bb init
Backblaze keyID (access key ID): 0001abcdef0123456789
Backblaze applicationKey (secret): K001-XXXXXXXXXXXXXXXXXXXXXXXXXXX
Bucket name: my-backups
Bucket ID (for native B2 API): e73ede9969c64827
S3 endpoint URL (e.g. https://s3.us-west-004.backblazeb2.com): https://s3.us-west-004.backblazeb2.com
S3 region (e.g. us-west-004): us-west-004
Configuration saved.
Folder to watch (blank to skip): /home/me/Documents
Watching /home/me/Documents
```

- Only the **applicationKey** is secret. Re-run `init` anytime to reconfigure — it overwrites the saved config.
- **Using only the native B2 backend?** Leave **endpoint** and **region** blank; they're used by the S3 backend only. (Required: keyID, applicationKey, bucket name. The bucket ID is required for the `b2` backend.)
- **Stay configured:** credentials persist encrypted on disk, so you never log in interactively again — each command silently re-authorizes with Backblaze using the saved key. You only re-run `init` if the key changes or is revoked.
- **Switch buckets later:** to point `bb` at a *different* bucket without re-entering credentials, use `bb bucket <name> <id>` (or `bb bucket <name>` for an S3-only bucket). It changes only the bucket name + ID. Note: the stored key must have access to the new bucket — a bucket-scoped key won't, so for a bucket under a different key, re-run `init` instead.
- **Rotate the application key:** `bb appkey` reads a new applicationKey from **stdin** (so the secret stays out of `argv` and shell history) and replaces the stored one. When you run it interactively the input is **not echoed** to the screen; you can also pipe it from a secret store, and pass the new keyID to rotate the whole pair:
  ```sh
  pass show backblaze/b2-key | bb appkey 0005newkeyid   # rotate keyID + secret
  bb appkey                                             # change only the secret (prompts on stdin)
  ```
  Don't pass the secret as a command-line argument (it would land in your shell history and process list). Verify afterwards with `bb ls`.

### 3. Pick a backend (optional)

`bb` can reach B2 two ways; the default is `s3`:

| Backend | `id` | Protocol |
|---------|------|----------|
| **S3-compatible** (default) | `s3` | aws-sdk-go-v2 against the B2 S3 endpoint |
| **Native B2** | `b2` | Backblaze B2 **v3** API over stdlib `net/http` |

```sh
bb backend b2        # set the stored backend (persists)
bb backend           # show the current backend
bb ls --backend s3   # override for one command only
```

### 4a. Folder backup (mode 1)

Watch folders, then `upload` whatever changed. A watched folder is backed up recursively (all files beneath it).

```sh
bb watch /home/me/Pictures      # add a folder (must already exist)
bb unwatch /home/me/Documents   # stop watching
bb list                         # watched folders + tracked files & last-backup times
bb status                       # quick summary (also what `bb` with no command prints)
bb upload                       # upload changed files; no-op if nothing changed
```

`upload` uploads only what changed and prints a summary:

```text
$ bb upload
Uploaded: 12, Skipped: 0, Deleted: 0, Failed: 0
$ bb upload            # second run, nothing changed
Uploaded: 0, Skipped: 12, Deleted: 0, Failed: 0
```

Failed files are counted, don't abort the run, and make `bb` exit non-zero so scripts can detect a problem.

**Run it continuously — the daemon:**

```sh
bb start            # watch folders and back up changes in real time (foreground; Ctrl-C to stop)
bb stop             # signal a running daemon to shut down gracefully
```

`start` uses filesystem events (`fsnotify`) for near-real-time backups, with a full scan every 5 minutes as a safety net for anything the event stream misses. It runs in the foreground — background it with a systemd user unit or `nohup bb start &`. Only one daemon runs at a time (tracked via `~/backup_repo/daemon.pid`).

Runs on **Linux and Windows**, and `bb stop` is graceful on both (the daemon removes its PID file and closes cleanly): Linux via `SIGTERM`, Windows via a named stop event (falling back to a forceful terminate only if that event is unavailable). Foreground **Ctrl-C** also stops it cleanly on either OS.

**Propagating deletions (opt-in):** by default a deleted local file keeps its remote backup. Pass `--delete` to also remove remote objects whose local files were deleted:

```sh
bb upload --delete    # one-shot: also remove remotes for locally-deleted files
bb start --delete     # continuously
```

This is destructive — on versioned buckets all versions are purged. As a safeguard, deletions are skipped for any watched folder that is currently **missing** (e.g. an unmounted drive), so a disconnected disk never wipes your backup.

> **Or schedule it:** because `upload` is a safe no-op when nothing changed, you can run it from cron instead of the daemon:
>
> ```cron
> */15 * * * * /home/me/.local/bin/bb upload >> ~/backup_repo/upload.log 2>&1
> ```

### 4b. Manual file operations (mode 2)

Act directly on the bucket — no watching required.

```sh
bb ls                                    # list bucket root (folders shown as name/)
bb ls photos/ -r                         # list everything under a prefix, recursively
bb put ./report.pdf reports/report.pdf   # upload a file to a specific key
bb put ./photos/ photos/ -r              # upload a whole directory
bb get reports/report.pdf ./out.pdf      # download an object
bb get photos/ ./local-photos/ -r        # download a whole prefix
bb find report                           # case-insensitive search of object names
bb rm reports/report.pdf                 # delete one object (asks to confirm)
bb rm photos/ -r -f                      # delete a whole prefix, skip confirmation
```

- For `get`/`put`, the second path defaults to the basename if you omit it.
- `rm` deletes **all versions** on the `b2` backend and **confirms** unless you pass `-f` or `-y`.
- **Flags go anywhere:** `-r`, `-f`, `-y`, and `--backend` may appear before, after, or between the path arguments — `bb rm photos/ -r -f` and `bb rm -r -f photos/` are equivalent.

### 5. Check your configuration — `bb config`

```text
$ bb config
Endpoint:    https://s3.us-west-004.backblazeb2.com
Region:      us-west-004
Bucket:      my-backups
Bucket ID:   e73ede9969c64827
Backend:     s3
Key ID:      0001abcdef0123456789
App Key:     ****XXXX
Watched folders: 1
  /home/me/Pictures
```

The applicationKey is masked (last 4 characters); the keyID is just an identifier and is shown in full.

---

## Command reference

| Command | Description |
|---------|-------------|
| `bb` (no args) | Same as `status` |
| `bb init` | Interactive setup (credentials, bucket name + ID, endpoint, region, optional first folder) |
| `bb config` | Show current configuration (app key masked) |
| `bb bucket [<name> [<id>]]` | Show, or switch to another bucket — changes only the bucket name + ID (keeps credentials/endpoint/region) |
| `bb appkey [<new-keyID>]` | Replace the stored application key — reads the secret from **stdin** (kept out of shell history); pass a new keyID to rotate the whole pair |
| `bb watch <dir>` | Add an existing directory to the watch list |
| `bb unwatch <dir>` | Remove a directory from the watch list |
| `bb list` | List watched folders and tracked files with last-backup times |
| `bb status` | Configured? backend, watched-folder count, pending uploads |
| `bb upload [--delete]` | Upload every changed file; no-op if nothing changed. `--delete` also removes remotes for locally-deleted files |
| `bb start [--delete]` | Run the watcher daemon (fsnotify + 5-min fallback scan) in the foreground until stopped |
| `bb stop` | Signal a running daemon to shut down gracefully |
| `bb serve` | Start the localhost web UI on `http://127.0.0.1:9171` (foreground) |
| `bb backend [s3\|b2]` | Show or set the stored backend |
| `bb ls [path] [-r]` | List bucket contents (folders shown with trailing `/`) |
| `bb get <remote> [local] [-r]` | Download an object, or a prefix with `-r` |
| `bb put <local> [remote] [-r]` | Upload a file, or a directory with `-r` |
| `bb rm <path> [-r] [-f]` | Delete an object/prefix; confirms unless `-f`/`-y` |
| `bb find <query> [prefix]` | Case-insensitive substring search of object names |
| `bb help` | Print usage |

Every file command also accepts `--backend s3|b2` to override the stored backend for that one invocation. Exit codes: `0` success, `1` error (message on stderr).

---

## How it works

**Local state** lives under `~/backup_repo/`:

```
~/backup_repo/
  backup.db   SQLite — credentials (AES-256-GCM encrypted at rest), watched
              folders, and per-file backup metadata
  key         32-byte random master key (mode 0600), created on first run
```

The `key` file is the only plaintext secret on disk — protect it like a password. Credential fields inside `backup.db` are encrypted with it, so the raw database file contains no usable credentials.

**Change detection** (during `upload`): compare size + mtime first; if either differs, compute a SHA-256 hash; upload only when the hash differs from the last backup — otherwise just refresh metadata. Files over **100 MB** upload via multipart automatically; smaller files go in a single request.

**Bucket name vs. ID:** B2 identifies a bucket two ways, and `bb` stores both — the **name** is used by the S3 API and by B2 downloads (`/file/<name>/<key>`); the **ID** is used by the native B2 API for listing and uploads. S3-only users can leave the ID blank.

---

## Web UI — `bb serve`

```sh
bb serve     # serves http://127.0.0.1:9171 in the foreground (Ctrl-C or the Close button to stop)
```

A warm-themed localhost page for browsing your watched folders and their backup state. It shows your username and the server location at the top, then a table of the current folder's contents:

| Column | Meaning |
|--------|---------|
| Filename | File or folder (click a folder to drill in; breadcrumb to go back) |
| File Type | Extension, or `folder` |
| File Size | Human-readable |
| Last Modified | Local modification time |
| Modified By | OS file owner (Unix; `—` on Windows) |
| Last Backup | Time of last successful upload, or `never` |
| Actions | 🗑️ delete |

Two buttons at the bottom: **Upload changed files** (runs a backup now) and **Close** (stops the server).

**Security & scope:**
- Binds to **`127.0.0.1` only**, with **no authentication** (you're already authenticated by the stored credentials). A `Host`-header check rejects non-localhost requests (DNS-rebinding guard), and POST actions require a same-origin `Origin`/`Referer` (CSRF guard) so a malicious web page can't drive the UI.
- Browsing and deletion are **confined to your watched folders** — the UI cannot reach arbitrary paths.
- ⚠️ **The 🗑️ action is destructive and unrecoverable: it deletes the file (or folder) from *both* this computer and the backup bucket.** The page asks you to confirm first.

---

## Roadmap

Feature-complete against the original spec. Design notes live in `docs/superpowers/`. The remaining possible enhancement (not blocking) is a leaner binary; graceful Windows `bb stop` and no-echo `bb appkey` entry are now implemented.

---

## License

MIT — see [LICENSE](LICENSE).
