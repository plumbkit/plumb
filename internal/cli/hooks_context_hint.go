package cli

// hooks_context_hint.go — the hook-process half of advisory context hints
// (PLAN-462 Slice B): lifting explicit selectors out of a prompt, and asking the
// daemon for a hint over the control socket.
//
// The prompt itself never leaves this process. Only the tokens a prompt names
// explicitly — a backticked selector, or a path-shaped token — are sent, at
// most maxContextHintSelectors of them, and the daemon re-checks every one
// against the caller's root. Every failure here is silence: no daemon, an older
// daemon, a slow one, a refusal. A hook never strands a turn and never adds a
// byte the agent did not need.

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
)

// contextHintHookBudget is the hook's whole wait, the daemon's own budget plus
// the round trip, well inside the 5 s hook timeout.
const contextHintHookBudget = contextHintBudget + 200*time.Millisecond

var (
	// backtickRe: `Cart.Total`, `(*Cart).Total`, `internal/x/y.go`.
	backtickRe = regexp.MustCompile("`([^`\\s]{1,200})`")
	// pathTokenRe: a bare token with a slash and a file extension.
	pathTokenRe = regexp.MustCompile(`(?:^|[\s(\[{"'])((?:\.{0,2}/)?[A-Za-z0-9_.\-]+(?:/[A-Za-z0-9_.\-]+)+\.[A-Za-z0-9]{1,8})(?::\d+(?:-\d+)?)?`)
	// selectorShapeRe: what a backticked token must look like to be a selector
	// rather than a command, a flag or a value.
	selectorShapeRe = regexp.MustCompile(`^(?:[A-Za-z0-9_.\-]+/)*[A-Za-z0-9_.\-]+\.[A-Za-z0-9]{1,8}$|^\(?\*?[A-Za-z_][A-Za-z0-9_]*\)?(?:\.[A-Za-z_][A-Za-z0-9_]*)+$`)
)

// promptSelectors lifts at most maxContextHintSelectors explicit selectors out
// of a prompt, in order of appearance, without duplicates. A bare word is never
// a selector: "fix the cart total" names nothing.
func promptSelectors(prompt string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		s = strings.TrimRight(s, ".,;:")
		if s == "" || seen[s] || len(out) == maxContextHintSelectors {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	for _, m := range backtickRe.FindAllStringSubmatch(prompt, -1) {
		if tok := strings.TrimSuffix(m[1], "()"); selectorShapeRe.MatchString(tok) {
			add(tok)
		}
	}
	for _, m := range pathTokenRe.FindAllStringSubmatch(prompt, -1) {
		add(m[1])
	}
	return out
}

// claudeContextHintOutput is what Claude Code's hook prints for a context-hint
// event: the hint as plain text where Claude Code adds stdout to the context
// (SessionStart, UserPromptSubmit), as additionalContext where it reads JSON
// (SubagentStart), and "" for every failure. Outside a plumb workspace it asks
// nothing: the hooks are user-scoped and fire for every session on the machine.
func claudeContextHintOutput(input claudeHookInput, ask func(contextHintRequest) string) string {
	if _, inside := plumbWorkspaceRoot(input.CWD); !inside || ask == nil {
		return ""
	}
	req := contextHintRequest{
		Host: "claude-code", Event: input.Event, Source: input.Source,
		SessionID: input.SessionID, AgentID: input.AgentID,
	}
	if input.Event == "UserPromptSubmit" {
		req.Selectors = promptSelectors(input.Prompt)
	}
	text := ask(req)
	if text == "" || input.Event != "SubagentStart" {
		return text
	}
	out, err := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName":     "SubagentStart",
		"additionalContext": text,
	}})
	if err != nil {
		return ""
	}
	return string(out) + "\n"
}

// codexHookOutput is codexHookResult plus context hints. A SessionStart hint
// joins the linkage sentence in the one additionalContext Codex reads; a
// UserPromptSubmit or SubagentStart hint is that event's whole output, and no
// hint is no output. Every failure is silence.
func codexHookOutput(input codexHookInput, probe func(string, string) (mailReport, bool), notify func(string), ask func(contextHintRequest) string) map[string]any {
	switch input.Event {
	case "SessionStart":
		out := codexHookResult(input, probe, notify)
		if out == nil {
			return nil
		}
		if hint := codexContextHintText(input, ask); hint != "" {
			spec, _ := out["hookSpecificOutput"].(map[string]any)
			if linkage, ok := spec["additionalContext"].(string); ok {
				spec["additionalContext"] = linkage + "\n\n" + strings.TrimRight(hint, "\n")
			}
		}
		return out
	case "UserPromptSubmit", "SubagentStart":
		hint := codexContextHintText(input, ask)
		if hint == "" {
			return nil
		}
		return map[string]any{"hookSpecificOutput": map[string]any{
			"hookEventName":     input.Event,
			"additionalContext": strings.TrimRight(hint, "\n"),
		}}
	}
	return codexHookResult(input, probe, notify)
}

// codexContextHintText asks for a Codex event's hint. Codex supplies a real
// turn_id, so the per-turn allowance is keyed on Codex's own turn. Outside a
// plumb workspace it asks nothing, as for Claude Code.
func codexContextHintText(input codexHookInput, ask func(contextHintRequest) string) string {
	if _, inside := plumbWorkspaceRoot(input.CWD); !inside || ask == nil {
		return ""
	}
	req := contextHintRequest{
		Host: "codex", Event: input.Event, Source: input.Source,
		SessionID: input.SessionID, AgentID: input.AgentID, TurnID: input.TurnID,
	}
	if input.Event == "UserPromptSubmit" {
		req.Selectors = promptSelectors(input.Prompt)
	}
	return ask(req)
}

// contextHintEvents are the hook events that exist only for context hints, and
// so are what `plumb hooks uninstall --only context` removes. SessionStart is
// not among them: its handler also links the session.
var contextHintEvents = []string{"UserPromptSubmit", "SubagentStart"}

// uninstallScope narrows an uninstall to some events; nil events is everything.
type uninstallScope struct{ events []string }

func hooksUninstallScope(only string) (uninstallScope, error) {
	switch only {
	case "":
		return uninstallScope{}, nil
	case "context":
		return uninstallScope{events: contextHintEvents}, nil
	}
	return uninstallScope{}, fmt.Errorf("unknown --only %q: supported: context", only)
}

// ownership narrows an ownership test to the scope's events.
func (u uninstallScope) ownership(ours ownershipTest) ownershipTest {
	if u.events == nil {
		return ours
	}
	return func(event string, h map[string]any) bool {
		return slices.Contains(u.events, event) && ours(event, h)
	}
}

// states keeps the status rows the scope covers, so the report lists only what
// this uninstall touched.
func (u uninstallScope) states(all []hookState) []hookState {
	if u.events == nil {
		return all
	}
	var out []hookState
	for _, s := range all {
		if slices.Contains(u.events, s.entry.event) {
			out = append(out, s)
		}
	}
	return out
}

// askContextHint asks the daemon for a hint and returns its text, or "" for
// every outcome but emitted and for every failure.
func askContextHint(req contextHintRequest) string {
	// The hook's own environment is the one a user's shell export reaches; the
	// daemon's may predate it. Either way the request still goes out, so the
	// daemon records the outcome and the switch stays measurable.
	if set, on := hintEnvSwitch(os.Getenv); set {
		req.Off, req.On = !on, on
	}
	if strings.TrimSpace(req.SessionID) == "" {
		return ""
	}
	payload, err := json.Marshal(req)
	if err != nil || len(payload) > maxContextHintRequest {
		return ""
	}
	line, _, err := askDaemonCtrl(ctrlContextHintCommand+string(payload), time.Now().Add(contextHintHookBudget))
	if err != nil {
		return ""
	}
	body, ok := strings.CutPrefix(strings.TrimSpace(line), "ok ")
	var reply contextHintReply
	if !ok || json.Unmarshal([]byte(body), &reply) != nil || reply.Outcome != "emitted" {
		return ""
	}
	if len(reply.Text) > contextHintPerTurn {
		return "" // a daemon that broke its own cap is not trusted with the turn
	}
	return reply.Text
}

// claudeContextHookEntries are Claude Code's opt-in advisory context-hint
// handlers: selectors and locations only, at most 1 KiB a turn,
// silent on any failure. They are installed only on `plumb hooks install
// --context`, or refreshed when already present: an unpromoted feature must not
// spawn a process on every prompt of every user.
func claudeContextHookEntries(plumbBin string) []hookEntry {
	command := plumbHookCommand(plumbBin, claudeHookVerb)
	return []hookEntry{
		{event: "UserPromptSubmit", label: "context hint", handler: map[string]any{
			"type":    "command",
			"command": command,
			"timeout": float64(5),
		}},
		{event: "SubagentStart", label: "subagent context hint", handler: map[string]any{
			"type":    "command",
			"command": command,
			"timeout": float64(5),
		}},
	}
}

// codexContextHookEntries are Codex's opt-in context-hint handlers, on the same
// terms as Claude Code's. They carry no statusMessage: Codex would show it on
// every prompt, and a hint that is usually silent must not announce itself.
// Codex asks the user to trust each new handler (/hooks) before it runs.
func codexContextHookEntries(plumbBin string) []hookEntry {
	command := plumbHookCommand(plumbBin, codexHookVerb)
	return []hookEntry{
		{event: "UserPromptSubmit", label: "context hint", handler: map[string]any{
			"type":    "command",
			"command": command,
			"timeout": float64(5),
		}},
		{event: "SubagentStart", label: "subagent context hint", handler: map[string]any{
			"type":    "command",
			"command": command,
			"timeout": float64(5),
		}},
	}
}
