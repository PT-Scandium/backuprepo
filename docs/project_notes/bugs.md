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

### 2026-06-16 - B2 native Delete: 400 on b2_list_file_versions
- **Issue**: `backuprepo rm` against the native `b2` backend failed with `delete failed: list versions <key>: status 400`; the object was never deleted.
- **Root Cause**: `B2Backend.Delete` always included `startFileId: ""` (empty) in the first `b2_list_file_versions` request. Backblaze rejects `startFileId` unless a non-empty `startFileName` accompanies it → HTTP 400. Latent since the native backend shipped; never caught because the live e2e test was blocked on credentials and the httptest stub ignored the paging params. Unrelated to the v2→v3 migration.
- **Solution**: Only add `startFileName`/`startFileId` to the request body when non-empty (mirrors `listRaw`). Strengthened the httptest stub to 400 on an empty/oraphan `startFileId` so it can't regress. Verified live against bucket SC-OFFICE.
- **Prevention**: Don't send empty optional cursor params to B2; make test stubs validate request shape rather than ignore it. Run the live B2 e2e for any new bucket op.

Candidates that were caught during the build review (not bugs, but worth knowing — see decisions.md):
- Root `errors.go` from the spec was infeasible (Go forbids `internal/` importing package `main`) → typed errors moved to `internal/apperr` (ADR-005).
- SQLCipher requires CGO, conflicting with the static-binary goal → switched to pure-Go SQLite + AES-GCM field encryption (ADR-001).
