#!/usr/bin/env bash
# Run golangci-lint with a bounded retry on the shared-cache lock. When a peer
# agent's lint is mid-run, golangci-lint reports "parallel golangci-lint is
# running" and exits non-zero — a contention signal, not a lint failure. Retry a
# handful of times with backoff, then fail for real; any other failure is printed
# unchanged and exits with golangci-lint's own status. See the `lint` Makefile
# target and docs/contributing.md#build--verify.
set -u

attempts=${LINT_RETRY_ATTEMPTS:-5}
rc=1
for ((attempt = 1; attempt <= attempts; attempt++)); do
	out="$(golangci-lint run 2>&1)"
	rc=$?
	printf '%s
' "$out"
	if [ "$rc" -eq 0 ]; then
		exit 0
	fi
	if ! printf '%s' "$out" | grep -q 'parallel golangci-lint is running'; then
		exit "$rc"
	fi
	echo "lint: golangci-lint lock contended (attempt $attempt/$attempts) — retrying in $((attempt * 3))s" >&2
	sleep "$((attempt * 3))"
done
echo "lint: golangci-lint lock still contended after $attempts attempts — failing for real" >&2
exit "$rc"
