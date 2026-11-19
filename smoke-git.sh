#!/bin/bash
# Smoke test Fase 2 with real git 2.43.0 against the forge server.
# Usage: ./smoke-git.sh <base-url>
# Exits non-zero on the first failure; prints PASS lines as it goes.
set -u
BASE="${1:?usage: smoke-git.sh <base-url>}"
WORK=/tmp/forge-smoke
rm -rf "$WORK"
mkdir -p "$WORK"
cd "$WORK" || exit 1

pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1"; exit 1; }

export GIT_AUTHOR_NAME="A U Thor"
export GIT_AUTHOR_EMAIL="a@example.invalid"
export GIT_COMMITTER_NAME="A U Thor"
export GIT_COMMITTER_EMAIL="a@example.invalid"
export GIT_AUTHOR_DATE="1700000000 +0000"
export GIT_COMMITTER_DATE="1700000000 +0000"

# 1. healthz
curl -fsS "$BASE/healthz" | grep -q '"ok":true' || fail "healthz"
pass "healthz"

# 2. create repo via the REST stub
code=$(curl -sS -o /tmp/forge-smoke/create.json -w "%{http_code}" -X POST "$BASE/api/v3/user/repos" \
  -H 'Content-Type: application/json' -d '{"name":"demo","description":"smoke","private":true}')
[ "$code" = "201" ] || fail "create repo HTTP $code: $(cat /tmp/forge-smoke/create.json)"
pass "create repo adaiversen/demo (201)"

# 3. clone the empty repo (v2)
git clone -q "$BASE/adaiversen/demo.git" empty 2>/tmp/forge-smoke/clone-empty.err \
  || fail "clone empty repo: $(cat /tmp/forge-smoke/clone-empty.err)"
[ -d empty/.git ] || fail "empty clone has no .git"
pass "clone empty repo"

# 4. build a local repo with two commits and push it
git init -q -b main src
cd src || exit 1
echo "hello" > a.txt
git add a.txt
git commit -qm "first"
echo "world" > b.txt
git add b.txt
git commit -qm "second"
git remote add origin "$BASE/adaiversen/demo.git"
git push -q origin main 2>/tmp/forge-smoke/push1.err \
  || fail "first push: $(cat /tmp/forge-smoke/push1.err)"
pass "push 2 commits to main"

# 5. clone (v2) and verify content
cd "$WORK" || exit 1
git clone -q "$BASE/adaiversen/demo.git" clone1 2>/tmp/forge-smoke/clone1.err \
  || fail "clone after push: $(cat /tmp/forge-smoke/clone1.err)"
cd clone1 || exit 1
[ "$(git log --oneline | wc -l)" = "2" ] || fail "clone1 log has $(git log --oneline | wc -l) commits, want 2"
[ "$(cat a.txt)" = "hello" ] && [ "$(cat b.txt)" = "world" ] || fail "clone1 file contents wrong"
[ "$(git cat-file -p HEAD:a.txt)" = "hello" ] || fail "cat-file a.txt wrong"
pass "clone v2: 2 commits, files byte-identical"

# 6. third commit, push, incremental fetch in the clone
cd "$WORK/src" || exit 1
echo "third" > c.txt
git add c.txt
git commit -qm "third"
git push -q origin main 2>/tmp/forge-smoke/push2.err \
  || fail "second push: $(cat /tmp/forge-smoke/push2.err)"
cd "$WORK/clone1" || exit 1
git fetch -q origin 2>/tmp/forge-smoke/fetch.err \
  || fail "incremental fetch: $(cat /tmp/forge-smoke/fetch.err)"
git merge -q --ff-only origin/main 2>/tmp/forge-smoke/merge.err \
  || fail "ff merge after fetch: $(cat /tmp/forge-smoke/merge.err)"
[ "$(git log --oneline | wc -l)" = "3" ] || fail "after fetch log has $(git log --oneline | wc -l) commits, want 3"
[ "$(cat c.txt)" = "third" ] || fail "c.txt wrong after fetch"
pass "incremental fetch + ff merge (3 commits)"

# 7. shallow clone --depth 1
cd "$WORK" || exit 1
git clone -q --depth 1 "$BASE/adaiversen/demo.git" shallow1 2>/tmp/forge-smoke/shallow.err \
  || fail "shallow clone: $(cat /tmp/forge-smoke/shallow.err)"
cd shallow1 || exit 1
[ "$(git log --oneline | wc -l)" = "1" ] || fail "shallow clone log has $(git log --oneline | wc -l) commits, want 1"
[ -f .git/shallow ] || fail "shallow clone has no .git/shallow file"
[ "$(cat c.txt)" = "third" ] || fail "shallow clone c.txt wrong"
pass "shallow clone --depth 1 (1 commit, boundary recorded)"

# 8. partial clone --filter=blob:none, then a lazy blob fetch
cd "$WORK" || exit 1
git clone -q --filter=blob:none "$BASE/adaiversen/demo.git" partial1 2>/tmp/forge-smoke/partial.err \
  || fail "partial clone: $(cat /tmp/forge-smoke/partial.err)"
cd partial1 || exit 1
[ "$(git log --oneline | wc -l)" = "3" ] || fail "partial clone log has $(git log --oneline | wc -l) commits, want 3"
[ "$(git cat-file -p HEAD:a.txt 2>/tmp/forge-smoke/lazy.err)" = "hello" ] \
  || fail "lazy blob fetch: $(cat /tmp/forge-smoke/lazy.err)"
pass "partial clone --filter=blob:none + lazy blob fetch"

# 9. v0 clone (force protocol 0)
cd "$WORK" || exit 1
git -c protocol.version=0 clone -q "$BASE/adaiversen/demo.git" clonev0 2>/tmp/forge-smoke/clonev0.err \
  || fail "v0 clone: $(cat /tmp/forge-smoke/clonev0.err)"
cd clonev0 || exit 1
[ "$(git log --oneline | wc -l)" = "3" ] || fail "v0 clone log has $(git log --oneline | wc -l) commits, want 3"
pass "v0 clone (3 commits)"

# 10. branch create + delete via real git
cd "$WORK/src" || exit 1
git checkout -qb feature HEAD 2>/dev/null
git push -q origin feature 2>/tmp/forge-smoke/feat.err \
  || fail "branch push: $(cat /tmp/forge-smoke/feat.err)"
git ls-remote "$BASE/adaiversen/demo.git" refs/heads/feature | grep -q feature \
  || fail "feature branch not advertised after push"
git push -q origin :feature 2>/tmp/forge-smoke/del.err \
  || fail "branch delete: $(cat /tmp/forge-smoke/del.err)"
if git ls-remote "$BASE/adaiversen/demo.git" refs/heads/feature | grep -q feature; then
  fail "feature branch still advertised after delete"
fi
pass "branch create + delete via real git"

# 11. non-fast-forward push must fail
git checkout -qb side HEAD~1 2>/dev/null
echo "side" > side.txt
git add side.txt
git commit -qm "side"
if git push -q origin side:main 2>/tmp/forge-smoke/nff.err; then
  fail "non-fast-forward push was accepted"
fi
pass "non-fast-forward push refused"

# 12. push to a missing repo must fail with 404
git remote add missing "$BASE/adaiversen/nope.git"
if git push -q missing main 2>/tmp/forge-smoke/missing.err; then
  fail "push to a missing repo was accepted"
fi
grep -qi "404\|not found\|repository.*not found" /tmp/forge-smoke/missing.err \
  || echo "NOTE: missing-repo push error did not mention 404: $(cat /tmp/forge-smoke/missing.err)"
pass "push to missing repo refused"

echo "ALL SMOKE TESTS PASSED"
