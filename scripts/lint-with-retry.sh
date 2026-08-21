#!/usr/bin/env bash
# Run golangci-lint with a bounded retry on the shared-cache lock. When a peer
# agent's lint is mid-run, golangci-lint reports "parallel golangci-lint is
# running" and exits non-zero — a contention signal, not a lint failure. Retry
# with capped backoff, then fail for real; any other failure is printed unchanged
# and exits with golangci-lint's own status. See the `lint` Makefile target and
# docs/contributing.md#build--verify.
#
# The budget is sized for a QUEUE, not for one peer. A single lint on this repo
# takes ~5s warm or cold (measured), so the previous 5 attempts / 45s looked
# generous — yet it still gave up twice in one session, because this repository
# is worked by several agents at once and the window fills with back-to-back
# peer lints rather than one long one. Losing that race is expensive out of all
# proportion to the wait: lint is the LAST step of `make verify`, so a contention
# failure discards a build and a full test run that already passed.
set -u

attempts=${LINT_RETRY_ATTEMPTS:-12}
max_sleep=${LINT_RETRY_MAX_SLEEP:-15}
rc=1
for ((attempt = 1; attempt <= attempts; attempt++)); do
	out="$(golangci-lint run 2>&1)"
	rc=$?
	printf '%s\n' "$out"
	if [ "$rc" -eq 0 ]; then
		exit 0
	fi
	if ! printf '%s' "$out" | grep -q 'parallel golangci-lint is running'; then
		exit "$rc"
	fi
	delay=$((attempt * 3))
	if [ "$delay" -gt "$max_sleep" ]; then
		delay=$max_sleep
	fi
	echo "lint: golangci-lint lock contended (attempt $attempt/$attempts) — retrying in ${delay}s" >&2
	sleep "$delay"
done
echo "lint: golangci-lint lock still contended after $attempts attempts — failing for real" >&2
echo "lint: raise LINT_RETRY_ATTEMPTS if this repository is busier than usual" >&2
exit "$rc"
