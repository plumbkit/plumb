# Plumb demos

Small, self-contained scripts that show plumb's behaviour live. Each demo runs
in an isolated throwaway HOME with its own daemon and workspace, never touches
your real plumb state, and cleans up after itself.

## two-agents-one-file.sh

Two MCP sessions ("Agent A" and "Agent B") share one plumb daemon and edit the
same file:

1. Agent A reads `shared.txt` and notes the version (mtime) from the read
   header.
2. Agent B edits the same file; the change applies.
3. Agent A saves its own edit guarded by the version it read in step 1. plumb
   refuses it with a clear "file was modified since you read it" message,
   because applying it could clobber work A never saw.
4. The final file keeps Agent B's change; nothing is lost, and Agent A knows
   exactly why and what to do (re-read, then retry).
5. `workspace_sessions` shows each agent that the other session is active.

The contrast worth noticing: with plain unguarded file writes, step 3 is a
silent overwrite, and whoever saves last wins. plumb turns that silent loss
into an explicit, recoverable refusal. The same behaviour is pinned by the
test suite: `internal/tools/multi_session_test.go` (unit tier, race-detector
clean) and `cmd/smoke/twosession_test.go` (two real `plumb serve` proxies
against one daemon).

### Run it

```sh
./docs/demos/two-agents-one-file.sh
```

Requirements: `bash`, `python3` (stdlib only), and a plumb binary. The script
looks for `$PLUMB_BIN`, then builds from the repo if a Go toolchain is
available, then falls back to `plumb` on PATH. It takes a few seconds; a
hard 3-minute alarm guarantees it can never hang your terminal.

### Expected output (abridged)

```text
== two agents, one file ==

1. Agent A reads shared.txt and notes its version (mtime 2026-…)
2. Agent B edits shared.txt: change applied
3. Agent A saves its edit using the version it read in step 1.
   plumb refuses:
   | edit_file: file "…/shared.txt" was modified since you read it
   |   expected_mtime: …
   |   current mtime:  …
   |   Re-read the file and try again

4. Final content: Agent B's change is intact, nothing lost:
   | alpha (edited by B)
   | beta

5. Agent A checks workspace_sessions: active sessions: 2 (including you)
```

## daemon-respawn.sh

One MCP session works against a plumb daemon. It applies an edit, then the
daemon is killed out from under the live `plumb serve` proxy (a simulated
crash). The agent's very next edit still succeeds — the proxy dials-or-spawns a
fresh daemon and transparently replays the MCP handshake, so the agent never
notices. The recovery is a genuinely new process (its pid changes), and the
first edit is never silently re-applied.

1. One agent session is up against the daemon (over a `plumb serve` proxy).
2. The agent edits `small.txt`; the change applies.
3. The daemon's pid is noted, then stopped with a targeted SIGTERM scoped to
   the demo's isolated HOME — it never touches the user's real plumb daemon.
4. The agent's next edit is the first thing it does after the crash. The first
   attempt(s) may see a transient retriable error while the proxy reconnects;
   once it has, the edit applies — as if the daemon had never died.
5. The recovery daemon's pid differs from the one stopped.
6. The final file shows both edits intact.

The same behaviour is pinned by the test suite: `cmd/smoke/reconnect_test.go`
(`//go:build integration`) kills the daemon mid-session and asserts the next
call succeeds with a changed pid.

### Run it

```sh
./docs/demos/daemon-respawn.sh
```

Requirements: `bash`, `python3` (stdlib only), and a plumb binary. The script
looks for `$PLUMB_BIN`, then builds from the repo if a Go toolchain is
available, then falls back to `plumb` on PATH. It takes a few seconds; a hard
3-minute alarm guarantees it can never hang your terminal.

### Expected output (abridged)

```text
== crash-resilient daemon ==

1. One agent session is up against the plumb daemon (over a `plumb serve` proxy).
2. Agent edits small.txt: change applied

3. The daemon is pid 210462. We stop it (SIGTERM, like `plumb stop`) — the proxy must recover as if it had crashed.
4. The agent's next edit is the first thing it does after the crash.
   The proxy must dial-or-spawn a fresh daemon to serve it…
   edit applied — the agent never noticed the daemon died.

5. Recovery daemon is pid 210484 (was 210462) — a brand-new process.

6. Final file — both edits intact, the first edit was never re-applied:
   | alpha (kept)
   | beta (kept)
```

## Recording a GIF

These scripts are the source for any demo GIF — render one when you need a
visual for the README or the site. The simplest portable path is
[asciinema](https://asciinema.org) + [agg](https://github.com/asciinema/agg):

```sh
asciinema rec --command='./docs/demos/daemon-respawn.sh' demo.cast
mkdir -p docs/assets
agg --rows=24 --font-family="JetBrains Mono" demo.cast docs/assets/daemon-respawn.gif
```

([`vhs`](https://github.com/charmbracelet/vhs) works too, via a `.tape` file
that runs the script.) Drop the GIF under `docs/assets/` and link it from the
README's "See it run" line.
