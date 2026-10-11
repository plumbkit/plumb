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
# While the suite runs, the plumb daemons are counted every 200 ms: at most one
# may run at a time. Afterwards none may be left: they are looked for for 30 s.
# The run fails if either rule breaks, or if the process table cannot be read.
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
# cannot be read. A process that exited mid-scan is skipped, but an empty
# listing is not "none": this shell is a process, so /proc must list one.
daemons() {
  local n=0 dir argv listed=0
  for dir in /proc/[0-9]*; do
    [ -d "$dir" ] || continue
    listed=1
    if ! argv=$(tr "\0" " " < "$dir/cmdline" 2> /dev/null); then
      [ -d "$dir" ] && { echo "?"; return; }
      continue
    fi
    [[ "$argv" =~ ^([^\ ]*/)?plumb\ daemon(\ |$) ]] && n=$((n + 1))
  done
  [ "$listed" -eq 1 ] || { echo "?"; return; }
  echo "$n"
}

# The peak during the suite and the number of samples, rewritten atomically to
# /tmp/peak after every sample ("?" sticks once the table could not be read).
supervise() {
  local peak=0 samples=0 n
  while :; do
    n=$(daemons)
    samples=$((samples + 1))
    if [ "$n" = "?" ]; then peak="?"; elif [ "$peak" != "?" ] && [ "$n" -gt "$peak" ]; then peak=$n; fi
    echo "$peak $samples" > /tmp/peak.new && mv /tmp/peak.new /tmp/peak
    sleep 0.2
  done
}
rm -f /tmp/peak /tmp/peak.new
supervise &
supervisor=$!

make verify-full
rc=$?
echo "verify-full-container: make verify-full exited $rc"

# Trust the peak only from a watcher that ran for the whole suite: alive now,
# stopped by us (TERM, so it exits 143), with a readable, numeric record.
watched=yes
if kill -0 "$supervisor" 2> /dev/null; then
  kill -TERM "$supervisor"
  wait "$supervisor"
  status=$?
  [ "$status" -eq 143 ] || { watched=no; echo "verify-full-container: the daemon watcher exited $status, not 143 on our TERM"; }
else
  watched=no
  echo "verify-full-container: the daemon watcher died during the suite"
fi
peak="?" samples=0
read -r peak samples < /tmp/peak 2> /dev/null
[[ "$samples" =~ ^[0-9]+$ ]] && [ "$samples" -gt 0 ] || { watched=no; echo "verify-full-container: the daemon watcher left no samples"; }
if [ "$watched" = no ]; then
  [ "$rc" -eq 0 ] && rc=1
else
  case "$peak" in
    0) echo "verify-full-container: no plumb daemon was seen during the suite ($samples samples)" ;;
    1) echo "verify-full-container: at most 1 plumb daemon ran at a time during the suite ($samples samples)" ;;
    "?") echo "verify-full-container: the process table could not be read during the suite"; [ "$rc" -eq 0 ] && rc=1 ;;
    *) echo "verify-full-container: $peak plumb daemons ran at once during the suite (at most 1)"; [ "$rc" -eq 0 ] && rc=1 ;;
  esac
fi

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
    while IFS= read -r -d '' f; do { [ -e "$f" ] || [ -L "$f" ]; } && printf '%s\0' "$f"; done |
    tar --null -T - -cf -
) | docker run -i --rm --init --name "$name" ${platform[@]+"${platform[@]}"} \
  -v plumb-verify-gocache:/root/.cache/go-build \
  -v plumb-verify-gomod:/go/pkg/mod \
  -v plumb-verify-lintcache:/root/.cache/golangci-lint \
  "$image" bash -c "$inside"
rc=$?

# Container exit is what ends a SIGKILL'd run's processes, so "gone" must be
# an answer from the engine, never the failure to get one.
if ! listed=$(docker ps -a --filter "name=^/$name\$" --format '{{.Names}}'); then
  echo "verify-full-container: could not ask docker whether container $name is gone" >&2
  rc=1
elif [ -n "$listed" ]; then
  echo "verify-full-container: container $name still exists" >&2
  rc=1
else
  echo "verify-full-container: container $name is gone, so every process it started has ended"
fi
exit "$rc"
