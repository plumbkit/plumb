package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// Codex's half of `plumb hooks`: the two handlers plumb installs, and the
// runtime behind `plumb hooks run-codex` that they invoke.
//
// Codex has no background-wake mechanism — no async hook can reach a
// conversation with no turn in flight — so the Stop hook narrows the
// end-of-turn race rather than delivering a wake. That difference is stated
// wherever this pack is described; a hook that cannot wake must never be sold
// as one that can.

const (
	codexSessionHookStatus = "Linking Plumb session"
	codexMailboxHookStatus = "Checking Plumb mailbox"
)

// codexHookEntries renders Codex's two handlers. statusMessage is Codex's own
// per-handler progress line; it doubles as the marker the first installer used,
// which codexHookOwned still honours.
func codexHookEntries(plumbBin string) []hookEntry {
	command := plumbHookCommand(plumbBin, codexHookVerb)
	return []hookEntry{
		{event: "SessionStart", label: "session linkage", handler: map[string]any{
			"type":          "command",
			"command":       command,
			"timeout":       float64(5),
			"statusMessage": codexSessionHookStatus,
		}},
		{event: "Stop", label: "mailbox check", handler: map[string]any{
			"type":          "command",
			"command":       command,
			"timeout":       float64(5),
			"statusMessage": codexMailboxHookStatus,
		}},
	}
}

var hooksRunCodexCmd = &cobra.Command{
	Use:         "run-codex",
	Short:       "Run a Codex lifecycle hook",
	Hidden:      true,
	Annotations: map[string]string{annoSkipLogo: "true"}, // stdout belongs to the client
	Args:        cobra.NoArgs,
	RunE:        runCodexHook,
}

type codexHookInput struct {
	SessionID      string `json:"session_id"`
	CWD            string `json:"cwd"`
	Event          string `json:"hook_event_name"`
	StopHookActive bool   `json:"stop_hook_active"`
	// Source is SessionStart's reason. Codex 0.161.0's hook schema lists startup,
	// resume, clear, compact and fork; only clear hands the identity over.
	Source string `json:"source"`
}

func runCodexHook(_ *cobra.Command, _ []string) error {
	return runCodexHookIO(os.Stdin, os.Stdout)
}

func runCodexHookIO(in io.Reader, out io.Writer) error {
	return runCodexHookIOWith(in, out, hookStopMailReport, notifyConversationCleared)
}

// runCodexHookIOWith is runCodexHookIO with the daemon probes injected, so tests
// can drive the real stdin/stdout path without a daemon.
func runCodexHookIOWith(in io.Reader, out io.Writer, probe func(string, string) (mailReport, bool), notify func(string)) error {
	var input codexHookInput
	if err := json.NewDecoder(io.LimitReader(in, 64<<10)).Decode(&input); err != nil {
		return nil // Hook failures must never strand a Codex turn.
	}
	output := codexHookResult(input, probe, notify)
	if output == nil {
		return nil
	}
	return json.NewEncoder(out).Encode(output)
}

// codexHookResult is deliberately pure apart from the supplied probe and notifier,
// so the protocol shape and fail-open recursion guard stay testable without a
// daemon.
//
// On SessionStart begun by /clear it announces the new conversation first, exactly
// as the Claude Code hook does (hooks_claude_clear.go), so the daemon holds the
// marker before the model is told anything it could call with. The announcement
// is best effort and adds nothing to stdout: the linkage document is the same for
// every start reason.
func codexHookResult(input codexHookInput, probe func(string, string) (mailReport, bool), notify func(string)) map[string]any {
	switch input.Event {
	case "SessionStart":
		id := strings.TrimSpace(input.SessionID)
		if id == "" {
			return nil
		}
		if input.Source == sessionSourceClear && notify != nil {
			notify(id)
		}
		return map[string]any{"hookSpecificOutput": map[string]any{
			"hookEventName":     "SessionStart",
			"additionalContext": sessionLinkageSentence(input.SessionID, "Codex conversation"),
		}}
	case "Stop":
		if input.StopHookActive || probe == nil {
			return nil
		}
		report, ok := probe(input.SessionID, input.CWD)
		if !ok || report.Count == 0 {
			return nil
		}
		return map[string]any{
			"decision": "block",
			"reason":   fmt.Sprintf("You have %d unread Plumb message(s) from a peer agent. Call check_messages before finishing.", report.Count),
		}
	default:
		return nil
	}
}
