# Bug Log

Chronological log of bugs and their fixes. Each entry: date, issue, root cause, solution, prevention. Keep entries brief; remove very old (6+ month) entries when irrelevant.

## Format

```
### YYYY-MM-DD - Brief Description
- **Issue**: What went wrong
- **Root Cause**: Why it happened
- **Solution**: How it was fixed
- **Prevention**: How to avoid it
```

## Entries

### 2026-11-19 - `bb <command> -h` ran the command, created state, or used "-h" as a name
- **Issue**: Flag commands (`upload/start/ls/find/get/put/rm/service install`) printed the flag package's usage and exited 1 after creating `~/backup_repo`. No-flag commands ignored `-h` and ran: `bb init -h` started setup, and `bb stop -h` stopped the daemon. Positional commands treated "-h" as a value: `bb watch -h` took it as the path `<cwd>/-h` (watched if that directory existed, otherwise "folder not found" after creating state), and `bb bucket -h` would switch to a bucket named "-h". Found in the 2026-11-18 docs audit.
- **Root Cause**: `run` answered help only as the first argument (`bb -h`). Each subcommand's arguments reached `config.Load`/`store.Open` and the command itself unchecked.
- **Solution**: `helpRequested` scans the subcommand's arguments for the flag package's help spellings (`isHelpArg`: `-h`, `--h`, `-help`, `--help`, also with `=value`) and stops at `--`. Top-level `bb --h` / `bb -help` now count too. `run` checks it after the `isCommand` gate and before `config.Load`: it prints usage to stdout and exits 0.
- **Prevention**: `TestSubcommandHelpCreatesNoState` runs every command in the dispatch switch (parsed from main.go) with `-h`, `--help` and a trailing `-help`, plus `service install -h`. It asserts exit 0 and no `~/backup_repo`, so a newly added command is covered automatically. It fails with the check disabled.

### 2026-11-18 - `make bump` breaks after the version reaches major 21
- **Issue**: A patch bump at `20.20.20`, a minor bump at `20.20.x`, or a major bump at `20.x.x` wrote `21.0.0` to VERSION; the next `make bump*` then failed with `version "21.0.0": component "21" must be an integer 0..20`. Latent (current version 1.0.9); found in the 2026-11-18 docs audit.
- **Root Cause**: `version.Parse` applied the `0..Max` bound to all three components, while `Bump` (and the package doc) treat major as unbounded.
- **Solution**: `Parse` bounds only minor/patch; major is any non-negative integer. Tests `TestParseUnboundedMajor`, `TestBumpOutputAlwaysParses` (every Bump output must parse back), and end-to-end `cmd/bump` `TestRunBumpsPastMajor20`.
- **Prevention**: When a type has a producer and a parser, test the round trip (`Parse(Bump(v).String())`) at the boundaries, not each side alone.

### 2026-11-18 - Bucket-ID auto-resolve fails with bucket-scoped keys
- **Issue**: `bb init` / `bb bucket <name>` couldn't resolve the bucket ID for an application key restricted to one bucket (the README-recommended setup), so users always got `Could not auto-resolve bucket ID (...)` and the manual prompt. Found in the 2026-11-18 docs audit; ADR-017 had claimed scoped keys still resolve their own bucket.
- **Root Cause**: `ListBuckets` sent `b2_list_buckets` with `accountId` only. Backblaze's docs: a bucket-restricted token "must include the bucketId or bucketName of that bucket in the request, or the request will be denied". The unit-test mock accepted any token, so tests never saw the denial.
- **Solution**: New `b2.FindBucket` sends `bucketName`; `resolveBucketID` passes the bucket name and `init`/`bucket` use `FindBucket`. Mock server now denies unfiltered lists for a restricted key. Tests: `TestB2FindBucketRestrictedKey`, `TestB2FindBucketAccountKey`, `TestB2FindBucketEmptyName`, `TestInitPassesBucketNameToLookup`, `TestBucketAutoResolvesID/passes_bucket_name_to_lookup`, and `TestBucketIDLookupWiring` (AST check that main.go passes `b2.FindBucket` to `cli.Init`/`cli.Bucket`).
- **Prevention**: When mocking a third-party API, model its permission rules (restricted vs. unrestricted credentials), not just the happy-path response shape; check the provider's docs for per-credential restrictions before claiming behaviour in an ADR.

### 2026-07-06 - `bb put -r` aborts on a transient B2 pod failure (no upload retry) — FIXED v1.0.2
- **Issue**: `bb put -r <dir>` uploaded N files then died mid-batch with `upload failed: upload <key>: Post "https://pod-050-1046-12.backblaze.com/.../b2_upload_file/...": dial tcp 149.137.140.146:443: connect: connection refused`. Re-running hit the **same dead pod** intermittently (still in B2's rotation) and re-uploaded everything each time.
- **Root Cause**: B2 hands out a **per-pod upload URL** via `b2_get_upload_url`; that pod was briefly unreachable. `B2Backend.Upload` (and `uploadPart`) made a **single attempt** with no retry — so one dead pod failed the file, and `cli.Put`'s `WalkDir` callback returned the error, aborting the whole batch. Verified the main API was reachable (HTTP 401) while the specific pod refused (connection refused) — a transport-layer, single-pod issue, not auth/addressing.
- **Solution** (v1.0.2, ADR-018): added `uploadWithRetry` — up to `b2MaxUploadAttempts` (5) attempts, fetching a **fresh upload URL each time** (usually a different, healthy pod) with exponential backoff (200ms→3s, ctx-aware). Retryable: connection errors + `408/429/500/502/503/504`, and `401` (clears cached auth → re-authorizes); `400/403` fail fast. Wired into small-file `Upload` and large-file `uploadPart`. Tests: `TestB2UploadRetriesTransient`, `TestB2UploadFailsFastOnNonRetryable`, `TestB2UploadGivesUpAfterMaxAttempts`.
- **Prevention**: For B2 native uploads, **always** implement get-new-URL-and-retry — it's a documented, expected failure mode (a pod can vanish between URL issue and POST), not an edge case. S3 SDKs hide pod routing so they don't need this; the native client must. Consider making `put -r` continue-past-failures + skip already-uploaded files so one bad file never abandons a batch and re-runs are cheap — done in v1.0.3 (ADR-019, `--skip-existing`).

### 2026-07-06 - `bb buckets` 401 "authentication failed" — Account ID entered as keyID
- **Issue**: After `bb init`, `bb buckets` (and any B2 op) failed with `authentication failed: status 401`. Config looked populated.
- **Root Cause**: **User config error, not a code bug** — the stored **keyID was 12 hex chars** (`e02fd8314d56`), i.e. the Backblaze **Account ID**, not an applicationKey **keyID** (~25 chars). `b2_authorize_account` rejects the bad keyID+secret pair → 401, surfaced as `apperr.ErrAuthFailed`. (Two related config slips in the same session: **endpoint/region left blank** — harmless for `bb buckets`/native B2, breaks the default `s3` backend; and a **name/ID mismatch**, stored name `Scandiumsc` paired with `SC-OFFICE`'s bucket ID, from manually pasting an ID during a bad-cred init.)
- **Solution**: Re-set credentials with `bb appkey <real-25-char-keyID>` (reads the secret no-echo from stdin — no full re-init). `bb bucket <name>` fixes the name/ID mismatch by auto-resolving a consistent pair (ADR-017).
- **Prevention**: The **error type localized it**: `ErrAuthFailed` comes only from the `authorize` step (bad key pair), never from `b2_list_buckets` (a capability/permission problem would surface as `ErrListFailed: ... status 401`). Remember: **keyID ≠ Account ID ≠ keyName**; only keyID + applicationKey authenticate, and `keyName` is not stored by `bb`. Documented in key_facts.md "Credential gotcha".

### 2026-06-16 - Web UI CSRF on POST endpoints
- **Issue**: The web UI's state-changing POST endpoints (`/upload`, `/delete`, `/close`) had no CSRF defense. A malicious web page the user visited could auto-submit a cross-site form to `http://127.0.0.1:9171/...` and drive the UI — including the **destructive local+remote delete** and stopping the server. Flagged HIGH by automated security review on the `feat/web-ui` merge.
- **Root Cause**: The server relied on the `Host`-header guard, which stops DNS-rebinding but NOT a cross-site form POST — the browser sends those with the real `Host: 127.0.0.1:9171`. With no auth, the server trusted every localhost request. ADR-015 had even (wrongly) claimed the Host guard mitigated CSRF.
- **Solution**: Added `sameOrigin(r)` in `requirePost`: each POST must carry an `Origin` (or `Referer` fallback) whose host equals the server's, failing closed when absent. The UI's own forms send a matching Origin; an attacker page sends its own and is rejected. Test `TestCSRFRejectsCrossOriginPost`; ADR-015 corrected.
- **Prevention**: For any no-auth localhost server, a Host/DNS-rebinding guard is necessary but **not sufficient** — add an Origin/Referer (or token) CSRF check to every state-changing endpoint. Don't conflate the two defenses.

### 2026-06-16 - Flags after the path silently ignored (`rm`/`ls`/`get`/`put`/`find`)
- **Issue**: For `ls`/`get`/`put`/`rm`/`find`, a flag placed after the path (e.g. `rm brtest/ -f`, `ls photos/ -r`) was silently dropped — `rm` would prompt and abort, `ls -r` wasn't recursive, `--backend` overrides were ignored.
- **Root Cause**: Each command called `fs.Parse(rest)` once; Go's `flag` parser stops at the first non-flag token, leaving any later flags as unparsed positionals.
- **Solution**: Added `parseFlags` (main.go) that resumes `fs.Parse` after each positional and returns the collected positionals; all five commands use it. Regression test `TestParseFlagsAnyOrder`.
- **Prevention**: When mixing flags and positionals with stdlib `flag`, never assume a single `Parse` honors trailing flags; parse interspersed or require flags-first explicitly.

### 2026-06-16 - B2 native Delete: 400 on b2_list_file_versions
- **Issue**: `backuprepo rm` against the native `b2` backend failed with `delete failed: list versions <key>: status 400`; the object was never deleted.
- **Root Cause**: `B2Backend.Delete` always included `startFileId: ""` (empty) in the first `b2_list_file_versions` request. Backblaze rejects `startFileId` unless a non-empty `startFileName` accompanies it → HTTP 400. Latent since the native backend shipped; never caught because the live e2e test was blocked on credentials and the httptest stub ignored the paging params. Unrelated to the v2→v3 migration.
- **Solution**: Only add `startFileName`/`startFileId` to the request body when non-empty (mirrors `listRaw`). Strengthened the httptest stub to 400 on an empty/oraphan `startFileId` so it can't regress. Verified live against bucket SC-OFFICE.
- **Prevention**: Don't send empty optional cursor params to B2; make test stubs validate request shape rather than ignore it. Run the live B2 e2e for any new bucket op.

### 2026-11-15 - `bb init` saved an S3 config with no endpoint/region; daemon couldn't upload
- **Issue**: User left *S3 endpoint URL* and *S3 region* blank (README said that's fine for the B2 backend). `init` saved them empty but kept the default `s3` backend, so every upload failed: `failed to resolve service endpoint ... A region must be set`. Fixed live with `bb backend b2`.
- **Root Cause**: `init` never validated that the stored backend could work with the answers; choosing `b2` was a separate manual step the docs didn't make obvious.
- **Solution (v1.0.5)**: blank endpoint or region → `init` sets backend `b2` (needs a bucket ID; refuses before saving if there is none). Endpoint host without scheme gets `https://`; blank region is derived from `s3.<region>.backblazeb2.com`. Tests: `TestInitBlankEndpointUsesNativeB2` and siblings in `cli_test.go`.
- **Prevention**: Validate a config against the backend that will use it at save time, not at first upload.

### 2026-11-15 - `bb init` without a terminal blamed the user; `bb version` created state
- **Issue**: `! bb init` (agent shell, no interactive stdin) printed all prompts on one line and failed with "keyID, applicationKey and bucket are required". Separately, `bb version` with a real HOME created `~/backup_repo/key` + an empty `backup.db`.
- **Root Cause**: `init` ignored `ReadString` EOF errors; `run()` called `config.Load` + `store.Open` before dispatching any subcommand.
- **Solution (v1.0.5)**: `init` tracks EOF and returns `apperr.ErrNoInput` ("run `bb init` in an interactive terminal") when required answers are missing because input ended; piped answers still work. `version`/`help` return before `config.Load`. Tests: `TestInitNoInput`, `TestVersionAndHelpCreateNoState`.
- **Prevention**: Commands that need no state must not open it; interactive commands should distinguish "no input" from "blank answer".

### 2026-11-15 - `bb rm` output mistaken for local deletions
- **Issue**: After `bb rm -r home/ -y`, lines like `deleted home/edisonch/Documents/...` made the user think local files were deleted (they weren't — `Rm` only calls `be.Delete`).
- **Root Cause**: Remote keys mirror local paths minus the leading `/`, and the output never said "bucket" or "remote".
- **Solution (v1.0.5)**: prompt names the bucket and says local files are not affected; lines read `deleted (remote) <key>`; recursive rm ends with `Deleted N object(s) from bucket "<name>". Local files were not touched.`; a mid-batch failure reports how many were already deleted. Tests: `TestRmOutputSaysRemote`, `TestRmRecursiveReportsPartialFailure`.
- **Prevention**: Destructive commands must say *where* they act.

### 2026-11-15 - `bb init` showed the applicationKey on screen
- **Issue**: During `bb init` the applicationKey prompt echoed the secret as typed (into terminal scrollback), unlike `bb appkey` (ADR-016).
- **Root Cause**: `Init` read the secret with the same plain `ask` line prompt as the other answers; reusing `readSecret` wasn't possible because it creates a second buffered reader that desyncs piped input.
- **Solution**: `readSecretFrom` reads without echo on a terminal, else from Init's shared reader (ADR-022); `readHidden` restores the terminal and exits 130 on Ctrl-C (review found echo was left off). Unit tests cover the piped path (`TestInitPipedSecretStaysInOrder`, `TestInitInputEndsAtSecret`, `TestReadSecretFromAndEOF`); the terminal path is verified by a manual pty check (old vs new, Ctrl-C).
- **Prevention**: Any prompt for a secret goes through the no-echo helper; anything that changes terminal modes must restore them on interrupt.

### 2026-11-15 - `bb stop` could signal an unrelated process after PID reuse
- **Issue**: The PID file held only the PID; after an unclean daemon exit the PID could be reused by another program of the same user. `bb start` then refused ("already running") and `bb stop` sent SIGTERM to that program.
- **Root Cause**: Liveness = "a process with this PID exists" (signal 0); no identity check before signalling.
- **Solution**: Record `<pid> <starttime>` (`/proc/<pid>/stat` field 22) in a separate `daemon.start` file (`daemon.pid` stays a bare PID so older bb versions can still read it) and require a match; `Stop` removes a stale pair instead of signalling (ADR-023). Linux only at the time; Windows followed in v1.0.8 (ADR-024); other OSes remain PID-only.
- **Prevention**: Never act on a PID without verifying it's still the same process.

### 2026-11-15 - `bb watch` stored relative paths; `unwatch` needed the exact text; typos created state
- **Issue**: `bb watch docs/` stored `docs/` (a different folder under the systemd service, cwd `$HOME`); `bb unwatch /home/u/Documents` failed when `/home/u/Documents/` was stored; `bb bogus` created `~/backup_repo`.
- **Root Cause**: Paths stored/matched verbatim; `run` opened state before rejecting unknown commands.
- **Solution**: `normalizeFolder` + equivalent-entry matching in `Watch`/`Unwatch`; `isCommand` allowlist before `config.Load` (ADR-023).
- **Prevention**: Normalise user-supplied paths at the boundary; answer/reject commands before creating state.

### 2026-11-15 - Windows: exited daemon could look alive; no PID-reuse protection; case-sensitive folder matching
- **Issue**: Windows `processAlive` treated "the process object can be opened" as alive (it survives exit while any handle is open); Windows had no start-time identity (ADR-023 was Linux-only), so a reused PID could be signalled/killed by `bb stop`; `C:\Users\X` and `c:\users\x` were different watch entries.
- **Root Cause**: Unix-shaped liveness/identity; string-exact path comparison.
- **Solution**: exit code `STILL_ACTIVE` check + `GetProcessTimes` creation time in `daemon.start`; `sameFolder` (EqualFold on Windows) in path-only folder matching (ADR-024; an `os.SameFile` variant was dropped after review — it hid real folders behind stored symlinks).
- **Prevention**: Compare paths the way the OS does; never treat "handle opened" as "running" on Windows. (Not executed on Windows — see ADR-024.)

Candidates that were caught during the build review (not bugs, but worth knowing — see decisions.md):
- Root `errors.go` from the spec was infeasible (Go forbids `internal/` importing package `main`) → typed errors moved to `internal/apperr` (ADR-005).
- SQLCipher requires CGO, conflicting with the static-binary goal → switched to pure-Go SQLite + AES-GCM field encryption (ADR-001).
