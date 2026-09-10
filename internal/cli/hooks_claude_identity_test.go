package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/mcp"
)

func acceptingDaemon() bool { return true }

func noEnv(string) string { return "" }

func TestClaudeIdentity(t *testing.T) {
	cases := []struct{ session, agent, want string }{
		{"conv-1", "", "conv-1"},
		{"conv-1", "agent-7", "conv-1/agent-7"},
		{" conv-1 ", " agent-7 ", "conv-1/agent-7"},
		{"", "agent-7", ""},
		{"", "", ""},
	}
	for _, tc := range cases {
		if got := claudeIdentity(tc.session, tc.agent); got != tc.want {
			t.Errorf("claudeIdentity(%q, %q) = %q, want %q", tc.session, tc.agent, got, tc.want)
		}
	}
}

// updatedInputOf renders the hook document and decodes updatedInput back to
// raw values, so a test can assert on bytes as the client will send them.
func updatedInputOf(t *testing.T, out map[string]any) (map[string]json.RawMessage, string) {
	t.Helper()
	data, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal hook output: %v", err)
	}
	var doc struct {
		Hook struct {
			Event string          `json:"hookEventName"`
			Input json.RawMessage `json:"updatedInput"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decode hook output %s: %v", data, err)
	}
	if doc.Hook.Event != "PreToolUse" {
		t.Fatalf("hookEventName = %q, want PreToolUse", doc.Hook.Event)
	}
	var input map[string]json.RawMessage
	if err := json.Unmarshal(doc.Hook.Input, &input); err != nil {
		t.Fatalf("updatedInput is not an object: %s", doc.Hook.Input)
	}
	return input, string(doc.Hook.Input)
}

// TestClaudePreToolUse_StampsPlumbTools: every original key is echoed
// byte-for-byte (updatedInput REPLACES the input) and exactly one key is added.
func TestClaudePreToolUse_StampsPlumbTools(t *testing.T) {
	in := claudeHookInput{
		Event: "PreToolUse", SessionID: "conv-1", AgentID: "agent-7",
		ToolName:  "mcp__plumb__edit_file",
		ToolInput: json.RawMessage(`{"file_path":"/w/a.go","edits":[{"old_string":"a","new_string":"b"}],"n":1.10,"await_diagnostics":true}`),
	}
	out, ok := claudePreToolUseOutput(in, noEnv, acceptingDaemon)
	if !ok {
		t.Fatal("a plumb tool call inside a subagent must be stamped")
	}
	input, raw := updatedInputOf(t, out)
	if len(input) != 5 {
		t.Fatalf("updatedInput has %d keys, want the 4 originals plus the stamp: %s", len(input), raw)
	}
	if got := string(input[mcp.ArgLogicalAgentKey]); got != `"conv-1/agent-7"` {
		t.Fatalf("stamp = %s, want \"conv-1/agent-7\"", got)
	}
	for k, want := range map[string]string{
		"file_path": `"/w/a.go"`, "edits": `[{"old_string":"a","new_string":"b"}]`, "n": `1.10`, "await_diagnostics": `true`,
	} {
		if got := string(input[k]); got != want {
			t.Errorf("%s = %s, want %s (bytes must be echoed verbatim)", k, got, want)
		}
	}
	if _, has := input["session_id"]; has {
		t.Error("only session_start gains a session_id")
	}
}

// TestClaudePreToolUse_SessionStartGetsSessionID: the hook owns session_id on
// session_start — added when absent, replaced when the model typed one — so a
// subagent's typed value can never become a phantom third identity.
func TestClaudePreToolUse_SessionStartGetsSessionID(t *testing.T) {
	for _, tc := range []struct{ name, input, agent, wantID string }{
		{"absent input", ``, "", `"conv-1"`},
		{"null input", `null`, "", `"conv-1"`},
		{"empty object", `{}`, "", `"conv-1"`},
		{"typed value replaced", `{"session_id":"subagent-7","workspace":"/w"}`, "agent-7", `"conv-1/agent-7"`},
		{"main thread keeps the conversation id", `{"session_id":"conv-1"}`, "", `"conv-1"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := claudeHookInput{SessionID: "conv-1", AgentID: tc.agent, ToolName: "mcp__plumb__session_start", ToolInput: json.RawMessage(tc.input)}
			out, ok := claudePreToolUseOutput(in, noEnv, acceptingDaemon)
			if !ok {
				t.Fatal("session_start must be stamped")
			}
			input, raw := updatedInputOf(t, out)
			if got := string(input["session_id"]); got != tc.wantID {
				t.Fatalf("session_id = %s, want %s (%s)", got, tc.wantID, raw)
			}
			if got := string(input[mcp.ArgLogicalAgentKey]); got != tc.wantID {
				t.Fatalf("stamp = %s, want %s", got, tc.wantID)
			}
			if strings.Contains(tc.input, "workspace") && string(input["workspace"]) != `"/w"` {
				t.Fatalf("workspace argument lost: %s", raw)
			}
		})
	}
}

// TestClaudePreToolUse_IgnoresNonPlumbTools: the matcher is not trusted.
func TestClaudePreToolUse_IgnoresNonPlumbTools(t *testing.T) {
	for _, name := range []string{"Bash", "mcp__github__create_issue", "mcp__plumbx__read_file", "mcp__plugin_x_plumb__read_file", ""} {
		in := claudeHookInput{SessionID: "conv-1", ToolName: name, ToolInput: json.RawMessage(`{"a":1}`)}
		if _, ok := claudePreToolUseOutput(in, noEnv, acceptingDaemon); ok {
			t.Errorf("%q was stamped; only mcp__plumb__* tools may be", name)
		}
	}
}

// TestClaudePreToolUse_FailsOpen enumerates every branch that must leave the
// call untouched.
func TestClaudePreToolUse_FailsOpen(t *testing.T) {
	base := claudeHookInput{SessionID: "conv-1", ToolName: "mcp__plumb__read_file", ToolInput: json.RawMessage(`{"file_path":"/w/a.go"}`)}
	if _, ok := claudePreToolUseOutput(base, noEnv, acceptingDaemon); !ok {
		t.Fatal("the control case must stamp")
	}
	noSession := base
	noSession.SessionID = "  "
	if _, ok := claudePreToolUseOutput(noSession, noEnv, acceptingDaemon); ok {
		t.Error("no session_id: stamped an identity from nothing")
	}
	for _, bad := range []string{`[1]`, `"s"`, `7`, `{"a":`} {
		in := base
		in.ToolInput = json.RawMessage(bad)
		if _, ok := claudePreToolUseOutput(in, noEnv, acceptingDaemon); ok {
			t.Errorf("non-object tool_input %s was stamped", bad)
		}
	}
	killed := func(name string) string {
		if name == claudeIdentityKillSwitch {
			return "OFF"
		}
		return ""
	}
	if _, ok := claudePreToolUseOutput(base, killed, acceptingDaemon); ok {
		t.Error("kill switch ignored")
	}
	if _, ok := claudePreToolUseOutput(base, noEnv, func() bool { return false }); ok {
		t.Error("a daemon that predates the channel must not be stamped")
	}
	if _, ok := claudePreToolUseOutput(base, noEnv, nil); ok {
		t.Error("no daemon check must read as no daemon")
	}
}

// TestDaemonAcceptsIdentityStamp covers the cache and threshold logic without
// a daemon: a fresh cache answers alone, a stale one re-probes and rewrites, a
// failing probe is "no", and the threshold reads releases as releases and a
// dev build as current.
func TestDaemonAcceptsIdentityStamp(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "probe", identityProbeCacheFile)
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	probes := 0
	probe := func() (string, error) { probes++; return "0.19.1", nil }

	if !daemonAcceptsIdentityStamp(probe, cache, now) || probes != 1 {
		t.Fatalf("first call must probe and accept: probes=%d", probes)
	}
	if !daemonAcceptsIdentityStamp(probe, cache, now.Add(30*time.Second)) || probes != 1 {
		t.Fatalf("a fresh cache must answer without probing: probes=%d", probes)
	}
	if !daemonAcceptsIdentityStamp(probe, cache, now.Add(2*time.Minute)) || probes != 2 {
		t.Fatalf("a stale cache must re-probe: probes=%d", probes)
	}
	old := func() (string, error) { return "0.19.0", nil }
	if daemonAcceptsIdentityStamp(old, cache, now.Add(4*time.Minute)) {
		t.Fatal("a 0.19.0 daemon predates the channel and must not be stamped")
	}
	if daemonAcceptsIdentityStamp(old, cache, now.Add(4*time.Minute+10*time.Second)) {
		t.Fatal("the cached old version must keep refusing")
	}
	failing := func() (string, error) { return "", errors.New("no daemon") }
	if daemonAcceptsIdentityStamp(failing, cache, now.Add(10*time.Minute)) {
		t.Fatal("a failing probe must read as no")
	}
	// A cache stamped in the future (clock stepped back, a copied home) is not
	// fresh: it must re-probe rather than trust a record from "later".
	writeIdentityProbe(cache, identityProbeRecord{DaemonVersion: "0.19.1", CheckedAt: now.Add(time.Hour)})
	probes = 0
	if daemonAcceptsIdentityStamp(func() (string, error) { probes++; return "0.19.0", nil }, cache, now) || probes != 1 {
		t.Fatalf("a future-stamped cache must be re-probed: probes=%d", probes)
	}
	if daemonAcceptsIdentityStamp(nil, filepath.Join(t.TempDir(), "none.json"), now) {
		t.Fatal("no probe and no cache must read as no")
	}
	for v, want := range map[string]bool{"0.19.1": true, "v0.20.0": true, "1.0.0": true, "0.19.0": false, "0.18.9": false, "dev": true, "": false} {
		if got := daemonVersionAcceptsStamp(v); got != want {
			t.Errorf("daemonVersionAcceptsStamp(%q) = %v, want %v", v, got, want)
		}
	}
}

// TestIdentityHookSkewNote: the status table says WHY an installed identity
// hook is stamping nothing, and tells a stopped daemon from an old one — the
// remedies differ.
func TestIdentityHookSkewNote(t *testing.T) {
	installed := []hookState{{entry: hookEntry{event: "PreToolUse"}, state: hookStateInstalled}}
	missing := []hookState{{entry: hookEntry{event: "PreToolUse"}, state: hookStateMissing}}
	current := func() (string, error) { return "0.19.1", nil }
	old := func() (string, error) { return "0.19.0", nil }
	down := func() (string, error) { return "", errDaemonNotRunning }
	mute := func() (string, error) { return "", errDaemonVersionUnknown }

	if got := identityHookSkewNote(claudeCodeHooksTarget, installed, current); got != "" {
		t.Errorf("a current daemon needs no note, got %q", got)
	}
	if got := identityHookSkewNote(claudeCodeHooksTarget, missing, old); got != "" {
		t.Errorf("a hook that is not installed needs no note, got %q", got)
	}
	if got := identityHookSkewNote(codexHooksTarget, installed, old); got != "" {
		t.Errorf("only the Claude Code target carries the identity hook, got %q", got)
	}
	if got := identityHookSkewNote(claudeCodeHooksTarget, installed, old); !strings.Contains(got, "0.19.0") || !strings.Contains(got, "plumb restart") {
		t.Errorf("an old daemon must be named with the restart remedy, got %q", got)
	}
	if got := identityHookSkewNote(claudeCodeHooksTarget, installed, mute); !strings.Contains(got, "predates") || !strings.Contains(got, "plumb restart") {
		t.Errorf("a daemon that cannot answer predates the channel, got %q", got)
	}
	if got := identityHookSkewNote(claudeCodeHooksTarget, installed, down); !strings.Contains(got, "no daemon is running") || strings.Contains(got, "restart") {
		t.Errorf("a stopped daemon is not a skew and needs no restart, got %q", got)
	}
	if got := identityHookSkewNote(claudeCodeHooksTarget, installed, nil); got != "" {
		t.Errorf("no probe, no note; got %q", got)
	}
}

func TestParseDaemonVersionReply(t *testing.T) {
	if v, err := parseDaemonVersionReply("ok 0.19.1\n"); err != nil || v != "0.19.1" {
		t.Fatalf("ok line: %q, %v", v, err)
	}
	for _, line := range []string{`error: unknown command "version"`, "ok", "ok ", "", "0.19.1"} {
		if _, err := parseDaemonVersionReply(line); err == nil {
			t.Errorf("%q must not parse as a version", line)
		}
	}
}

// TestRunClaudeHook_PreToolUseAcceptsLargeInput drives the real command with a
// one-megabyte write_file body: the old 64 KiB stdin cap would have failed the
// decode and silently un-stamped exactly the call most likely to be refused.
func TestRunClaudeHook_PreToolUseAcceptsLargeInput(t *testing.T) {
	t.Setenv("PLUMB_WAKE_DIR", t.TempDir())
	body := strings.Repeat("x", 1<<20)
	payload, _ := json.Marshal(map[string]any{
		"hook_event_name": "PreToolUse", "session_id": "conv-1", "tool_name": "mcp__plumb__write_file",
		"tool_input": map[string]any{"file_path": "/w/big.txt", "content": body},
	})
	out := runHookForTest(t, payload)
	if !bytes.Contains(out, []byte(mcp.ArgLogicalAgentKey)) {
		t.Fatalf("a large PreToolUse payload was not stamped (stdout %d bytes)", len(out))
	}
	if !bytes.Contains(out, []byte(body[:64])) {
		t.Fatal("the content was not echoed back")
	}
}

// runHookForTest runs runClaudeHook with stdin and stdout swapped for pipes,
// with the daemon probe answering "current" through a pre-seeded cache.
func runHookForTest(t *testing.T, stdin []byte) []byte {
	t.Helper()
	writeIdentityProbe(filepath.Join(wakeDir(), identityProbeCacheFile), identityProbeRecord{DaemonVersion: "dev", CheckedAt: time.Now()})
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldIn, oldOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = inR, outW
	t.Cleanup(func() { os.Stdin, os.Stdout = oldIn, oldOut })
	go func() { _, _ = inW.Write(stdin); inW.Close() }()
	done := make(chan []byte, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(outR)
		done <- buf.Bytes()
	}()
	if err := runClaudeHook(nil, nil); err != nil {
		t.Fatalf("runClaudeHook: %v", err)
	}
	outW.Close()
	os.Stdin, os.Stdout = oldIn, oldOut
	return <-done
}

// TestClaudeHookEntries_IdentityHookIsMatched pins the entry's shape: matched
// to plumb's tools, a short numeric timeout, and synchronous — an async hook's
// output is discarded, which would silently stamp nothing.
func TestClaudeHookEntries_IdentityHookIsMatched(t *testing.T) {
	for _, e := range claudeHookEntries("/opt/plumb") {
		if e.event != "PreToolUse" {
			if e.matcher != "" {
				t.Errorf("%s carries a matcher %q; only PreToolUse may", e.event, e.matcher)
			}
			continue
		}
		if e.matcher != "mcp__plumb__.*" {
			t.Errorf("PreToolUse matcher = %q, want mcp__plumb__.*", e.matcher)
		}
		if timeout, ok := e.handler["timeout"].(float64); !ok || timeout <= 0 || timeout > 10 {
			t.Errorf("PreToolUse timeout = %v, want a short number", e.handler["timeout"])
		}
		if _, async := e.handler["async"]; async {
			t.Error("the identity hook must be synchronous: an async hook's stdout is discarded")
		}
		if !claudeHookOwned("PreToolUse", e.handler) {
			t.Error("the identity handler is not recognised as plumb's own")
		}
		return
	}
	t.Fatal("no PreToolUse entry in the Claude Code pack")
}

// TestInstallHooksAt_AddsIdentityHookToAnExistingInstall: an install written
// by a plumb without the identity hook gains exactly one matcher group; the
// user's own PreToolUse group and the two older plumb entries are untouched;
// a re-run is a no-op; an uninstall removes all three and only plumb's group.
func TestInstallHooksAt_AddsIdentityHookToAnExistingInstall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	old := claudeHookEntries("/opt/plumb")[:2]
	writeJSONFixture(t, path, map[string]any{
		"hooks": map[string]any{
			"PreToolUse": []any{map[string]any{
				"matcher": "Bash",
				"hooks":   []any{map[string]any{"type": "command", "command": "audit.sh"}},
			}},
		},
	})
	if _, err := installHooksAt(path, old, claudeHookOwned); err != nil {
		t.Fatal(err)
	}
	before := readHookJSON(t, path)

	changed, err := installHooksAt(path, claudeHookEntries("/opt/plumb"), claudeHookOwned)
	if err != nil || !changed {
		t.Fatalf("install = (%v, %v), want a change", changed, err)
	}
	got := readHookJSON(t, path)
	hooks := got["hooks"].(map[string]any)
	groups := hooks["PreToolUse"].([]any)
	if len(groups) != 2 {
		t.Fatalf("PreToolUse groups = %d, want the user's plus plumb's: %v", len(groups), groups)
	}
	user := groups[0].(map[string]any)
	if user["matcher"] != "Bash" || !hasCommand(hooks, "PreToolUse", "audit.sh") {
		t.Errorf("user's PreToolUse group changed: %v", user)
	}
	plumbs := groups[1].(map[string]any)
	if plumbs["matcher"] != claudeIdentityMatcher || len(plumbs) != 2 {
		t.Errorf("plumb's group = %v, want {matcher, hooks}", plumbs)
	}
	for _, event := range []string{"SessionStart", "Stop"} {
		beforeHooks := before["hooks"].(map[string]any)
		if !jsonEqual(beforeHooks[event], hooks[event]) {
			t.Errorf("%s changed when only PreToolUse should have: %v → %v", event, beforeHooks[event], hooks[event])
		}
	}

	changed, err = installHooksAt(path, claudeHookEntries("/opt/plumb"), claudeHookOwned)
	if err != nil || changed {
		t.Fatalf("re-run = (%v, %v), want a no-op", changed, err)
	}

	removed, err := removeHooksAt(path, claudeHookOwned)
	if err != nil || removed != 3 {
		t.Fatalf("uninstall removed %d (%v), want 3", removed, err)
	}
	hooks = readHookJSON(t, path)["hooks"].(map[string]any)
	groups = hooks["PreToolUse"].([]any)
	if len(groups) != 1 || groups[0].(map[string]any)["matcher"] != "Bash" {
		t.Errorf("uninstall left PreToolUse = %v, want only the user's group", groups)
	}
}

// TestRemoveHooksAt_KeepsAUsersPlumbMatcherGroup: a user's own handler living
// in a group that happens to carry plumb's matcher survives with its group.
func TestRemoveHooksAt_KeepsAUsersPlumbMatcherGroup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	writeJSONFixture(t, path, map[string]any{
		"hooks": map[string]any{
			"PreToolUse": []any{map[string]any{
				"matcher": claudeIdentityMatcher,
				"hooks": []any{
					map[string]any{"type": "command", "command": "mine.sh"},
					map[string]any{"type": "command", "command": plumbHookCommand("/opt/plumb", claudeHookVerb)},
				},
			}},
		},
	})
	if removed, err := removeHooksAt(path, claudeHookOwned); err != nil || removed != 1 {
		t.Fatalf("removed %d (%v), want 1", removed, err)
	}
	hooks := readHookJSON(t, path)["hooks"].(map[string]any)
	groups := hooks["PreToolUse"].([]any)
	if len(groups) != 1 || !hasCommand(hooks, "PreToolUse", "mine.sh") {
		t.Fatalf("the user's handler or its group went: %v", groups)
	}
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}
