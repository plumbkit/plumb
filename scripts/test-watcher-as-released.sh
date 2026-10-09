#!/usr/bin/env bash
# Run the file-watcher and SQLite-lock tests the way macOS releases are built:
# CGO_ENABLED=0, on both shipped architectures (arm64 natively, amd64 under
# Rosetta 2).
#
# Why: release binaries are CGO_ENABLED=0, while `make verify` on a Mac builds
# with cgo, so it never runs the watcher users actually get. PLAN-488: in
# CGO_ENABLED=0 builds the old watcher backend (kqueue) opened every file in the
# workspace, .plumb's SQLite databases included, and dropped their fcntl locks
# whenever a watcher stopped. These tests hold a real lock and probe it from a
# child process.
#
# CI's macOS verify leg runs this on every change, and the release workflow runs
# it again before goreleaser builds anything from a tag. A missing Rosetta fails
# the amd64 run loudly ("bad CPU type in executable"). It never skips.
set -euo pipefail

if [ "$(go env GOOS)" != "darwin" ]; then
	echo "test-watcher-as-released: macOS only (GOOS=$(go env GOOS))" >&2
	exit 1
fi

# The lock tests, by name. go test exits 0 on "[no tests to run]", so a rename
# that drops one out of the -run filter must fail this script, not leave it
# green.
locks="TestWatcher_KeepsSQLiteLocks TestFSWatcher_StopKeepsSQLiteLocksInPlumbDir TestLSPFSWatcher_StopKeepsSQLiteLocksInPlumbDir"

out="$(mktemp)"
trap 'rm -f "$out"' EXIT

# run appends a go test's verbose output to $out, and prints all of it on a
# failure.
run() {
	if ! "$@" >>"$out" 2>&1; then
		cat "$out"
		exit 1
	fi
}

export CGO_ENABLED=0
for arch in arm64 amd64; do
	echo "test-watcher-as-released: darwin/$arch, CGO_ENABLED=0"
	: >"$out"
	run env GOARCH="$arch" go test -count=1 -v ./internal/fswatch/
	run env GOARCH="$arch" go test -count=1 -v -run 'FSWatcher|LSPFSWatcher|LSPWatch' ./internal/topology/ ./internal/cli/
	grep -E '^(ok|FAIL)[[:space:]]' "$out"
	for name in $locks; do
		if ! grep -q -- "^--- PASS: $name " "$out"; then
			echo "test-watcher-as-released: $name did not run on darwin/$arch" >&2
			exit 1
		fi
	done
	echo "test-watcher-as-released: darwin/$arch lock tests passed: $locks"
done
