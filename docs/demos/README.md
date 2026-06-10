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
