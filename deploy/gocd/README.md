# GoCD CI for backuprepo

backuprepo is verified by **GoCD on andromeda** (`http://127.0.0.1:8153/go`), the same
server and Linux agent (`andromeda`, user `go`) as the Sentral apps and rentalmdl. It
replaced GitHub Actions on 2026-11-21 (ADR-026 in `docs/project_notes/decisions.md`).
How the server itself was set up: `~/projects/users_sentral/deploy/gocd/README.md`.

- Pipeline: `.gocd/backuprepo.gocd.yaml`, pipeline `backuprepo` in its **own group `backuprepo`**.
- Stage `verify` on every commit to `master`:
  - `go mod verify`, `make lint` (vet on linux/windows/darwin + gofmt) and the hook tests
  - `make coverage`: the Linux tests, with the total printed but not gated
  - `make wintest`: the Windows test binaries, compiled and linked but **not run** (no Windows agent)
  - `make build` + `make build-windows`: the static `bb` and `bb.exe` with their sha256 (not published)
- No deploy stage. Releases stay manual (CLAUDE.md "Releasing"); GoCD is the gate.
- GoCD builds from a **bare mirror**, `/var/lib/gocd-repos/backuprepo.git`, never the
  working tree. This clone's `post-commit`, `post-merge` and `post-rewrite` hooks
  (`deploy/gocd/mirror-hook`) push `master` to it. Only `master` is mirrored, so feature
  branches get no CI: merge, or run the same Makefile targets locally.
- The agent's Go is the **Fedora rpm's** (`/usr/bin/go`), not `~/.local/bin/go`. The
  first task prints `go version` and the commit.

## One-time setup

### 1. Mirror (the owner, sudo, on andromeda)

Run the whole block in **one** shell. Every use of `$M` is `${M:?}`: git treats an
**empty** `safe.directory` value as "reset the list", which would make the `go` user
distrust every other mirror and stop all pipelines. Safe to re-run. **Do not
`chown -R go:go`**: then edisonch can't push and the hooks fail.

```bash
M=/var/lib/gocd-repos/backuprepo.git
[ -d "${M:?}" ] || sudo git clone --bare --quiet /home/edisonch/projects/backuprepo "${M:?}"
sudo chown -R edisonch:go "${M:?}"
sudo chmod -R g+rX,o-rwx "${M:?}"
sudo find "${M:?}" -type d -exec chmod g+s {} +
sudo chmod 2750 "${M:?}"
git --git-dir="${M:?}" rev-parse --verify --quiet refs/heads/master >/dev/null \
  && git --git-dir="${M:?}" symbolic-ref HEAD refs/heads/master   # as edisonch
sudo -u go git config --global --get-all safe.directory | grep -qxF "${M:?}" \
  || sudo -u go git config --global --add safe.directory "${M:?}"
# verify: both must print the same commit
sudo -u go git --git-dir="${M:?}" log -1 --format='%h %s'
git -C /home/edisonch/projects/backuprepo log -1 --format='%h %s'
```

### 2. Hooks (this clone, no sudo)

`.git/hooks` is not tracked, so a fresh clone has none:

```bash
for h in post-commit post-merge post-rewrite; do install -m 0755 deploy/gocd/mirror-hook .git/hooks/$h; done
```

While the mirror is missing, each prints one "not found, skipping" line and exits 0.

### 3. Register the config repo (GoCD admin)

Done 2026-11-21 through the API with the owner's token (`-H @~/.config/gocd/owner.token`):
`POST /go/api/admin/config_repos` (Accept `v4`) with id `backuprepo`, plugin
`yaml.config.plugin`, material git `file:///var/lib/gocd-repos/backuprepo.git` branch
`master`, pattern `*.gocd.yaml`, and one rule: allow · refer · pipeline_group ·
`backuprepo`. Then the group's permissions: edisonch admin, the view-only user `claude`
view (`PUT /go/api/admin/pipeline_groups/backuprepo`, v1, `If-Match` its ETag).

The UI route, if it has to be redone: Admin > Config Repositories > Add, with the same
values. Leave Username / Password **empty**: a browser-filled login ends up in the
`file://` URL and git reads it as an ssh host ("Could not resolve hostname file").

A config repo that fails to parse does so **silently**: no pipeline, no email. Check its
parse status straight after any change to `.gocd/`.

## Checks (Claude's view-only token)

```bash
curl -s -H @$HOME/.config/gocd/claude.token -H 'Accept: application/vnd.go.cd.v1+json' \
  'http://127.0.0.1:8153/go/api/pipelines/backuprepo/history?page_size=10'
```

GoCD folds commits that land between polls into one run: `modifications[]` lists them all,
but only the newest, `build_cause.material_revisions[0].modifications[0].revision`, was
built. A release waits for a passed run whose **`modifications[0].revision` equals the
tagged SHA**, never "the latest run" and never "the SHA appears somewhere in the list".
If the commit was folded into a later run, trigger a run of it or tag the built commit.

## Troubleshooting

- "Modification check failed ... dubious ownership": the `safe.directory` line in step 1
  is missing for user `go`.
- The mirror is behind `master`: `git push --no-verify /var/lib/gocd-repos/backuprepo.git +master:master`.
- A task script with a literal hash breaks the whole config repo's parse: write two.
