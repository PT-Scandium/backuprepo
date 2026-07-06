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

### 2026-07-06 - `bb put -r` aborts on a transient B2 pod failure (no upload retry) — FIXED v1.0.2
- **Issue**: `bb put -r <dir>` uploaded N files then died mid-batch with `upload failed: upload <key>: Post "https://pod-050-1046-12.backblaze.com/.../b2_upload_file/...": dial tcp 149.137.140.146:443: connect: connection refused`. Re-running hit the **same dead pod** intermittently (still in B2's rotation) and re-uploaded everything each time.
- **Root Cause**: B2 hands out a **per-pod upload URL** via `b2_get_upload_url`; that pod was briefly unreachable. `B2Backend.Upload` (and `uploadPart`) made a **single attempt** with no retry — so one dead pod failed the file, and `cli.Put`'s `WalkDir` callback returned the error, aborting the whole batch. Verified the main API was reachable (HTTP 401) while the specific pod refused (connection refused) — a transport-layer, single-pod issue, not auth/addressing.
- **Solution** (v1.0.2, ADR-018): added `uploadWithRetry` — up to `b2MaxUploadAttempts` (5) attempts, fetching a **fresh upload URL each time** (usually a different, healthy pod) with exponential backoff (200ms→3s, ctx-aware). Retryable: connection errors + `408/429/500/502/503/504`, and `401` (clears cached auth → re-authorizes); `400/403` fail fast. Wired into small-file `Upload` and large-file `uploadPart`. Tests: `TestB2UploadRetriesTransient`, `TestB2UploadFailsFastOnNonRetryable`, `TestB2UploadGivesUpAfterMaxAttempts`.
- **Prevention**: For B2 native uploads, **always** implement get-new-URL-and-retry — it's a documented, expected failure mode (a pod can vanish between URL issue and POST), not an edge case. S3 SDKs hide pod routing so they don't need this; the native client must. Consider making `put -r` continue-past-failures + skip already-uploaded files (still pending) so one bad file never abandons a batch and re-runs are cheap.

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
- **Issue**: For `ls`/`get`/`put`/`rm`/`find`, a flag placed after the path (e.g. `rm brtest/ -f`, `ls photos/ -r`) was silently dropped — `rm` would prompt and abort, `ls -r` wasn't recursive, `--backend` overrides were ignored.
- **Root Cause**: Each command called `fs.Parse(rest)` once; Go's `flag` parser stops at the first non-flag token, leaving any later flags as unparsed positionals.
- **Solution**: Added `parseFlags` (main.go) that resumes `fs.Parse` after each positional and returns the collected positionals; all five commands use it. Regression test `TestParseFlagsAnyOrder`.
- **Prevention**: When mixing flags and positionals with stdlib `flag`, never assume a single `Parse` honors trailing flags; parse interspersed or require flags-first explicitly.

### 2026-06-16 - B2 native Delete: 400 on b2_list_file_versions
- **Issue**: `backuprepo rm` against the native `b2` backend failed with `delete failed: list versions <key>: status 400`; the object was never deleted.
- **Root Cause**: `B2Backend.Delete` always included `startFileId: ""` (empty) in the first `b2_list_file_versions` request. Backblaze rejects `startFileId` unless a non-empty `startFileName` accompanies it → HTTP 400. Latent since the native backend shipped; never caught because the live e2e test was blocked on credentials and the httptest stub ignored the paging params. Unrelated to the v2→v3 migration.
- **Solution**: Only add `startFileName`/`startFileId` to the request body when non-empty (mirrors `listRaw`). Strengthened the httptest stub to 400 on an empty/oraphan `startFileId` so it can't regress. Verified live against bucket SC-OFFICE.
- **Prevention**: Don't send empty optional cursor params to B2; make test stubs validate request shape rather than ignore it. Run the live B2 e2e for any new bucket op.

Candidates that were caught during the build review (not bugs, but worth knowing — see decisions.md):
- Root `errors.go` from the spec was infeasible (Go forbids `internal/` importing package `main`) → typed errors moved to `internal/apperr` (ADR-005).
- SQLCipher requires CGO, conflicting with the static-binary goal → switched to pure-Go SQLite + AES-GCM field encryption (ADR-001).
