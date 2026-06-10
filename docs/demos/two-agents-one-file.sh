#!/usr/bin/env bash
# two-agents-one-file.sh — runnable demo of plumb's concurrent-write guardrails.
#
# Two MCP sessions ("Agent A" and "Agent B") share one plumb daemon and one
# workspace. A reads a file and notes its version; B edits the same file; A
# then tries to save an edit guarded by the version it read. plumb refuses the
# stale write with a clear message and B's change survives intact.
#
# Everything runs in an isolated throwaway HOME (own daemon, own socket, own
# config) and cleans up after itself. Requirements: bash, python3, and either
# a plumb binary (PLUMB_BIN or on PATH) or a Go toolchain inside the repo.
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
printf 'alpha\nbeta\n' > "$DEMO_WS/shared.txt"

demo_env() {
  env HOME="$DEMO_HOME" \
    XDG_CONFIG_HOME="$DEMO_HOME/.config" \
    XDG_DATA_HOME="$DEMO_HOME/.local/share" \
    XDG_STATE_HOME="$DEMO_HOME/.local/state" \
    XDG_CACHE_HOME="$DEMO_HOME/.cache" \
    PLUMB_BIN="$PLUMB" DEMO_WS="$DEMO_WS" "$@"
}

cleanup() {
  demo_env "$PLUMB" stop --force >/dev/null 2>&1 || true
  rm -rf "$DEMO_TMP"
}
trap cleanup EXIT

# ── the driver: two persistent `plumb serve` sessions over stdio ────────────
demo_env python3 - <<'PYEOF'
import json, os, signal, subprocess, sys

signal.alarm(180)  # hard backstop so a wedged demo never hangs the terminal

PLUMB = os.environ["PLUMB_BIN"]
WS = os.environ["DEMO_WS"]


class Agent:
    """One MCP session: a `plumb serve` subprocess driven over stdio
    (newline-delimited JSON-RPC 2.0, the MCP stdio transport)."""

    def __init__(self, name):
        self.name = name
        self.next_id = 0
        self.proc = subprocess.Popen(
            [PLUMB, "serve"], stdin=subprocess.PIPE, stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL, text=True, bufsize=1)
        self.request("initialize", {
            "protocolVersion": "2024-11-05",
            "capabilities": {"roots": {"listChanged": True}},
            "clientInfo": {"name": f"demo-agent-{name}", "version": "0.0.1"},
        })
        self.notify("notifications/initialized", {})

    def _write(self, msg):
        self.proc.stdin.write(json.dumps(msg) + "\n")
        self.proc.stdin.flush()

    def notify(self, method, params):
        self._write({"jsonrpc": "2.0", "method": method, "params": params})

    def request(self, method, params):
        self.next_id += 1
        rid = self.next_id
        self._write({"jsonrpc": "2.0", "id": rid, "method": method, "params": params})
        while True:
            line = self.proc.stdout.readline()
            if not line:
                sys.exit(f"agent {self.name}: connection closed mid-request")
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
                    sys.exit(f"agent {self.name}: {method} failed: {msg['error']}")
                return msg["result"]

    def call(self, tool, args):
        """tools/call returning (text, is_error)."""
        result = self.request("tools/call", {"name": tool, "arguments": args})
        text = "".join(c.get("text", "") for c in result.get("content", []))
        return text, bool(result.get("isError"))


def must(agent, tool, args):
    text, is_error = agent.call(tool, args)
    if is_error:
        sys.exit(f"agent {agent.name}: {tool} unexpectedly failed:\n{text}")
    return text


def indent(text, prefix="   | "):
    return "\n".join(prefix + l for l in text.strip().splitlines())


shared = os.path.join(WS, "shared.txt")
print("== two agents, one file ==")
print(f"workspace: {WS}\n")

a, b = Agent("A"), Agent("B")
must(a, "session_start", {"workspace": WS})
must(b, "session_start", {"workspace": WS})
print("two sessions attached to one shared daemon\n")

# 1. A reads the file and notes its version.
read_out = must(a, "read_file", {"file_path": shared})
mtime = next(f.split("=", 1)[1] for f in read_out.split()
             if f.startswith("mtime="))
print(f"1. Agent A reads shared.txt and notes its version (mtime {mtime})")

# 2. B edits the same file.
must(b, "edit_file", {"file_path": shared, "edits": [
    {"old_string": "alpha", "new_string": "alpha (edited by B)"}]})
print("2. Agent B edits shared.txt: change applied")

# 3. A saves an edit guarded by the version it read, now stale.
text, is_error = a.call("edit_file", {
    "file_path": shared, "expected_mtime": mtime,
    "edits": [{"old_string": "beta", "new_string": "beta (edited by A)"}]})
if not is_error:
    sys.exit("expected plumb to refuse the stale edit, but it succeeded")
print("3. Agent A saves its edit using the version it read in step 1.")
print("   plumb refuses:")
print(indent(text))

# 4. Nothing was lost.
final = must(a, "read_file", {"file_path": shared})
body = "\n".join(l.split("\t", 1)[-1] for l in final.splitlines()
                 if not l.startswith("#") and l.strip())
print("\n4. Final content: Agent B's change is intact, nothing lost:")
print(indent(body))

# 5. The sessions can see each other.
sessions = must(a, "workspace_sessions", {})
peer_line = next((l for l in sessions.splitlines() if "active sessions" in l), "")
print(f"\n5. Agent A checks workspace_sessions: {peer_line.strip()}")

print("""
Without the version guard and the daemon's per-path locks, step 3 would have
silently overwritten Agent B's change: whoever saves last wins. plumb turns
that silent loss into an explicit, recoverable refusal, and the same behaviour
is pinned by the test suite (internal/tools/multi_session_test.go and
cmd/smoke/twosession_test.go).""")

for agent in (a, b):
    agent.proc.stdin.close()
    agent.proc.terminate()
PYEOF
