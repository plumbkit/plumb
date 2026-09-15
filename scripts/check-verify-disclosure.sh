#!/usr/bin/env bash
# check-verify-disclosure.sh — pin the contract behind issue #473.
#
# `make verify` compiles the //go:build integration suite (build-integration) but
# does not run it, and the repo documents it as the gate to run before pushing.
# The disclosure in the verify recipe is the whole reason that is honest, so this
# check exists to notice if the disclosure is deleted: a disclosure nothing
# checks is one edit away from the silence #473 is about.
#
# It pins the mechanical halves only, never the wording — the verify recipe must
# name both the suite it skips (integration-test) and the target that runs it
# (verify-full), and verify-full must actually depend on the suite.
set -euo pipefail

root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
makefile="$root/Makefile"
[ -f "$makefile" ] || { printf 'check-verify-disclosure: no Makefile at %s\n' "$makefile" >&2; exit 1; }

fail() { printf 'check-verify-disclosure: %s\n' "$1" >&2; exit 1; }

# The tab-indented recipe lines directly under the `verify:` rule.
recipe=$(awk '
  /^verify:[[:space:]]/ { in_recipe = 1; next }
  in_recipe && /^\t/     { print; next }
  in_recipe              { exit }
' "$makefile")

[ -n "$recipe" ] || fail 'the verify rule has no recipe; the integration-suite disclosure is missing'

printf '%s\n' "$recipe" | grep -q 'integration-test' \
  || fail 'the verify recipe no longer names the suite it skips (make integration-test)'
printf '%s\n' "$recipe" | grep -q 'verify-full' \
  || fail 'the verify recipe no longer points at verify-full, the target that runs the suite'

grep -Eq '^verify-full:.*integration-test' "$makefile" \
  || fail 'verify-full no longer depends on integration-test'

printf 'check-verify-disclosure: ok\n'
