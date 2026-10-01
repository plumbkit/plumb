#!/bin/sh
# pre-commit-test.sh — run scripts/pre-commit against a stub golangci-lint.
#
# Why: the hook runs on every commit of every agent sharing this checkout, and
# two of its failure modes only show under that sharing (#545):
#
#   - a peer's golangci-lint holding the lock made the hook fail with "parallel
#     golangci-lint is running", failing a commit whose code was fine; and
#   - `run --fix` rewrote files AFTER the commit's content had been chosen, so a
#     commit could pass the hook while the tree it left behind was dirty, and the
#     reformatting drifted into whichever unrelated commit swept it up next.
#
# The stub stands in for golangci-lint (and for go), so this is hermetic and
# fast: it checks the HOOK's behaviour, not the linter's. Each expectation that
# asserts "the tree was not changed" has a control proving the stub WOULD have
# changed it under --fix, so the assertion cannot pass vacuously.
#
# Usage: scripts/pre-commit-test.sh [hook]   (default: scripts/pre-commit)
set -eu

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
HOOK="$(cd "$(dirname "${1:-$ROOT/scripts/pre-commit}")" && pwd)/$(basename "${1:-$ROOT/scripts/pre-commit}")"

work="$(mktemp -d "${TMPDIR:-/tmp}/pre-commit-test.XXXXXX")"
trap 'rm -rf "$work"' EXIT INT TERM

# ── stubs ────────────────────────────────────────────────────────────────────
mkdir -p "$work/bin"
cat >"$work/bin/go" <<'EOF'
#!/bin/sh
exit 0
EOF
# STUB_PEER: another golangci-lint holds the lock (refused unless the caller
# passes --allow-parallel-runners). STUB_UNFORMATTED: a.go is not formatted
# (--fix rewrites it; a check reports it). STUB_LINTFAIL: an ordinary finding.
cat >"$work/bin/golangci-lint" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >>"$STUB_LOG"
case "${1:-}" in
--version) echo "golangci-lint has version 2.13.2 built with go1.27.1"; exit 0 ;;
run) ;;
*) exit 0 ;;
esac
fix= par=
for a in "$@"; do
	[ "$a" = "--fix" ] && fix=1
	[ "$a" = "--allow-parallel-runners" ] && par=1
done
if [ -n "${STUB_PEER:-}" ] && [ -z "$par" ]; then
	echo 'level=error msg="Running error: parallel golangci-lint is running"'
	exit 5
fi
if [ -n "${STUB_UNFORMATTED:-}" ]; then
	if [ -n "$fix" ]; then
		printf 'package p\n\nfunc F() {}\n' >a.go
		exit 0
	fi
	echo 'a.go:3:1: File is not properly formatted (gofumpt)'
	echo '1 issues:'
	exit 1
fi
if [ -n "${STUB_LINTFAIL:-}" ]; then
	echo 'b.go:7:2: ineffectual assignment to err (ineffassign)'
	exit 1
fi
exit 0
EOF
chmod +x "$work/bin/go" "$work/bin/golangci-lint"

# ── a throwaway repository the hook runs in ─────────────────────────────────
repo="$work/repo"
mkdir -p "$repo/scripts"
for s in check-file-size.sh check-agents-brief.sh check-changelog-headings.sh; do
	printf '#!/bin/sh\nexit 0\n' >"$repo/scripts/$s"
	chmod +x "$repo/scripts/$s"
done
UNFORMATTED='package p

func  F() {}
'

total=0
failed=0
fail() {
	failed=$((failed + 1))
	echo "FAIL: $1" >&2
	[ -n "${2:-}" ] && printf '%s\n' "$2" | sed 's/^/    /' >&2
	return 0
}

# run_hook <env assignments...>: runs the hook in a fresh a.go; sets rc/out.
run_hook() {
	printf '%s' "$UNFORMATTED" >"$repo/a.go"
	: >"$work/log"
	set +e
	out="$(cd "$repo" && env PATH="$work/bin:$PATH" STUB_LOG="$work/log" "$@" sh "$HOOK" 2>&1)"
	rc=$?
	set -e
}

unchanged() { [ "$(cat "$repo/a.go")" = "$(printf '%s' "$UNFORMATTED")" ]; }

echo "pre-commit-test: $HOOK"

# 1. A peer's lint holding the lock must not fail this commit.
total=$((total + 1))
run_hook STUB_PEER=1
[ "$rc" -eq 0 ] || fail "a peer's golangci-lint made the hook fail (rc=$rc)" "$out"

# 2. Unformatted code FAILS the commit, names the file and the fix, and leaves
#    the tree exactly as it was.
total=$((total + 1))
run_hook STUB_UNFORMATTED=1
[ "$rc" -ne 0 ] || fail "an unformatted file passed the hook" "$out"
unchanged || fail "the hook rewrote a.go instead of failing" "$(cat "$repo/a.go")"
printf '%s' "$out" | grep -q 'not formatted' || fail "the hook did not say the failure is formatting" "$out"
printf '%s' "$out" | grep -q '^  a\.go$' || fail "the hook did not list a.go as unformatted" "$out"
printf '%s' "$out" | grep -q 'golangci-lint run --fix' || fail "the hook did not say how to fix it" "$out"

# 2c. Control for 2: the stub really does rewrite a.go under --fix, so the
#     unchanged() assertion above can fail.
total=$((total + 1))
printf '%s' "$UNFORMATTED" >"$repo/a.go"
(cd "$repo" && PATH="$work/bin:$PATH" STUB_LOG="$work/log" STUB_UNFORMATTED=1 golangci-lint run --fix ./...)
if unchanged; then fail "control: the stub's --fix did not change a.go, so check 2 is vacuous"; fi

# 3. A clean run passes the lock-sharing flag and never asks for a rewrite.
total=$((total + 1))
run_hook
[ "$rc" -eq 0 ] || fail "a clean tree failed the hook (rc=$rc)" "$out"
grep -q -- '^run .*--allow-parallel-runners' "$work/log" || fail "golangci-lint run was not given --allow-parallel-runners" "$(cat "$work/log")"
if grep -q -- '--fix' "$work/log"; then fail "the hook asked golangci-lint to rewrite files" "$(cat "$work/log")"; fi

# 4. An ordinary lint finding still fails, and is not mislabelled as formatting.
total=$((total + 1))
run_hook STUB_LINTFAIL=1
[ "$rc" -ne 0 ] || fail "a lint finding passed the hook" "$out"
printf '%s' "$out" | grep -q 'ineffassign' || fail "the lint finding was not shown" "$out"
if printf '%s' "$out" | grep -q 'not formatted'; then fail "a non-formatting finding was reported as formatting" "$out"; fi

echo "pre-commit-test: $total cases, $failed failed assertion(s)"
[ "$failed" -eq 0 ]
