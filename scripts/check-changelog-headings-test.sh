#!/bin/sh
# check-changelog-headings-test.sh — prove check-changelog-headings.sh rejects a
# replayed '### ' subsection and a repeated entry, and accepts a clean file and
# legacy repeats in released versions.
set -eu

HERE="$(cd "$(dirname "$0")" && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
mkdir "$TMP/scripts"
cp "$HERE/check-changelog-headings.sh" "$TMP/scripts/"

fail=0
run() { # name expected-exit changelog-body
	printf '%s\n' "$3" >"$TMP/CHANGELOG.md"
	rc=0
	"$TMP/scripts/check-changelog-headings.sh" >/dev/null 2>&1 || rc=$?
	if [ "$rc" != "$2" ]; then
		echo "FAIL: $1 (exit $rc, want $2)"
		fail=1
	fi
}

run clean 0 '# Changelog

## 0.2.0 (unreleased)

### Added

- one
- two

### Fixed

- three

## 0.1.0

### Added

- one'

run duplicate-subsection 1 '# Changelog

## 0.2.0 (unreleased)

### Added

- one

### Fixed

- two

### Added

- three'

run duplicate-entry 1 '# Changelog

## 0.2.0 (unreleased)

### Added

- one

### Fixed

- one

### Added

- one'

run released-legacy-repeat-allowed 0 '# Changelog

## 0.2.0 (unreleased)

### Added

- new

## 0.1.0

### Fixed

- a

### Fixed

- a'

[ "$fail" = 0 ] && echo "check-changelog-headings-test: OK"
exit "$fail"
