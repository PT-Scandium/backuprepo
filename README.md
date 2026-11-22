# backuprepo (`bb`)

A cross-platform CLI that backs up your files to a Backblaze B2 bucket — either by **watching local folders** and uploading changed files, or by **manually pushing/pulling** individual files and folders. Credentials are stored encrypted in a local SQLite database, and it ships as a single static binary named **`bb`**.

> **Status:** feature-complete against the original spec — core CLI, dual backend, the background daemon (`bb start`/`stop`, Linux + Windows), and the localhost web UI (`bb serve`, port 9171) are all built.

---

## Install

### Download a prebuilt binary (v1.0.12)

No Go toolchain needed — grab the static binary for your platform from the [**v1.0.12 release**](https://github.com/PT-Scandium/backuprepo/releases/tag/v1.0.12):

| Platform | Download |
|----------|----------|
| Linux (x86-64) | [`bb-v1.0.12-linux-amd64`](https://github.com/PT-Scandium/backuprepo/releases/download/v1.0.12/bb-v1.0.12-linux-amd64) |
| Windows (x86-64) | [`bb-v1.0.12-windows-amd64.exe`](https://github.com/PT-Scandium/backuprepo/releases/download/v1.0.12/bb-v1.0.12-windows-amd64.exe) |
| Checksums | [`SHA256SUMS`](https://github.com/PT-Scandium/backuprepo/releases/download/v1.0.12/SHA256SUMS) |

**Linux:**

```sh
curl -LO https://github.com/PT-Scandium/backuprepo/releases/download/v1.0.12/bb-v1.0.12-linux-amd64
curl -LO https://github.com/PT-Scandium/backuprepo/releases/download/v1.0.12/SHA256SUMS
sha256sum -c SHA256SUMS --ignore-missing     # verify the download
chmod +x bb-v1.0.12-linux-amd64
sudo mv bb-v1.0.12-linux-amd64 /usr/local/bin/bb
bb version                                   # -> bb 1.0.12
```

**Windows:** download `bb-v1.0.12-windows-amd64.exe` (optionally verify it against `SHA256SUMS`), rename it to `bb.exe`, and place it on your `PATH` — step by step in [**Windows quick start**](#windows-quick-start) below.

### Build from source

Requires **Go 1.25.10+** (per `go.mod`). No CGO (pure-Go SQLite), so the build is a single **statically linked** binary that runs on any Linux host with no shared-library dependencies.

```sh
make install        # build and copy `bb` to ~/.local/bin
```

Make sure `~/.local/bin` is on your `PATH`. Other options:

```sh
make                # just build ./bb in the repo (then run it as ./bb)
sudo make install PREFIX=/usr/local/bin    # install system-wide
make clean | uninstall | test | help       # housekeeping targets
make coverage | lint                       # coverage report; vet (linux/windows/darwin) + gofmt check
```

No `make`? `CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bb .` produces the same static binary (without `CGO_ENABLED=0` a host with cgo enabled builds a dynamically linked one). The stripped binary is ~22 MB.

> This guide uses the command **`bb`** (the installed name). If you only ran `make`, call it as `./bb` from the repo directory.

### Windows quick start

Install, log in, and upload — using the **native B2 backend** (no S3 endpoint or region needed). Also available as a printable page: [`docs/windows-setup.html`](docs/windows-setup.html).

| # | Step | Exactly what to do |
|---|------|--------------------|
| 1 | Open PowerShell | Press **Windows key** → type `powershell` → **Enter** |
| 2 | Create a folder for the tool | `mkdir $env:USERPROFILE\bb` |
| 3 | Move the download and rename it | `move $env:USERPROFILE\Downloads\bb-v1.0.12-windows-amd64.exe $env:USERPROFILE\bb\bb.exe` |
| 4 | Remove the "downloaded from internet" block | `Unblock-File $env:USERPROFILE\bb\bb.exe` |
| 5 | Add it to your `PATH` | `[Environment]::SetEnvironmentVariable("Path",[Environment]::GetEnvironmentVariable("Path","User")+";$env:USERPROFILE\bb","User")` |
| 6 | Reopen PowerShell | Close the window → **Windows key** → `powershell` → **Enter** (`PATH` only applies to new windows) |
| 7 | Check the install | `bb version` → prints `bb 1.0.12` |
| 8 | Start setup | `bb init` |
| 9 | Answer the credential prompts | Paste **keyID** → **Enter**, paste **applicationKey** → **Enter**, type **bucket name** → **Enter** |
| 10 | Bucket ID | Resolved automatically. If it still asks, paste the **Bucket ID** from the B2 console → **Enter** |
| 11 | Skip the two S3 prompts | Press **Enter** on *S3 endpoint URL* and again on *S3 region* — `init` then selects the B2 backend for you |
| 12 | Folder to watch | Press **Enter** to skip (add it later with `bb watch`) |
| 13 | Check the B2 backend | `bb backend` → `b2` (set automatically by step 11 since v1.0.5; on older versions run `bb backend b2`) |
| 14 | Verify the login works | `bb ls` — lists the bucket; credentials stay saved, so you never log in again |
| 15 | Upload one file | `bb put C:\Users\you\Documents\report.pdf reports/report.pdf` |
| 16 | Upload a whole folder | `bb put C:\Users\you\Documents\ documents/ -r` (resume an interrupted run with `--skip-existing`) |

Credentials are saved encrypted in `%USERPROFILE%\backup_repo\`. Confirm an upload arrived with `bb ls -r`.

**Automatic backup instead of manual `put`:** run `bb watch C:\Users\you\Documents` once, then `bb upload` whenever you want changed files sent — it is a safe no-op when nothing changed. To have it run continuously at logon:

```powershell
schtasks /create /tn "backuprepo" /sc onlogon /tr "$env:USERPROFILE\bb\bb.exe start" /f
```

Stop it with `bb stop`. Two Windows notes: the binary is **unsigned**, so SmartScreen shows *"Windows protected your PC"* on first run (**More info → Run anyway**); and `bb.exe` is a **console app** — double-clicking it just runs `bb status` and closes, so always call it from PowerShell.

---

## User guide

There are two ways to use `bb`, sharing one setup:

- **Folder backup** *(mode 1)* — watch folders; `upload` (or the **daemon**, see below) sends whatever changed.
- **Manual file operations** *(mode 2)* — `put`/`get`/`ls`/`rm`/`find` act directly on the bucket.

Follow steps 1–3 once, then use mode 1, mode 2, or both.

> **One bucket at a time.** `bb` is configured for a single destination bucket — every command (`upload`, the daemon, `ls`/`put`/`get`/`rm`/`find`) acts on that one bucket. There is no multi-bucket mode. To work with a different bucket, point `bb` at it with `bb bucket <name> <id>` (the stored key must have access to it; if it needs different credentials, re-run `bb init`) — see the **Switch buckets later** note in step 2.

### 1. Get your Backblaze B2 credentials

In the [Backblaze B2 console](https://secure.backblaze.com/b2_buckets.htm):

1. **Pick or create a bucket.** Note its **name** (e.g. `my-backups`) and its **bucket ID** (alphanumeric, shown in the bucket list). `bb init` looks the ID up from the name for you — a bucket-scoped key works as long as it has the `listBuckets` capability (shown with the key on the Application Keys page); it only asks for the ID if that lookup fails.
2. **Application Keys → Add a New Application Key**, with read/write on that bucket. Backblaze shows a **keyID** and an **applicationKey** — copy both now; the applicationKey is shown only once. *(A bucket-scoped key is safer than your account master key.)*
3. *(S3 backend only)* On the **Buckets** page note the **Endpoint** (e.g. `s3.us-west-004.backblazeb2.com`); the **region** is its middle segment (`us-west-004`).

### 2. Configure — `bb init`

`init` prompts for each value and saves them (credentials encrypted) to `~/backup_repo/backup.db`:

```text
$ bb init
Backblaze keyID (access key ID): 0001abcdef0123456789
Backblaze applicationKey (secret, input hidden):
Bucket name: my-backups
Resolved bucket ID for "my-backups": e73ede9969c64827
S3 endpoint URL (e.g. https://s3.us-west-004.backblazeb2.com; blank = native B2 API only): s3.us-west-004.backblazeb2.com
S3 region (e.g. us-west-004; blank = derive from endpoint):
Region: us-west-004 (from endpoint)
Configuration saved.
Folder to watch (blank to skip): /home/me/Documents
Watching /home/me/Documents
```

If the lookup fails (for example the key lacks the `listBuckets` capability, or the name is misspelled), `init` prints why and asks `Bucket ID (for native B2 API):` instead — paste the ID from the B2 console.

- Only the **applicationKey** is secret, and it is **not shown as you type** (paste it and press Enter — nothing appears). Re-run `init` anytime to reconfigure — it overwrites the saved config.
- **Endpoint shortcuts:** paste the endpoint exactly as the Buckets page shows it (`https://` is added for you) and leave **region** blank — it's derived from `s3.<region>.backblazeb2.com`.
- **Using only the native B2 backend?** Leave **endpoint** and **region** blank: `init` then switches to the `b2` backend automatically (it needs the bucket ID — looked up automatically from the bucket name, or entered at the prompt if the lookup fails). With neither an endpoint nor a bucket ID, `init` refuses rather than save a config that can't upload. (Required: keyID, applicationKey, bucket name.)
- **Run `init` in a real terminal.** With no interactive input (e.g. `bb init </dev/null`, or a `!` shell inside an AI agent), it stops with `error: bb: input ended before setup was complete (run `bb init` in an interactive terminal)`.
- **Stay configured:** credentials persist encrypted on disk, so you never log in interactively again — each command silently re-authorizes with Backblaze using the saved key. You only re-run `init` if the key changes or is revoked.
- **Switch buckets later:** to point `bb` at a *different* bucket without re-entering credentials, use `bb bucket <name>` — the bucket ID is auto-resolved from your account (pass `bb bucket <name> <id>` to set it explicitly; if the lookup fails the ID is cleared, which is fine for an S3-only bucket, and the message tells you that command). Bucket names aren't case sensitive in B2: `bb bucket sc-office` finds `SC-Office` and stores B2's spelling. It changes only the bucket name + ID. Note: the stored key must have access to the new bucket — a bucket-scoped key won't, so for a bucket under a different key, re-run `init` instead.
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

- **Paths are stored absolute.** `bb watch docs/` or `bb watch ~/Documents` (a literal `~` is expanded too) saves the full path, so the daemon and the systemd service — which run from your home directory — back up the folder you meant. An empty path is refused. Watching a folder that's already on the list under another spelling (a trailing `/`, or different letter case on Windows) keeps the existing entry. `bb unwatch` accepts the exact text shown by `bb list` or any spelling of an absolute entry.
- **Store a folder under a bucket folder of your choice — `--remote`** (since v1.0.13 such a folder is *mapped*: kept in two-way sync, see 4c; the "already in the bucket" check below then applies to one-way folders only, since sync compares both sides itself). By default an object is named after the file's full local path (`home/me/Documents/a.txt`). `bb watch ~/Documents/Contracts --remote Contracts` stores `~/Documents/Contracts/2025/a.pdf` as `Contracts/2025/a.pdf` instead — handy to sync into a bucket folder that already exists. `bb list` / `bb config` show it as `path -> Contracts/`. It is `bb mapto Contracts` run inside that folder, with the same rules: a folder already watched or mapped isn't re-mapped in place (`bb unwatch`/`bb unmap` it first — re-mapping would upload everything again under new names), the prefix can't overlap another watched folder's object names (they'd overwrite each other) and can't be empty, `.`/`..`, or the bucket root.
  - **Nested folders:** a file belongs to the *innermost* watched folder. Watching `~/Documents` plainly and `~/Documents/Contracts --remote Contracts` stores the contracts under `Contracts/…` and everything else under `home/me/Documents/…`, each file once.
  - **Files already in the bucket aren't uploaded twice (native `b2` backend):** before uploading a file that isn't backed up under its name yet, `bb` asks B2 for that object's SHA-1; if it matches the local file, the file is just recorded as backed up (`bb upload` prints `Already in the bucket (recorded, not uploaded): N`). So you can download a bucket folder with `bb get Contracts/ ~/Documents/Contracts -r`, watch it with `--remote Contracts`, and only your later edits are uploaded. Any doubt (other content, a large file B2 hashes as `none`, the S3 backend, a lookup error) means a normal upload — on a bucket with Object Lock that is a version you can't delete. So check `bb backend` says `b2`, and **let no scan run until both the prefix and the files are in place**: stop the daemon, `bb watch <dir> --remote P` first (if `<dir>` sits inside an already-watched folder, a scan before that would upload the files under their full local paths), then `bb get P/ <dir> -r` (a scan during the download could upload half-written files), then `bb upload`, then start the daemon again. A failed lookup is reported as `hash lookup failed for N, uploaded instead`.
  - **Restart the daemon after changing the watch list** (`systemctl --user restart backuprepo`, or `bb stop` / `bb start`): real-time events are set up per folder at startup. (Each scan re-reads folders and prefixes, so a new prefix on a folder that sits inside an already-watched folder applies from the next scan even without a restart.)
- **Folders added by older versions** may be stored as relative paths (e.g. `docs`). They keep working as before; `bb watch` points them out. To convert one, `bb unwatch docs` then `bb watch /full/path/docs` — its files are then uploaded again under their new, absolute names.

`upload` uploads only what changed and prints a summary:

```text
$ bb upload
Uploaded: 12, Skipped: 0, Deleted: 0, Failed: 0
$ bb upload            # second run, nothing changed
Uploaded: 0, Skipped: 12, Deleted: 0, Failed: 0
```

Failed files are counted, don't abort the run, and make `bb` exit non-zero so scripts can detect a problem.

**Daemon mode (real-time backups) — turn it on with `bb start`:**

```sh
bb start            # turn ON daemon mode: watch all folders, back up changes in real time
bb stop             # turn it OFF: signal the running daemon to shut down gracefully
```

`bb start` *is* daemon mode: it watches your folders via filesystem events (`fsnotify`) for near-real-time backups, plus a full scan every 5 minutes as a safety net for anything the event stream misses. It runs in the **foreground** (you see activity; `Ctrl-C` stops it).

**Run it automatically in the background (Linux, systemd) — `bb service install`:**

```sh
make install                       # put bb at ~/.local/bin/bb first (the service runs that binary)
bb init                            # once: credentials + bucket (the service can't prompt)
bb service install                 # watch ~/Documents, then install + enable + start the service
bb service install --folder ~/Work # ...or watch a different folder (repeat to add more folders)
bb service status                  # installed? enabled? running?
journalctl --user -u backuprepo -f # follow the daemon's log
bb service uninstall               # stop, disable and remove it (watched folders are kept)
```

`bb service install` adds the folder to the watch list (default **`~/Documents`**; it must exist), writes a **systemd user unit** at `~/.config/systemd/user/backuprepo.service` that runs `bb start`, and runs `systemctl --user daemon-reload`, `enable` and `restart`. From then on the daemon starts at every login and backs up changes automatically. Pass `--delete` to bake deletion propagation into the service (see below).

- **After logout / at boot:** user services run only while you're logged in. To keep backing up after logout and start at boot, install with `--linger` (runs `loginctl enable-linger` for you; a failure is only a warning), or run `loginctl enable-linger $USER` once yourself. The install only prints this hint while lingering is off.
- **Adding folders later:** the daemon reads the watch list at startup, so after `bb watch <dir>` run `systemctl --user restart backuprepo` (or `bb service install --folder <dir>`, which does both).
- **Safety:** all checks run before the watch list or unit file is touched (only a later `systemctl` failure can leave the folder added). Install refuses if a daemon you started by hand (`bb start`) is running (`bb stop` it first), won't overwrite a hand-written `backuprepo.service` unless you pass `--force`, and refuses `--delete` while any watched folder is stored as a relative path (under systemd it would resolve against `$HOME`). After starting the unit it checks the daemon is still running and fails with a pointer to `journalctl` if not.
- **If the unit keeps failing** at startup, systemd gives up after 5 starts within 10 minutes and leaves it `failed` (`bb service status`). Fix the cause, then re-run `bb service install` (it clears the failed state) or `systemctl --user reset-failed backuprepo && systemctl --user restart backuprepo`.
- Run `bb service install` from the installed binary, not `go run` — the unit records the absolute path of the `bb` that installed it.

Elsewhere (Windows, macOS), use the OS service manager to run `bb start`.

For a quick one-off detached run instead: `nohup bb start &`. Either way, only **one** daemon runs at a time (guarded by `~/backup_repo/daemon.pid`); `bb stop` (or `systemctl --user stop`) ends it. On Linux and Windows the daemon also records its start time (in `~/backup_repo/daemon.start`), so a stale PID file whose PID now belongs to another program is recognised: `bb start` proceeds and `bb stop` removes the stale file instead of signalling that program.

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

- **`rm` only deletes objects in the bucket — never your local files.** Each line reads `deleted (remote) <object>` and a recursive delete ends with `Deleted N object(s) from bucket "<name>". Local files were not touched.` Object names mirror local paths without the leading `/` (e.g. `home/me/Documents/a.txt`), which is why they can look local. All versions of each object are removed.

- For `get`/`put`, the second path defaults to the basename if you omit it.
- `rm` deletes **all versions** of each object (on both backends) and **confirms** unless you pass `-f` or `-y`.
- **Flags go anywhere:** `-r`, `-f`, `-y`, and `--backend` may appear before, after, or between the path arguments — `bb rm photos/ -r -f` and `bb rm -r -f photos/` are equivalent.

### 4c. Map and sync a folder both ways (mode 3)

Keep a local folder and a bucket folder the same in **both directions**. Needs the native backend (`bb backend b2`).

```sh
cd ~/Documents/Contracts
bb mapto Contracts          # map THIS folder to bucket folder Contracts/ (created if it doesn't exist)
bb sync --dry-run           # preview the first sync: what would be downloaded / uploaded
bb sync                     # sync every mapped folder now (stop the daemon first)
systemctl --user restart backuprepo   # then let the daemon keep it in sync
bb maps                     # list mapped folders
bb unmap                    # stop syncing this folder (files stay on both sides)

mkdir ~/SC-OFFICE && cd ~/SC-OFFICE
bb sync --all --dry-run     # preview mirroring the WHOLE bucket here
bb sync --all               # map this folder to the whole bucket and sync it
bb sync --folder Contracts  # map this folder to bucket folder Contracts/ and sync it
```

- **Rules:** a change on one side is copied to the other. If a file changed on **both** sides since the last sync, the **newer** version wins; when that's the remote one, your local version is moved to `~/backup_repo/conflicts/<date-time>/<folder>/…` (never thrown away, never overwritten). **Nothing is ever deleted:** a file missing on one side is restored from the other. To really delete a synced file, delete it on both sides (e.g. the web UI's trash can, `bb serve`, removes the local file and its backup together). **On a bucket with Object Lock retention, B2 refuses to delete locked versions:** `bb rm` and the web trash can then fail (the web UI keeps the local file, since it deletes the remote copy first), so a synced file can't be removed until its retention period ends.
- **Automatic:** with the daemon running (`bb start` or `bb service install`), local changes in mapped folders sync seconds after you save, and remote changes (e.g. a colleague using the Backblaze web UI) come down on each **5-minute** scan. `bb upload` and the web UI's Upload button sync mapped folders too (the web UI prints the result in the `bb serve` terminal). Only one bb process syncs at a time (a lock in `~/backup_repo`); `bb sync` refuses while the daemon runs — stop it first, or use `--dry-run`. `bb mapto` does **not** restart the daemon, so you can preview first; `bb unmap` restarts a running service.
- **One local copy per bucket folder:** two mapped folders can't share bucket objects, a whole-bucket mirror (`--all`) can't coexist with other mapped folders, a mapped folder isn't re-mapped in place (`bb unmap` it first: re-mapping would upload everything again under new names), and a folder containing `~/backup_repo` can't be mapped. Folders backed up one way (`bb watch`), and watched folders nested in a mapped one, are left out of it in both directions.
- **Safety:** downloads go to a temporary `.bb-sync-*` file, are checked against the remote SHA-1, and replace the local file only if it didn't change meanwhile; a mapped folder that is missing (e.g. an unmounted drive) is skipped, never re-created; one where *every* previously synced file is missing (an empty mount point?) is refused until you run `bb sync --refill` in it; object names that would escape the folder (or that Windows can't store) are refused. Each run lists the bucket folder once (B2 class C call).
- **Times:** files `bb` uploads carry their modification time (`src_last_modified_millis`, as rclone and the web UI also set); downloads get the remote file's time, so a fresh download never looks "newer". For older objects without it, the upload time is used.
- `bb watch <dir> --remote P` is the explicit-path form of `bb mapto P`.

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
| `bb buckets` | List all buckets the key can see, with their IDs (native B2 API). A key restricted to one bucket can't list them all, so it shows just the configured bucket |
| `bb bucket [<name> [<id>]]` | Show, or switch to another bucket — changes only the bucket name + ID (keeps credentials/endpoint/region) |
| `bb appkey [<new-keyID>]` | Replace the stored application key — reads the secret from **stdin** (kept out of shell history); pass a new keyID to rotate the whole pair |
| `bb watch <dir> [--remote P]` | Add an existing directory to the watch list. `--remote P` maps it to bucket folder `P/` instead (two-way sync, exactly like `bb mapto P` run inside it) |
| `bb mapto <bucket-folder>` | Map the **current** folder to a bucket folder (created if missing) and keep them in two-way sync |
| `bb unmap [dir]` | Stop syncing a mapped folder (default: the current one); files stay on both sides |
| `bb maps` | List mapped folders |
| `bb sync [--all \| --folder F] [--dry-run]` | Two-way sync now: `--all` mirrors the whole bucket in the current folder, `--folder F` maps the current folder to `F/`, neither syncs every mapped folder. `--dry-run` previews |
| `bb unwatch <dir>` | Remove a directory from the watch list |
| `bb list` | List watched folders and tracked files with last-backup times |
| `bb status` | Configured? backend, watched-folder count, pending uploads, whether a daemon is running, and (Linux) the service state |
| `bb upload [--delete]` | Upload every changed file; no-op if nothing changed. `--delete` also removes remotes for locally-deleted files |
| `bb start [--delete]` | Run the watcher daemon (fsnotify + 5-min fallback scan) in the foreground until stopped |
| `bb stop` | Signal a running daemon to shut down gracefully |
| `bb serve` | Start the localhost web UI on `http://127.0.0.1:9171` (foreground) |
| `bb service install [--folder DIR] [--delete] [--force]` | Linux: watch DIR (default `~/Documents`), then install, enable and start the daemon as a systemd user service |
| `bb service uninstall` | Stop, disable and remove the service (watched folders are kept) |
| `bb service status` | Show whether the service is installed, enabled and running |
| `bb backend [s3\|b2]` | Show or set the stored backend |
| `bb ls [path] [-r]` | List bucket contents (folders shown with trailing `/`) |
| `bb get <remote> [local] [-r]` | Download an object, or a prefix with `-r` |
| `bb put <local> [remote] [-r] [--skip-existing]` | Upload a file, or a directory with `-r`; `--skip-existing` skips objects already in the bucket (presence-only) |
| `bb rm <path> [-r] [-f]` | Delete an object/prefix **in the bucket** (local files untouched); confirms unless `-f`/`-y` |
| `bb find <query> [prefix]` | Case-insensitive substring search of object names |
| `bb version` | Print the version (also `--version`, `-v`) |
| `bb help` | Print usage (also `-h`, `--help`, or `bb <command> -h`) |

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

Feature-complete against the original spec; current release **v1.0.12**. Design notes live in `docs/superpowers/`, decisions in `docs/project_notes/decisions.md`. Status of the former follow-ups:

- CI is a local GoCD pipeline (`.gocd/backuprepo.gocd.yaml`, see `deploy/gocd/README.md`): vet on Linux, Windows and macOS, the Linux tests with coverage, the Windows test binaries compiled (not run) and both release builds, for every commit on `master`.
- The ~22 MB binary size and Windows per-directory case sensitivity are accepted as they are (see ADR-025 in `docs/project_notes/decisions.md`).

---

## License

MIT — see [LICENSE](LICENSE).
