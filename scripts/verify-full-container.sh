#!/usr/bin/env bash
# verify-full-container.sh — run `make verify-full` in a Linux container.
#
# The full gate includes suites that start plumb daemons of their own
# (cmd/smoke, cmd/clientsmoke), and a developer machine must run exactly one
# plumb daemon: those suites refuse to run outside CI, a container or a declared
# VM (internal/separateenv). This is the local way to run the whole gate.
#
# The working tree is streamed in as git sees it (tracked and untracked files,
# not ignored ones), so uncommitted changes are verified and host build output
# is not. Inside, it becomes a fresh one-commit git repository, which the checks
# that call git need. The container runs with --init (exited processes are
# reaped, not counted) and --rm: its exit ends every process it started.
# Afterwards no plumb daemon may be left: it is looked for for 30 s, and the run
# fails if one is seen or the process table cannot be read.
#
#   make verify-full-container        (DOCKER_PLATFORM=linux/amd64 for amd64)

set -uo pipefail

root=$(git rev-parse --show-toplevel) || exit 1
platform=()
[ -n "${DOCKER_PLATFORM:-}" ] && platform=(--platform "$DOCKER_PLATFORM")
image=plumb-verify

docker build ${platform[@]+"${platform[@]}"} -q -f "$root/build/docker/verify.Dockerfile" -t "$image" "$root/build/docker" > /dev/null || exit 1

name=plumb-verify-$(date +%s)-$$
trap 'docker kill "$name" > /dev/null 2>&1' INT TERM HUP

inside='
set -uo pipefail
mkdir -p /work && tar -C /work -xf - && cd /work || exit 70
git init -q && git add -A && git -c user.name=verify -c user.email=verify@localhost commit -qm "working tree" || exit 70

# The plumb daemons alive now, by their own argv; "?" when the process table
# cannot be read (a process that exited mid-scan is skipped).
daemons() {
  local n=0 dir argv
  for dir in /proc/[0-9]*; do
    if ! argv=$(tr "\0" " " < "$dir/cmdline" 2> /dev/null); then
      [ -d "$dir" ] && { echo "?"; return; }
      continue
    fi
    [[ "$argv" =~ ^([^\ ]*/)?plumb\ daemon(\ |$) ]] && n=$((n + 1))
  done
  echo "$n"
}

make verify-full
rc=$?
echo "verify-full-container: make verify-full exited $rc"

seen=0
for _ in $(seq 1 60); do
  n=$(daemons)
  if [ "$n" = "?" ]; then echo "verify-full-container: cannot read the process table"; exit 1; fi
  [ "$n" -gt "$seen" ] && seen=$n
  sleep 0.5
done
if [ "$seen" -gt 0 ]; then
  echo "verify-full-container: $seen plumb daemon(s) still running after the suite (watched 30 s)"
  [ "$rc" -eq 0 ] && rc=1
else
  echo "verify-full-container: no plumb daemon left after the suite (watched 30 s)"
fi
exit "$rc"
'

(
  cd "$root" || exit 1
  git ls-files -z --cached --others --exclude-standard |
    while IFS= read -r -d '' f; do [ -e "$f" ] && printf '%s\0' "$f"; done |
    tar --null -T - -cf -
) | docker run -i --rm --init --name "$name" ${platform[@]+"${platform[@]}"} \
  -v plumb-verify-gocache:/root/.cache/go-build \
  -v plumb-verify-gomod:/go/pkg/mod \
  -v plumb-verify-lintcache:/root/.cache/golangci-lint \
  "$image" bash -c "$inside"
rc=$?

if docker container inspect "$name" > /dev/null 2>&1; then
  echo "verify-full-container: container $name still exists" >&2
  rc=1
else
  echo "verify-full-container: container $name is gone, so every process it started has ended"
fi
exit "$rc"
