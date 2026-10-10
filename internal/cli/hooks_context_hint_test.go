package cli

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// promptSelectors lifts only what a prompt names explicitly: backticked
// selectors and path-shaped tokens. Prose, commands and flags name nothing.
func TestPromptSelectors(t *testing.T) {
	for _, tc := range []struct {
		prompt string
		want   []string
	}{
		{"fix the cart total so it never goes negative", nil},
		{"see `Cart.Total` and `(*Cart).Add`", []string{"Cart.Total", "(*Cart).Add"}},
		{"the bug is in internal/cart/cart.go:45-50, near `pricing.Apply()`", []string{"pricing.Apply", "internal/cart/cart.go"}},
		{"run `go test ./...` with `--count=1`", nil},
		{"`internal/x/y.go` and again internal/x/y.go", []string{"internal/x/y.go"}},
		{"`README`", nil},
	} {
		if got := promptSelectors(tc.prompt); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("promptSelectors(%q) = %q, want %q", tc.prompt, got, tc.want)
		}
	}
	many := make([]string, 0, 20)
	for i := range 20 {
		many = append(many, "`Sym.F"+strings.Repeat("x", i)+"`")
	}
	if got := promptSelectors(strings.Join(many, " ")); len(got) != maxContextHintSelectors {
		t.Errorf("lifted %d selectors, want the cap %d", len(got), maxContextHintSelectors)
	}
}

func TestHintEnvSwitch(t *testing.T) {
	for v, want := range map[string][2]bool{
		"": {false, false}, "maybe": {false, false},
		"on": {true, true}, "1": {true, true}, "True": {true, true},
		"off": {true, false}, "Off": {true, false}, "0": {true, false}, "false": {true, false},
	} {
		set, on := hintEnvSwitch(func(string) string { return v })
		if set != want[0] || on != want[1] {
			t.Errorf("%q: set=%v on=%v, want %v", v, set, on, want)
		}
	}
}

// The hint-only handlers are opt-in: a plain plan neither writes nor lists them,
// --context adds them, and once present a plain plan keeps them (refreshing a
// moved binary) rather than reporting them missing or dropping them.
func TestHookPlan_ContextEntriesAreOptIn(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	target := claudeCodeHooksTarget
	target.pathFn = func() (string, error) { return path, nil }
	events := func(entries []hookEntry) []string {
		out := make([]string, 0, len(entries))
		for _, e := range entries {
			out = append(out, e.event)
		}
		return out
	}
	_, plain, _, err := hookPlan(target, "/opt/plumb", false)
	if err != nil || slices.ContainsFunc(plain, func(e hookEntry) bool { return slices.Contains(contextHintEvents, e.event) }) {
		t.Fatalf("plain plan = %v (%v), want no context-hint entries", events(plain), err)
	}
	_, opted, _, _ := hookPlan(target, "/opt/plumb", true)
	if _, err := installHooksAt(path, opted, target.ours); err != nil {
		t.Fatal(err)
	}
	_, again, states, err := hookPlan(target, "/opt/plumb-moved", false)
	if err != nil || len(again) != len(opted) {
		t.Fatalf("plain plan after opting in = %v (%v), want it to keep %v", events(again), err, events(opted))
	}
	for _, s := range states {
		if slices.Contains(contextHintEvents, s.entry.event) && s.state != hookStateStale {
			t.Errorf("%s state = %q after the binary moved, want stale (to be refreshed)", s.entry.label, s.state)
		}
	}
}

// `plumb hooks` says where context hints stand per client: not installed,
// installed but off, or on (the environment overriding the config).
func TestContextHintStatusNote(t *testing.T) {
	base := make([]hookState, 0, 2)
	base = append(base, hookState{entry: hookEntry{event: "SessionStart"}, state: hookStateInstalled})
	with := append(base, hookState{entry: hookEntry{event: "UserPromptSubmit"}, state: hookStateInstalled})
	t.Setenv(contextHintsEnv, "")
	if got := contextHintStatusNote(claudeCodeHooksTarget, base, func() bool { return true }); !strings.Contains(got, "not installed") {
		t.Errorf("no hint handlers: %q", got)
	}
	if got := contextHintStatusNote(claudeCodeHooksTarget, with, func() bool { return false }); !strings.Contains(got, "installed, off") {
		t.Errorf("installed, config off: %q", got)
	}
	if got := contextHintStatusNote(claudeCodeHooksTarget, with, func() bool { return true }); !strings.HasSuffix(got, ": on.") {
		t.Errorf("installed, config on: %q", got)
	}
	t.Setenv(contextHintsEnv, "on")
	if got := contextHintStatusNote(claudeCodeHooksTarget, with, func() bool { return false }); !strings.HasSuffix(got, ": on.") {
		t.Errorf("installed, env on over config off: %q", got)
	}
	if got := contextHintStatusNote(codexHooksTarget, with, nil); got != "" {
		t.Errorf("a client with no hint handlers got a note: %q", got)
	}
}

// hintWorkspace makes a directory with a .plumb marker and a subdirectory in it.
func hintWorkspace(t *testing.T) (root, sub string) {
	t.Helper()
	root = t.TempDir()
	sub = filepath.Join(root, "pkg")
	for _, d := range []string{filepath.Join(root, ".plumb"), sub} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root, sub
}

// The Claude adapter asks nothing outside a plumb workspace; inside one it sends
// the prompt's selectors (never the prompt), prints the hint as plain text for
// UserPromptSubmit and SessionStart, and as additionalContext JSON for
// SubagentStart; a failed ask prints nothing.
func TestClaudeContextHintOutput(t *testing.T) {
	_, sub := hintWorkspace(t)
	var asked []contextHintRequest
	ask := func(r contextHintRequest) string { asked = append(asked, r); return "HINT\n" }

	// An empty cwd is outside every workspace on every machine. A t.TempDir() is
	// not: with GOTMPDIR inside the checkout (CI, make verify-full) it sits under
	// the repository's own .plumb, and the case silently tests nothing.
	if got := claudeContextHintOutput(claudeHookInput{Event: "UserPromptSubmit", CWD: "", SessionID: "c"}, ask); got != "" || len(asked) != 0 {
		t.Fatalf("outside a workspace: printed %q after %d asks, want nothing and no ask", got, len(asked))
	}

	in := claudeHookInput{Event: "UserPromptSubmit", CWD: sub, SessionID: "c", Prompt: "secret plan for `Cart.Total`"}
	if got := claudeContextHintOutput(in, ask); got != "HINT\n" {
		t.Fatalf("UserPromptSubmit printed %q", got)
	}
	req := asked[0]
	if req.Host != "claude-code" || !reflect.DeepEqual(req.Selectors, []string{"Cart.Total"}) {
		t.Fatalf("request = %+v", req)
	}
	if b, _ := json.Marshal(req); strings.Contains(string(b), "secret plan") {
		t.Fatalf("the prompt text left the hook: %s", b)
	}

	got := claudeContextHintOutput(claudeHookInput{Event: "SubagentStart", CWD: sub, SessionID: "c", AgentID: "a1"}, ask)
	var doc struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if json.Unmarshal([]byte(got), &doc) != nil || doc.HookSpecificOutput.HookEventName != "SubagentStart" || doc.HookSpecificOutput.AdditionalContext != "HINT\n" {
		t.Fatalf("SubagentStart printed %q", got)
	}
	if asked[len(asked)-1].AgentID != "a1" {
		t.Errorf("SubagentStart did not carry the child's agent id: %+v", asked[len(asked)-1])
	}

	if got := claudeContextHintOutput(in, func(contextHintRequest) string { return "" }); got != "" {
		t.Errorf("a failed ask printed %q", got)
	}
}

// `plumb hooks uninstall --only context` removes exactly the context-hint
// handlers: the user's hooks on the same events, and plumb's linkage, mailbox
// and identity hooks, all stay; a second run is a no-op.
func TestUninstallOnlyContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	writeJSONFixture(t, path, map[string]any{"hooks": map[string]any{
		"UserPromptSubmit": []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "mine.sh"}}}},
	}})
	all := append(claudeHookEntries("/opt/plumb"), claudeContextHookEntries("/opt/plumb")...)
	if _, err := installHooksAt(path, all, claudeHookOwned); err != nil {
		t.Fatal(err)
	}
	scope, err := hooksUninstallScope("context")
	if err != nil {
		t.Fatal(err)
	}
	removed, err := removeHooksAt(path, scope.ownership(claudeHookOwned))
	if err != nil || removed != 2 {
		t.Fatalf("removed %d (%v), want the 2 context-hint handlers", removed, err)
	}
	hooks := readHookJSON(t, path)["hooks"].(map[string]any)
	if !hasCommand(hooks, "UserPromptSubmit", "mine.sh") {
		t.Error("the user's UserPromptSubmit hook was removed")
	}
	if _, ok := hooks["SubagentStart"]; ok {
		t.Error("plumb's SubagentStart handler survived")
	}
	states, err := hookStatesAt(path, all, claudeHookOwned)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range states {
		kept := !slices.Contains(contextHintEvents, s.entry.event)
		if kept != (s.state == hookStateInstalled) {
			t.Errorf("%s (%s) state = %q after --only context", s.entry.label, s.entry.event, s.state)
		}
	}
	if again, err := removeHooksAt(path, scope.ownership(claudeHookOwned)); err != nil || again != 0 {
		t.Errorf("second run removed %d (%v), want a no-op", again, err)
	}
	if _, err := hooksUninstallScope("everything"); err == nil {
		t.Error("an unknown --only scope was accepted")
	}
}

// askContextHint round-trips through a daemon's control socket: an emitted hint
// comes back as text; an error line, a non-emitted outcome, an oversized text or
// no daemon at all come back as nothing. The hook's own off switch travels with
// the request.
func TestAskContextHint(t *testing.T) {
	for name, tc := range map[string]struct {
		reply string
		want  string
	}{
		"emitted":   {`ok {"outcome":"emitted","text":"HINT\n"}`, "HINT\n"},
		"noop":      {`ok {"outcome":"noop"}`, ""},
		"error":     {`error: context hint unavailable`, ""},
		"malformed": {`ok {not json`, ""},
		"oversized": {`ok {"outcome":"emitted","text":"` + strings.Repeat("x", contextHintPerTurn+1) + `"}`, ""},
	} {
		t.Run(name, func(t *testing.T) {
			probeTestEnv(t)
			fakeCtrlDaemon(t, func(c net.Conn, _ string) { _, _ = c.Write([]byte(tc.reply + "\n")) })
			if got := askContextHint(contextHintRequest{SessionID: "c", Event: "UserPromptSubmit"}); got != tc.want {
				t.Errorf("askContextHint = %q, want %q", got, tc.want)
			}
		})
	}
	t.Run("no daemon", func(t *testing.T) {
		probeTestEnv(t)
		if got := askContextHint(contextHintRequest{SessionID: "c"}); got != "" {
			t.Errorf("askContextHint = %q with no daemon, want nothing", got)
		}
	})
	for env, want := range map[string][2]bool{"off": {true, false}, "on": {false, true}, "": {false, false}} {
		t.Run("env="+env+" travels", func(t *testing.T) {
			probeTestEnv(t)
			t.Setenv(contextHintsEnv, env)
			got := make(chan string, 1)
			fakeCtrlDaemon(t, func(c net.Conn, line string) { got <- line; _, _ = c.Write([]byte("ok {\"outcome\":\"noop\"}\n")) })
			askContextHint(contextHintRequest{SessionID: "c"})
			line := <-got
			var req contextHintRequest
			if json.Unmarshal([]byte(strings.TrimPrefix(line, ctrlContextHintCommand)), &req) != nil || req.Off != want[0] || req.On != want[1] {
				t.Errorf("request %q: off=%v on=%v, want %v", line, req.Off, req.On, want)
			}
		})
	}
}
