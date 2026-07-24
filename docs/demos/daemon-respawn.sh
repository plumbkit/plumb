#!/usr/bin/env bash
# daemon-respawn.sh — runnable demo of plumb's crash-resilient daemon.
#
# One MCP session ("Agent") works against a plumb daemon. It applies an edit,
# then the daemon is killed out from under the live `plumb serve` proxy
# (simulating a crash). The agent's very next edit still succeeds: the proxy
# dials-or-spawns a fresh daemon and transparently replays the MCP handshake, so
# the agent never notices the daemon died. That the recovery daemon is genuinely
# new is shown by its pid changing, and the first edit is never re-applied.
#
# Everything runs in an isolated throwaway HOME (own daemon, own socket, own
# config) and cleans up after itself. Requirements: bash, python3, and either a
# plumb binary (PLUMB_BIN or on PATH) or a Go toolchain inside the repo.
set -euo pipefail

# ── locate a plumb binary ────────────────────────────────────────────────────
DEMO_TMP="$(mktemp -d /tmp/plumbdemo.XXXXXX)"
if [[ -n "${PLUMB_BIN:-}" ]]; then
  PLUMB="$PLUMB_BIN"
elif [[ -f "$(git rev-parse --show-toplevel 2>/dev/null)/cmd/plumb/main.go" ]]; then
  echo "building plumb from this repo…"
  PLUMB="$DEMO_TMP/plumb"
  (cd "$(git rev-parse --show-toplevel)" && go build -o "$PLUMB" ./cmd/plumb)
elif command -v plumb >/dev/null 2>&1; then
  PLUMB="$(command -v plumb)"
else
  echo "no plumb binary found: set PLUMB_BIN, put plumb on PATH, or run from the repo" >&2
  exit 1
fi

# ── isolated environment: own HOME/XDG tree, so the demo daemon never touches
#    your real plumb state and the unix-socket path stays short ──────────────
DEMO_HOME="$DEMO_TMP/home"
DEMO_WS="$DEMO_TMP/workspace"
mkdir -p "$DEMO_HOME" "$DEMO_WS/.plumb"
printf 'alpha\nbeta\n' > "$DEMO_WS/small.txt"

demo_env() {
  env HOME="$DEMO_HOME" \
    XDG_CONFIG_HOME="$DEMO_HOME/.config" \
    XDG_DATA_HOME="$DEMO_HOME/.local/share" \
    XDG_STATE_HOME="$DEMO_HOME/.local/state" \
    XDG_CACHE_HOME="$DEMO_HOME/.cache" \
    PLUMB_BIN="$PLUMB" DEMO_WS="$DEMO_WS" "$@"
}

cleanup() {
  # Stop ONLY the demo daemon: targeted SIGTERM of the pid in the isolated
  # cache dir. `plumb stop --force` also runs a system-wide `pgrep -f "plumb
  # daemon"` fallback (internal/cli/stop.go findAllDaemonByArgs) that is not
  # scoped to HOME, so it would stop the user's real plumb daemon too.
  local pidfile pid
  if [[ "$(uname -s)" == "Darwin" ]]; then
    pidfile="$DEMO_HOME/Library/Caches/plumb/plumb.pid"
  else
    pidfile="$DEMO_HOME/.cache/plumb/plumb.pid"
  fi
  if [[ -f "$pidfile" ]]; then
    pid="$(tr -dc '0-9' < "$pidfile" 2>/dev/null || true)"
    if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
      kill "$pid" 2>/dev/null || true
    fi
  fi
  rm -rf "$DEMO_TMP"
}
trap cleanup EXIT

# ── the driver: one persistent `plumb serve` session over stdio ─────────────
demo_env python3 - <<'PYEOF'
import json, os, platform, signal, subprocess, sys, time

signal.alarm(180)  # hard backstop so a wedged demo never hangs the terminal

PLUMB = os.environ["PLUMB_BIN"]
WS = os.environ["DEMO_WS"]


def pid_path():
    # Matches os.UserCacheDir() semantics, like cmd/smoke/reconnect_test.go.
    if platform.system() == "Darwin":
        return os.path.join(os.environ["HOME"], "Library", "Caches", "plumb", "plumb.pid")
    return os.path.join(os.environ["HOME"], ".cache", "plumb", "plumb.pid")


def read_pid():
    try:
        with open(pid_path()) as f:
            return f.read().strip()
    except OSError:
        return ""


def wait_for_pid(unlike=""):
    deadline = time.time() + 15
    while time.time() < deadline:
        pid = read_pid()
        if pid and pid != unlike:
            return pid
        time.sleep(0.2)
    return read_pid()


class Agent:
    """One MCP session: a `plumb serve` subprocess driven over stdio
    (newline-delimited JSON-RPC 2.0, the MCP stdio transport)."""

    def __init__(self):
        self.next_id = 0
        self.proc = subprocess.Popen(
            [PLUMB, "serve"], stdin=subprocess.PIPE, stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL, text=True, bufsize=1)
        self.request("initialize", {
            "protocolVersion": "2024-11-05",
            "capabilities": {"roots": {"listChanged": True}},
            "clientInfo": {"name": "demo-agent", "version": "0.0.1"},
        })
        self.notify("notifications/initialized", {})

    def _write(self, msg):
        self.proc.stdin.write(json.dumps(msg) + "\n")
        self.proc.stdin.flush()

    def notify(self, method, params):
        self._write({"jsonrpc": "2.0", "method": method, "params": params})

    def request(self, method, params):
        """Returns ("ok", result) | ("rpc_error", err) | ("closed", None).

        The non-ok forms let the caller tolerate the transient -32000 the proxy
        emits while it is mid-reconnect, instead of treating it as fatal.
        """
        self.next_id += 1
        rid = self.next_id
        self._write({"jsonrpc": "2.0", "id": rid, "method": method, "params": params})
        while True:
            line = self.proc.stdout.readline()
            if not line:
                return ("closed", None)
            msg = json.loads(line)
            if "method" in msg and "id" in msg:  # server-initiated request
                result = {"roots": [{"uri": "file://" + WS, "name": "demo"}]} \
                    if msg["method"] == "roots/list" else {}
                self._write({"jsonrpc": "2.0", "id": msg["id"], "result": result})
                continue
            if "method" in msg:  # notification: ignore
                continue
            if msg.get("id") == rid:
                if "error" in msg:
                    return ("rpc_error", msg["error"])
                return ("ok", msg["result"])

    def call(self, tool, args):
        """tools/call returning (text, ok). ok is False on an RPC error, a
        closed connection, or a tool-level is_error."""
        status, result = self.request("tools/call", {"name": tool, "arguments": args})
        if status == "ok":
            text = "".join(c.get("text", "") for c in result.get("content", []))
            return text, not bool(result.get("isError"))
        if status == "rpc_error":
            return result.get("message", "rpc error"), False
        return "connection closed", False


def must(agent, tool, args):
    text, ok = agent.call(tool, args)
    if not ok:
        sys.exit(f"agent: {tool} unexpectedly failed:\n{text}")
    return text


def indent(text, prefix="   | "):
    return "\n".join(prefix + l for l in text.strip().splitlines())


def file_body(read_out):
    return "\n".join(l.split("\t", 1)[-1] for l in read_out.splitlines()
                     if not l.startswith("#") and l.strip())


small = os.path.join(WS, "small.txt")
print("== crash-resilient daemon ==")
print(f"workspace: {WS}\n")

a = Agent()
must(a, "session_start", {"workspace": WS})
print("1. One agent session is up against the plumb daemon (over a `plumb serve` proxy).")

# 2. Real work before the crash: an edit that must survive.
must(a, "edit_file", {"file_path": small, "edits": [
    {"old_string": "alpha", "new_string": "alpha (kept)"}]})
print("2. Agent edits small.txt: change applied\n")

# 3. Snapshot the daemon, then kill it — a simulated crash.
pid1 = wait_for_pid()
if not pid1:
    sys.exit("daemon pid file never appeared; is plumb serve spawning a daemon?")
print(f"3. The daemon is pid {pid1}. We stop it (SIGTERM, like `plumb stop`) —")
print("   the proxy must recover as if it had crashed.")
# Kill ONLY the demo daemon: targeted SIGTERM of the pid we just read. Never
# `plumb stop --force` — its system-wide pgrep fallback (stop.go
# findAllDaemonByArgs) is not scoped to HOME and would stop the user's real
# plumb daemon too.
try:
    os.kill(int(pid1), signal.SIGTERM)
except (ProcessLookupError, ValueError):
    pass
time.sleep(1)  # let the proxy notice the dead backend

# 4. The agent's next edit is the first thing it does after the crash. The
#    proxy must dial-or-spawn and replay the handshake. The first attempt(s)
#    may see the synthesised retryable error (-32000) while reconnect is in
#    flight, so we retry.
print("4. The agent's next edit is the first thing it does after the crash.")
print("   The proxy must dial-or-spawn a fresh daemon to serve it…")
deadline = time.time() + 45
text, ok = a.call("edit_file", {"file_path": small, "edits": [
    {"old_string": "beta", "new_string": "beta (kept)"}]})
while not ok and time.time() < deadline:
    time.sleep(0.5)
    text, ok = a.call("edit_file", {"file_path": small, "edits": [
        {"old_string": "beta", "new_string": "beta (kept)"}]})
if not ok:
    sys.exit(f"proxy did not recover: edit still failing after the daemon was killed:\n{text}")
print("   edit applied — the agent never noticed the daemon died.\n")

# 5. Prove it is a NEW daemon: the pid must change.
pid2 = wait_for_pid(unlike=pid1)
if not pid2:
    sys.exit("no daemon pid file after recovery")
if pid1 == pid2:
    sys.exit(f"daemon pid unchanged after kill ({pid1}); expected a respawned daemon")
print(f"5. Recovery daemon is pid {pid2} (was {pid1}) — a brand-new process.")

# 6. Nothing lost, and the first edit was never silently re-run.
final = must(a, "read_file", {"file_path": small})
print("\n6. Final file — both edits intact, the first edit was never re-applied:")
print(indent(file_body(final)))

print("""
When the daemon crashed in step 3, the proxy kept the agent's stdio connection
open, spawned a fresh daemon, and replayed the captured MCP handshake — so the
agent's next edit in step 4 simply succeeded. The same resilience is pinned by the
test suite: cmd/smoke/reconnect_test.go kills the daemon mid-session and asserts
the next call succeeds with a changed pid.""")

a.proc.stdin.close()
a.proc.terminate()
PYEOF
