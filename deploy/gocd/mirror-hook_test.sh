#!/usr/bin/env bash
# Tests for deploy/gocd/mirror-hook against throwaway repos (no real mirror is
# touched). Run: bash deploy/gocd/mirror-hook_test.sh   (also run by GoCD).
set -euo pipefail

HOOK_SRC=$(cd "$(dirname "$0")" && pwd)/mirror-hook
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.invalid
export GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.invalid
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
# Run from inside a hook or `git rebase -x`, GIT_DIR and friends would point
# every git call below at the outer (real) repository.
unset $(git rev-parse --local-env-vars)

fail=0
check() { # check <name> <condition...>
    local name=$1; shift
    if "$@"; then echo "ok   $name"; else echo "FAIL $name"; fail=1; fi
}

repo=$WORK/clone
git init -q -b master "$repo"
for h in post-commit post-merge post-rewrite; do install -m 0755 "$HOOK_SRC" "$repo/.git/hooks/$h"; done
export BACKUPREPO_GOCD_CANON="$repo/.git"
export BACKUPREPO_GOCD_MIRROR="$WORK/mirror.git"
commit() { git -C "$repo" commit -q --allow-empty -m "$1" 2>&1; }
mirror_head() { git --git-dir="$BACKUPREPO_GOCD_MIRROR" rev-parse --verify --quiet refs/heads/master 2>/dev/null || true; }
head_of() { git -C "$repo" rev-parse "$1"; }

# 1. Mirror missing: warns, the commit still succeeds.
out=$(commit one)
check "missing mirror warns and skips" grep -q "not found, skipping" <<<"$out"

# 2. On master: pushes, mirror follows HEAD.
git init -q --bare "$BACKUPREPO_GOCD_MIRROR"
out=$(commit two)
check "post-commit reports the mirror" grep -q "post-commit: mirrored master -> GoCD" <<<"$out"
check "mirror equals master" test "$(mirror_head)" = "$(head_of master)"

# 3. Amend rewrites master: the forced push follows it.
git -C "$repo" commit -q --amend --allow-empty -m "two amended" >/dev/null 2>&1
check "amend is force-mirrored" test "$(mirror_head)" = "$(head_of master)"

# 4. Another branch: the mirror stays on master.
before=$(mirror_head)
git -C "$repo" switch -q -c feature
commit feature >/dev/null
check "feature branch is not mirrored" test "$(mirror_head)" = "$before"

# 5. Merge into master: post-merge mirrors it.
git -C "$repo" switch -q master
out=$(git -C "$repo" merge -q --no-ff -m merge feature 2>&1)
check "post-merge reports the mirror" grep -q "post-merge: mirrored master -> GoCD" <<<"$out"
check "merge is mirrored" test "$(mirror_head)" = "$(head_of master)"

# 6. Rebase rewrites master: post-rewrite mirrors it.
git -C "$repo" switch -q -c base HEAD~1
commit side >/dev/null
git -C "$repo" switch -q master
out=$(git -C "$repo" rebase -q base 2>&1)
check "post-rewrite reports the mirror" grep -q "post-rewrite: mirrored master -> GoCD" <<<"$out"
check "rebase is mirrored" test "$(mirror_head)" = "$(head_of master)"

# 7. Worktrees share the clone: off master nothing moves, on master it mirrors.
git -C "$repo" worktree add -q "$WORK/wt" base
before=$(mirror_head)
git -C "$WORK/wt" commit -q --allow-empty -m wt-base >/dev/null 2>&1
check "worktree off master is not mirrored" test "$(mirror_head)" = "$before"
git -C "$repo" switch -q --detach
git -C "$WORK/wt" switch -q master
git -C "$WORK/wt" commit -q --allow-empty -m wt-master >/dev/null 2>&1
check "worktree on master is mirrored" test "$(mirror_head)" = "$(head_of master)"
git -C "$WORK/wt" switch -q --detach
git -C "$repo" switch -q master

# 8. A different clone with the same hook: never writes the mirror.
other=$WORK/other
git clone -q "$repo" "$other"
install -m 0755 "$HOOK_SRC" "$other/.git/hooks/post-commit"
before=$(mirror_head)
git -C "$other" commit -q --allow-empty -m other >/dev/null 2>&1
check "foreign clone is not mirrored" test "$(mirror_head)" = "$before"

# 9. Push fails (mirror is not a repository): warns, exits 0.
rm -rf "$BACKUPREPO_GOCD_MIRROR"; mkdir "$BACKUPREPO_GOCD_MIRROR"
set +e
out=$(commit three); rc=$?
set -e
check "failed push still lets the commit succeed" test "$rc" -eq 0
check "failed push warns with a retry line" grep -q "retry with: git push --no-verify" <<<"$out"

exit "$fail"
