#!/usr/bin/env bash
# verify-full-container.sh — run `make verify-full` in a Linux container.
#
# The full gate includes suites that start plumb daemons of their own
# (cmd/smoke, cmd/clientsmoke), and a developer machine must run exactly one
# plumb daemon: those suites refuse to run outside CI, a container or a declared
# VM (internal/separateenv). This is the local way to run the whole gate.
#
# The container gets the repository's history (a git bundle of every ref, tags
# included, which the version and history-reading tests need) checked out at
# HEAD, and over it the working tree as git sees it (tracked and untracked
# files, not ignored ones, minus deleted ones), so uncommitted changes are
# verified and host build output is not. It runs as a non-root user. The container runs with --init (exited processes are
# reaped, not counted) and --rm: its exit ends every process it started.
# While the suite runs, the plumb daemons are counted about every 200 ms (plus
# the time a scan takes): at most one may run at a time, and at least one must
# be seen, since cmd/smoke always starts one. Afterwards none may be left: they
# are looked for for 30 s or more.
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

# No apostrophes inside this single-quoted string. One would end it early and the
# rest of the script would run as shell, one word at a time — the failure reads
# as `step: command not found`, from a comment. Write "the image has" rather
# than a possessive.
inside='
set -uo pipefail
cd /work || exit 70
git -c init.defaultBranch=main clone -q --no-checkout /in/repo.bundle . && git checkout -q --detach "$(cat /in/head)" || exit 70
tar -xf /in/tree.tar || exit 70
while IFS= read -r -d "" f; do rm -f -- "$f"; done < /in/deleted || exit 70

# The plumb daemons alive now, by their own argv; "?" when the process table
# cannot be read. A process that exited mid-scan is skipped, but an empty
# listing is not "none": this shell is a process, so /proc must list one.
# Until exec, a forked child still shows its parent argv, so a daemon starting
# gopls or git briefly looks like a second daemon. The kernel marks that state
# with PF_FORKNOEXEC (0x40) in the stat flags (field 9), so stat is read FIRST
# and a flagged process is skipped; only then is cmdline read, which after exec
# is the process own. A real plumb daemon counts, whatever its parent.
daemons() {
  local n=0 dir stat argv listed=0 rest matched=""
  for dir in /proc/[0-9]*; do
    [ -d "$dir" ] || continue
    listed=1
    if ! stat=$({ cat "$dir/stat"; } 2> /dev/null); then
      [ -d "$dir" ] && { echo "?"; return; }
      continue
    fi
    # "pid (comm) state ppid pgrp session tty tpgid flags ...": comm may hold
    # spaces or ")", so the greedy match ends at the last ") ". A line without
    # that header, or naming another pid, is not trusted.
    [[ "$stat" =~ ^([0-9]+)\ [\(](.*)[\)]\ (.*)$ ]] && [ "${BASH_REMATCH[1]}" = "${dir#/proc/}" ] || { echo "?"; return; }
    rest=${BASH_REMATCH[3]}
    flags=""
    read -r _ _ _ _ _ _ flags _ <<< "$rest"
    [[ "$flags" =~ ^[0-9]+$ ]] || { echo "?"; return; }
    (( flags & 0x40 )) && continue                   # forked, not yet exec-ed
    if ! argv=$({ tr "\0" " " < "$dir/cmdline"; } 2> /dev/null); then
      [ -d "$dir" ] && { echo "?"; return; }
      continue
    fi
    [[ "$argv" =~ ^([^\ ]*/)?plumb\ daemon(\ |$) ]] && { n=$((n + 1)); matched="$matched ${dir#/proc/}"; }
  done
  [ "$listed" -eq 1 ] || { echo "?"; return; }
  echo "$matched" > /tmp/daemons.now 2> /dev/null # diagnostics only
  echo "$n"
}

# The peak during the suite and the number of samples, rewritten atomically to
# /tmp/peak after every sample ("?" sticks once the table could not be read).
supervise() {
  local peak=0 samples=0 n
  while :; do
    n=$(daemons)
    [[ "$n" =~ ^[0-9]+$ ]] || n="?" # anything but a count is an unread table
    samples=$((samples + 1))
    if [ "$n" = "?" ]; then peak="?"; elif [ "$peak" != "?" ] && [ "$n" -gt "$peak" ]; then peak=$n; fi
    # Diagnostics only (never the verdict): who the daemons were when more than
    # one ran, so an overlap can be traced to the tests that caused it.
    if [ "$n" != "?" ] && [ "$n" -gt 1 ] && [ "$({ wc -l < /tmp/overlaps; } 2> /dev/null || echo 0)" -lt 40 ]; then
      for pid in $(cat /tmp/daemons.now 2> /dev/null); do
        echo "$(date +%T.%N) pid=$pid ppid=$(awk "{print \$4}" /proc/$pid/stat 2> /dev/null) home=$(tr "\0" "\n" < /proc/$pid/environ 2> /dev/null | sed -n "s/^HOME=//p") started=$(ps -o lstart= -p $pid 2> /dev/null)" >> /tmp/overlaps
      done
    fi
    # A checkpoint that cannot be written ends the watcher (status 3), which the
    # checks after the suite reject, rather than leave an older record trusted.
    { echo "$peak $samples" > /tmp/peak.new && mv /tmp/peak.new /tmp/peak; } || exit 3
    sleep 0.2
  done
}
rm -f /tmp/peak /tmp/peak.new /tmp/overlaps
supervise &
supervisor=$!

# Every test runs: the build cache volume is reused between runs, and a test
# result replayed from it would prove nothing, least of all about daemons.
GOFLAGS=-count=1 make verify-full
rc=$?
echo "verify-full-container: make verify-full exited $rc"

# The same blind spot the workflow step closes, closed here too: this image ships
# gcc, so every go command above is CGO_ENABLED=1 and the `linux && !cgo` account
# path — the one Linux releases actually execute — is in no build of this gate.
# Its status folds into rc, so a failure fails the run rather than printing
# underneath a green one.
GOFLAGS=-count=1 make test-pure
pure_rc=$?
echo "verify-full-container: make test-pure exited $pure_rc"
[ "$rc" -eq 0 ] && rc=$pure_rc

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
    0) echo "verify-full-container: no plumb daemon was seen during the suite ($samples samples), though cmd/smoke starts one: the watcher saw nothing"; [ "$rc" -eq 0 ] && rc=1 ;;
    1) echo "verify-full-container: at most 1 plumb daemon ran at a time during the suite ($samples samples)" ;;
    "?") echo "verify-full-container: the process table could not be read during the suite"; [ "$rc" -eq 0 ] && rc=1 ;;
    *) echo "verify-full-container: $peak plumb daemons ran at once during the suite (at most 1)"; [ "$rc" -eq 0 ] && rc=1
       sed "s/^/verify-full-container: overlap: /" /tmp/overlaps 2> /dev/null ;;
  esac
fi

seen=0
for _ in $(seq 1 60); do
  n=$(daemons)
  [[ "$n" =~ ^[0-9]+$ ]] || { echo "verify-full-container: cannot read the process table"; exit 1; }
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

in=$(mktemp -d "${TMPDIR:-/tmp}/plumb-verify-in.XXXXXX") || exit 1
trap 'rm -rf "$in"' EXIT
# A signal kills the container but does not exit: the run still ends with the
# engine's answer on whether the container is gone, and then fails.
interrupted=0
trap 'interrupted=1; docker kill "$name" > /dev/null 2>&1' INT TERM HUP
(
  cd "$root" || exit 1
  git bundle create "$in/repo.bundle" --all 2> /dev/null &&
    git rev-parse HEAD > "$in/head" &&
    git ls-files -z --deleted > "$in/deleted" &&
    git ls-files -z --cached --others --exclude-standard |
    while IFS= read -r -d '' f; do { [ -e "$f" ] || [ -L "$f" ]; } && printf '%s\0' "$f"; :; done |
    COPYFILE_DISABLE=1 tar --no-xattrs --null -T - -cf "$in/tree.tar" # no macOS ._* AppleDouble entries or xattr headers
) || { echo "verify-full-container: could not package the repository (git bundle, ls-files or tar failed; see above)" >&2; exit 1; }
chmod -R a+rX "$in"

docker run --rm --init --name "$name" ${platform[@]+"${platform[@]}"} \
  -v "$in:/in:ro" \
  -v plumb-verify-v2-gocache:/home/verify/.cache/go-build \
  -v plumb-verify-v2-gomod:/home/verify/go/pkg/mod \
  -v plumb-verify-v2-lintcache:/home/verify/.cache/golangci-lint \
  "$image" bash -c "$inside" &
child=$! # in the background and waited for, so the trap can run meanwhile
wait "$child"
rc=$?
kill -0 "$child" 2> /dev/null && { wait "$child"; rc=$?; }
[ "$interrupted" -eq 1 ] && { echo "verify-full-container: interrupted" >&2; rc=130; }

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
