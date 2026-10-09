#!/usr/bin/env bash
# Run internal/fswatch's inotify watch-limit end-to-end test. It drives a real
# Watcher past fs.inotify.max_user_watches after it has started and expects it
# to report that it can no longer see every change.
#
# The limit is normally far too high to reach in a test, so this lowers it to
# what this user already uses plus 1500, and restores it on exit. Changing it
# needs sudo. CI's Linux verify leg runs this; locally, run it only on a
# machine or VM you can briefly starve of inotify watches.
set -euo pipefail

if [ "$(uname -s)" != "Linux" ]; then
	echo "test-watch-limit: Linux only" >&2
	exit 1
fi

orig="$(cat /proc/sys/fs/inotify/max_user_watches)"
# Every inotify watch this user's processes hold, which is what the limit
# counts. Other users' fdinfo is unreadable and does not count anyway.
used="$( (find /proc/[0-9]*/fdinfo -type f -exec grep -h '^inotify wd' {} + 2>/dev/null || true) | wc -l)"
limit=$((used + 1500))

restore() { sudo sysctl -q -w "fs.inotify.max_user_watches=$orig"; }
trap restore EXIT
sudo sysctl -q -w "fs.inotify.max_user_watches=$limit"
echo "test-watch-limit: max_user_watches $orig -> $limit ($used in use)"

out="$(mktemp)"
trap 'restore; rm -f "$out"' EXIT
if ! PLUMB_FSWATCH_LIMIT_E2E=1 go test -count=1 -v -run '^TestWatcher_WatchLimitAfterStartIsDegraded$' ./internal/fswatch/ >"$out" 2>&1; then
	cat "$out"
	exit 1
fi
# go test exits 0 on a skip, too: require the PASS.
if ! grep -q -- '^--- PASS: TestWatcher_WatchLimitAfterStartIsDegraded ' "$out"; then
	cat "$out"
	echo "test-watch-limit: the test did not run" >&2
	exit 1
fi
grep -E '^(--- PASS|ok)' "$out"
