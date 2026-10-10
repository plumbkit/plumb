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

func TestContextHintsOffInEnv(t *testing.T) {
	for v, want := range map[string]bool{"": false, "on": false, "off": true, "Off": true, "0": true, "false": true} {
		if got := contextHintsOffInEnv(func(string) string { return v }); got != want {
			t.Errorf("%q: off = %v, want %v", v, got, want)
		}
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

	if got := claudeContextHintOutput(claudeHookInput{Event: "UserPromptSubmit", CWD: t.TempDir(), SessionID: "c"}, ask); got != "" || len(asked) != 0 {
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
	if _, err := installHooksAt(path, claudeHookEntries("/opt/plumb"), claudeHookOwned); err != nil {
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
	states, err := hookStatesAt(path, claudeHookEntries("/opt/plumb"), claudeHookOwned)
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
	t.Run("off travels", func(t *testing.T) {
		probeTestEnv(t)
		t.Setenv(contextHintsEnv, "off")
		got := make(chan string, 1)
		fakeCtrlDaemon(t, func(c net.Conn, line string) { got <- line; _, _ = c.Write([]byte("ok {\"outcome\":\"noop\"}\n")) })
		askContextHint(contextHintRequest{SessionID: "c"})
		line := <-got
		var req contextHintRequest
		if json.Unmarshal([]byte(strings.TrimPrefix(line, ctrlContextHintCommand)), &req) != nil || !req.Off {
			t.Errorf("request %q did not carry off", line)
		}
	})
}
