package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestQwenHookTimeout_Notation pins Qwen Code's split reading of a command-hook
// timeout: below 1000 it is seconds, at or above 1000 it is milliseconds. The
// Stop watcher's budget crosses that boundary under the compiled defaults, and
// a narrowed window can sit below it — writing every budget in one unit would
// turn a 90-second watch into 90 milliseconds, or a 3630-second one into 3.6.
func TestQwenHookTimeout_Notation(t *testing.T) {
	for _, tc := range []struct {
		seconds float64
		want    float64
	}{
		{5, 5},
		{330, 330},
		{999, 999},
		{1000, 1000000},
		{3630, 3630000},
		{9030, 9030000},
	} {
		if got := qwenHookTimeout(tc.seconds); got != tc.want {
			t.Errorf("qwenHookTimeout(%v) = %v, want %v", tc.seconds, got, tc.want)
		}
	}
}

// TestQwenHookEntries_StopIsSynchronous pins the one delivery difference from
// Claude Code: Qwen delivers no async hook output and has no asyncRewake, so
// the watcher must hold the turn synchronously, under a timeout that covers
// the wake ceiling in Qwen's notation.
func TestQwenHookEntries_StopIsSynchronous(t *testing.T) {
	for _, e := range qwenHookEntries("/opt/plumb") {
		if e.event != "Stop" {
			continue
		}
		if _, ok := e.handler["async"]; ok {
			t.Error("the Qwen Stop watcher must not be async — its output would never be delivered")
		}
		if _, ok := e.handler["asyncRewake"]; ok {
			t.Error("Qwen has no asyncRewake; the watcher blocks the stop synchronously")
		}
		want := qwenHookTimeout(float64((globalWakeWindows().ceiling() + claudeStopTimeoutSlack) / time.Second))
		if e.handler["timeout"] != want {
			t.Errorf("Stop timeout = %v, want %v (the wake ceiling plus slack, in Qwen's notation)", e.handler["timeout"], want)
		}
		return
	}
	t.Fatal("no Stop entry in the Qwen Code pack")
}

// TestQwenHookOwned pins the ownership test: the qwen verb, the Claude verb
// early installs reused (the payloads are byte-compatible), and nothing else.
func TestQwenHookOwned(t *testing.T) {
	for _, tc := range []struct {
		name, command string
		want          bool
	}{
		{"qwen verb", `"/opt/plumb" hooks run-qwen`, true},
		{"claude verb an early install reused", `"/opt/plumb" hooks run-claude`, true},
		{"moved binary", `"/moved/plumb" hooks run-qwen`, true},
		{"user hook", "mine.sh", false},
		{"verb mentioned in an argument", "/home/u/bin/logger --note 'hooks run-qwen'", false},
		{"another client's verb", `"/opt/plumb" hooks run-codex`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := qwenHookOwned("Stop", map[string]any{"command": tc.command}); got != tc.want {
				t.Errorf("qwenHookOwned(%q) = %v, want %v", tc.command, got, tc.want)
			}
		})
	}
}

// TestInstallHooksAt_QwenMigratesRunClaude is the migration path for the
// hand-installed entries that predate this target: they are plumb's, so an
// install refreshes them where they stand rather than adding a second handler
// that fires alongside them.
func TestInstallHooksAt_QwenMigratesRunClaude(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	writeJSONFixture(t, path, map[string]any{
		"mcpServers": map[string]any{"plumb": map[string]any{"command": "/opt/plumb", "args": []any{"serve"}}},
		"hooks": map[string]any{
			"SessionStart": []any{
				map[string]any{"matcher": "startup", "hooks": []any{map[string]any{"type": "command", "command": "mine.sh"}}},
				map[string]any{"hooks": []any{map[string]any{
					"type": "command", "command": `"/opt/plumb" hooks run-claude`, "timeout": float64(5),
				}}},
			},
			"Stop": []any{map[string]any{"hooks": []any{map[string]any{
				"type": "command", "command": `"/opt/plumb" hooks run-claude`, "timeout": float64(9030000),
			}}}},
		},
	})

	entries := append(qwenHookEntries("/opt/plumb"), qwenContextHookEntries("/opt/plumb")...)
	if _, err := installHooksAt(path, entries, qwenHookOwned); err != nil {
		t.Fatal(err)
	}

	got := readHookJSON(t, path)
	if _, ok := got["mcpServers"]; !ok {
		t.Error("the MCP registration in the same file was lost")
	}
	hooks := got["hooks"].(map[string]any)
	for _, event := range []string{"SessionStart", "Stop", "PreToolUse", "UserPromptSubmit", "SubagentStart"} {
		groups, ok := hooks[event].([]any)
		if !ok {
			t.Fatalf("%s missing after install", event)
		}
		n := 0
		for _, g := range groups {
			n += len(g.(map[string]any)["hooks"].([]any))
		}
		want := 1
		if event == "SessionStart" {
			want = 2 // the user's own hook survives alongside plumb's
		}
		if n != want {
			t.Errorf("%s: %d handlers, want %d — the legacy entry must be refreshed, not doubled", event, n, want)
		}
	}
	if !hasCommand(hooks, "SessionStart", "mine.sh") {
		t.Error("the user's own SessionStart hook did not survive")
	}
	if !hasCommand(hooks, "Stop", `"/opt/plumb" hooks run-qwen`) {
		t.Error("the legacy Stop entry was not refreshed to the qwen verb")
	}

	// A re-run over the migrated file is a no-op.
	if changed, err := installHooksAt(path, entries, qwenHookOwned); err != nil || changed {
		t.Errorf("re-install: changed=%v err=%v, want a no-op", changed, err)
	}
}

// TestRemoveHooksAt_QwenRemovesBothVerbs: an uninstall takes the early
// run-claude entries away with plumb's own, and only those.
func TestRemoveHooksAt_QwenRemovesBothVerbs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	writeJSONFixture(t, path, map[string]any{
		"hooks": map[string]any{
			"SessionStart": []any{map[string]any{"hooks": []any{map[string]any{
				"type": "command", "command": `"/opt/plumb" hooks run-claude`, "timeout": float64(5),
			}}}},
			"Stop": []any{map[string]any{"hooks": []any{map[string]any{
				"type": "command", "command": `"/opt/plumb" hooks run-qwen`, "timeout": float64(9030000),
			}}}},
			"PreToolUse": []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "audit.sh"}}}},
		},
	})
	removed, err := removeHooksAt(path, qwenHookOwned)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Errorf("removed = %d, want 2 (the run-claude linkage and the run-qwen watch)", removed)
	}
	hooks := readHookJSON(t, path)["hooks"].(map[string]any)
	if _, ok := hooks["SessionStart"]; ok {
		t.Error("SessionStart survived with nothing in it")
	}
	if _, ok := hooks["Stop"]; ok {
		t.Error("Stop survived with nothing in it")
	}
	if !hasCommand(hooks, "PreToolUse", "audit.sh") {
		t.Error("the user's own PreToolUse hook did not survive")
	}
}

// TestQwenContextHint_HostAttribution: the shared render attributes the hint to
// the host it is told, so Qwen's uptake is not counted as Claude Code's.
func TestQwenContextHint_HostAttribution(t *testing.T) {
	_, sub := hintWorkspace(t)
	var asked []contextHintRequest
	ask := func(r contextHintRequest) string { asked = append(asked, r); return "HINT\n" }
	in := claudeHookInput{Event: "UserPromptSubmit", CWD: sub, SessionID: "c", Prompt: "fix `Cart.Total`"}
	if got := claudeShapedContextHintOutput(in, "qwen-code", ask); got != "HINT\n" {
		t.Fatalf("printed %q, want the hint as plain text", got)
	}
	if len(asked) != 1 || asked[0].Host != "qwen-code" {
		t.Fatalf("asked = %+v, want one request attributed to qwen-code", asked)
	}
}

// TestRunQwenHook_SessionStartLinkage drives the verb itself: the Claude-shaped
// runtime decodes Qwen's payload and states the linkage on stdout, which Qwen
// adds to the model context. The runtime directory is private, so the context
// hint's daemon probe fails open instead of reaching a real daemon.
func TestRunQwenHook_SessionStartLinkage(t *testing.T) {
	probeTestEnv(t)
	_, sub := hintWorkspace(t)
	stdin := []byte(`{"session_id":"qwen-test-conv","cwd":"` + sub + `","hook_event_name":"SessionStart","source":"startup"}`)

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
	if err := runQwenHook(nil, nil); err != nil {
		t.Fatalf("runQwenHook: %v", err)
	}
	outW.Close()
	os.Stdin, os.Stdout = oldIn, oldOut
	got := string(<-done)
	if !strings.Contains(got, `Plumb session linkage: this conversation has id "qwen-test-conv"`) {
		t.Errorf("stdout = %q, want the linkage sentence", got)
	}
}
