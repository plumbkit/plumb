package cli

import (
	"time"

	"github.com/spf13/cobra"
)

// Qwen Code's half of `plumb hooks`: the handlers plumb installs, and the
// runtime behind `plumb hooks run-qwen` that they invoke.
//
// Qwen Code is a Gemini-CLI fork whose hook contract is Claude Code's: the
// same stdin payload (session_id, cwd, hook_event_name, source, agent_id,
// tool_name, tool_input, stop_hook_active), plain stdout added to the model
// context on SessionStart and UserPromptSubmit, hookSpecificOutput for
// SubagentStart and PreToolUse updatedInput, and exit 2 with stderr as the
// blocking feedback. Verified against Qwen Code 0.28.0's shipped hooks
// documentation and runtime — including that a deferred tool_call invocation
// is re-dispatched under the underlying tool's name and arguments before
// hooks fire, so the identity stamp reaches every plumb call. The runtime is
// therefore Claude's, told which host to attribute context hints to
// (runClaudeShapedHook).
//
// The one difference is the Stop hook's delivery. Qwen has no asyncRewake,
// and an async hook's output is not delivered at all, so the watcher runs
// SYNCHRONOUSLY: it holds the turn open while it polls, and exit 2 blocks the
// stop, feeding the wake sentence to the model. A session that has already
// gone idle cannot be woken — the watch covers the turn's end, not the idle
// after it. The timeout is written in Qwen's notation: a command-hook timeout
// of 1000 or more is read as milliseconds, below 1000 as seconds.

// qwenHooksTarget keeps its hooks in the SAME settings.json as the MCP
// registration (QwenConfigPath), unlike Claude Code's two files — the shared
// merge edits only plumb's own handlers, so the registration and the user's
// other hooks survive.
var qwenHooksTarget = hooksTarget{
	use:            "qwen",
	name:           "Qwen Code",
	setup:          qwenTarget,
	pathFn:         QwenConfigPath,
	entries:        qwenHookEntries,
	contextEntries: qwenContextHookEntries,
	ours:           qwenHookOwned,
	notes: []string{
		"Qwen Code — open the /hooks menu (or restart) to load the handlers; settings.json alone does not arm them in a running session.",
		"Qwen Code — the Stop hook watches synchronously: the turn stays open while it waits for peer mail, and user input interrupts it. Qwen has no asyncRewake, so an already-idle session cannot be woken.",
		"Qwen Code — the Stop timeout is written in milliseconds: Qwen reads a command-hook timeout of 1000 or more as milliseconds, below 1000 as seconds.",
		"Qwen Code — the PreToolUse hook stamps a per-agent identity onto every plumb call, including deferred tool_call invocations, which Qwen re-dispatches under the underlying tool name; set PLUMB_IDENTITY_HOOK=off to disable it.",
		"Qwen Code — consecutive wakes are bounded by Qwen's stopHookBlockingCap (default 8); raise it above plumb's wake-chain cap (10) for long back-and-forths.",
	},
}

// qwenHookOwned matches the qwen verb, and the Claude verb early installs
// reused: Qwen Code's hook payload is byte-compatible with Claude Code's, so
// before this target existed the documented setup ran run-claude from Qwen's
// settings.json. An install refreshes those entries where they stand rather
// than adding a second handler that fires alongside them, and an uninstall
// takes them away with plumb's own.
func qwenHookOwned(event string, h map[string]any) bool {
	cmd, _ := h["command"].(string)
	return runsPlumbVerb(cmd, qwenHookVerb) || runsPlumbVerb(cmd, claudeHookVerb)
}

var hooksRunQwenCmd = &cobra.Command{
	Use:         "run-qwen",
	Short:       "Run a Qwen Code lifecycle hook",
	Hidden:      true,
	Annotations: map[string]string{annoSkipLogo: "true"}, // stdout belongs to the client
	Args:        cobra.NoArgs,
	RunE:        runQwenHook,
}

func runQwenHook(_ *cobra.Command, _ []string) error {
	return runClaudeShapedHook("qwen-code")
}

// qwenHookEntries renders Qwen Code's handlers. They mirror Claude Code's
// except the Stop watcher: no async or asyncRewake (Qwen delivers neither),
// and the timeout in Qwen's millisecond notation.
func qwenHookEntries(plumbBin string) []hookEntry {
	command := plumbHookCommand(plumbBin, qwenHookVerb)
	return []hookEntry{
		{event: "SessionStart", label: "session linkage", handler: map[string]any{
			"type":    "command",
			"command": command,
			"timeout": float64(5),
		}},
		{event: "Stop", label: "mailbox watch", handler: map[string]any{
			"type":    "command",
			"command": command,
			"timeout": qwenHookTimeout(float64((globalWakeWindows().ceiling() + claudeStopTimeoutSlack) / time.Second)),
		}},
		// The identity hook is matched to plumb's own tools, as on Claude Code;
		// see hooks_claude_identity.go for the stamp itself.
		{event: "PreToolUse", label: "agent identity", matcher: claudeIdentityMatcher, handler: map[string]any{
			"type":    "command",
			"command": command,
			"timeout": float64(5),
		}},
	}
}

// qwenHookTimeout renders a seconds budget in the notation Qwen Code reads:
// below 1000 it is seconds, at or above 1000 it is milliseconds. The Stop
// watcher's budget is the wake ceiling plus slack — above the boundary under
// the compiled defaults, but a narrowed window can sit below it, and writing
// every budget in one unit would turn a 90-second watch into 90 milliseconds.
func qwenHookTimeout(seconds float64) float64 {
	if seconds >= 1000 {
		return seconds * 1000
	}
	return seconds
}
