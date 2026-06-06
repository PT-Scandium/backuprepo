# Architectural Decisions

Architectural Decision Records (ADRs) for backuprepo. Numbered sequentially; include date, context, decision, alternatives, and consequences.

> Note: several of these deliberately deviate from the original spec in `CLAUDE.md`, which describes the aspirational design. Where they conflict, **these ADRs reflect what was actually built.**

### ADR-001: Pure-Go SQLite + AES-256-GCM field encryption instead of SQLCipher (2026-06-06)

**Context:**
- `CLAUDE.md` specified SQLCipher (`go-sqlcipher`) for an encrypted DB.
- SQLCipher requires CGO, which conflicts with three other stated goals: single static binary, <10 MB size, and easy cross-compilation to Windows.

**Decision:**
- Use `modernc.org/sqlite` (pure Go, no CGO) for the database.
- Encrypt only the credential fields (keyID, applicationKey) with AES-256-GCM before insertion, in `internal/crypto`.

**Alternatives Considered:**
- `go-sqlcipher` (whole-DB encryption) → Rejected: CGO breaks static binary / small size / cross-compile.
- Pure-Go SQLCipher-format library → Rejected: less mature/maintained.

**Consequences:**
- ✅ Single static binary, no CGO, trivial cross-compile.
- ✅ Credentials never stored in plaintext (verified by `TestConfigEncryptedAtRest`, which reads raw DB bytes).
- ❌ Only credential fields are encrypted, not the whole DB (folder paths/metadata are plaintext).

### ADR-002: Master key stored in a 0600 key file (2026-06-06)

**Context:**
- The daemon (future) must start silently with no passphrase prompt, so the AES key must be available without interaction.

**Decision:**
- Store a random 32-byte key at `~/backup_repo/key` with mode `0600`, created on first run and reused thereafter.

**Alternatives Considered:**
- OS keyring (Credential Manager / GNOME Keyring / Keychain) → Rejected for now: extra dependency; revisit if stronger at-rest protection is needed.
- Machine-derived key (KDF over host attributes) → Rejected: predictable inputs, weak.
- Passphrase each start → Rejected: incompatible with a silent daemon.

**Consequences:**
- ✅ Silent start; simple; fully offline.
- ❌ Anyone who can read the user's home dir can read the key — protects against casual at-rest access only.

### ADR-003: Per-file uploads instead of per-folder tar+gzip (2026-06-06)

**Context:**
- `CLAUDE.md` mentioned tar+gzip compression of watched folders.

**Decision:**
- Upload one local file → one S3 object (`backup.RemoteKey` maps the absolute path to a stable key).

**Alternatives Considered:**
- tar+gzip per folder → Rejected: obscures per-file backup state, which the `list`/`status` commands (and the future web UI's per-file table) depend on.

**Consequences:**
- ✅ Meaningful per-file `last_backup` / `remote_key` tracking and change detection.
- ❌ No cross-file compression; many small files = many objects.

### ADR-004: Address B2 buckets by NAME, not bucket ID (2026-06-06)

**Context:**
- B2 has two identifiers: bucket name and numeric bucket ID. The user initially had the bucket ID.

**Decision:**
- `init` collects and stores the bucket **name** (plus S3 endpoint + region), because the S3-compatible API addresses buckets by name.

**Consequences:**
- ✅ Works with the aws-sdk-go-v2 S3 client directly.
- ❌ Users must look up the bucket name in the B2 console (documented in README).

### ADR-005: Typed errors in `internal/apperr`, not a root `errors.go` (2026-06-06)

**Context:**
- The spec named a root `errors.go` as the typed-error catalog. Go forbids `internal/` packages from importing the root `main` package.

**Decision:**
- Put the shared sentinel errors in `internal/apperr` so every package can import them. Errors wrap these with `%w`.

**Consequences:**
- ✅ `errors.Is(err, apperr.X)` works from any package including `main`.
- ✅ Preserves the "errors are typed, never raw strings" invariant.

### ADR-006: Uploader behind an interface with an in-memory fake (2026-06-06)

**Context:**
- The real B2/S3 upload path can't be unit-tested without live credentials.

**Decision:**
- Define `b2.Uploader` (`Upload`, `Exists`); `S3Uploader` is the real aws-sdk-go-v2 impl, `FakeUploader` is an in-memory map used in tests. `backup.Service` depends on the interface.

**Consequences:**
- ✅ All change-detection/backup logic is unit-tested with the fake.
- ❌ `S3Uploader` itself has no automated test — correctness verified by compilation + manual B2 run (see `issues.md`).

### ADR-007: Accept ~14 MB binary (over the <10 MB goal) (2026-06-06)

**Context:**
- `CLAUDE.md` targets a <10 MB binary. `aws-sdk-go-v2` + `modernc.org/sqlite` are large.

**Decision:**
- Ship the stripped (`-ldflags="-s -w"`) ~14 MB binary for now; do not trade away the SDK or pure-Go SQLite to hit the target.

**Consequences:**
- ✅ Robust, well-maintained S3 client and no-CGO SQLite.
- ❌ Binary exceeds the size goal — flagged for possible future trimming (e.g. a leaner S3 client).
