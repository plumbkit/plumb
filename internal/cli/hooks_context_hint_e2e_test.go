package cli

import (
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// `plumb hooks uninstall <client> --only context`, run end to end, removes the
// two hint handlers and nothing else: a user's own hook on EVERY event plumb
// touches survives, and plumb's linkage, mailbox and identity handlers stay
// (cross-review S4).
func TestRunHooksUninstall_OnlyContextKeepsEveryOtherHook(t *testing.T) {
	for _, target := range []hooksTarget{claudeCodeHooksTarget, codexHooksTarget} {
		t.Run(target.use, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("CODEX_HOME", "")
			path, err := target.pathFn()
			if err != nil {
				t.Fatal(err)
			}
			all := append(target.entries("/opt/plumb"), target.contextEntries("/opt/plumb")...)
			var events []string
			userHooks := map[string]any{}
			for _, e := range all {
				if !slices.Contains(events, e.event) {
					events = append(events, e.event)
					userHooks[e.event] = []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "mine-" + e.event + ".sh"}}}}
				}
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			writeJSONFixture(t, path, map[string]any{"hooks": userHooks})
			if _, err := installHooksAt(path, all, target.ours); err != nil {
				t.Fatal(err)
			}

			prev := hooksUninstallOnly
			hooksUninstallOnly = "context"
			t.Cleanup(func() { hooksUninstallOnly = prev })
			captureStdout(t, func() {
				if err := runHooksUninstall([]string{target.use}); err != nil {
					t.Errorf("uninstall: %v", err)
				}
			})

			hooks := readHookJSON(t, path)["hooks"].(map[string]any)
			for _, ev := range events {
				if !hasCommand(hooks, ev, "mine-"+ev+".sh") {
					t.Errorf("the user's %s hook was removed", ev)
				}
			}
			states, err := hookStatesAt(path, all, target.ours)
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range states {
				kept := !slices.Contains(contextHintEvents, s.entry.event)
				if kept != (s.state == hookStateInstalled) {
					t.Errorf("%s (%s) state = %q after --only context", s.entry.label, s.entry.event, s.state)
				}
			}
		})
	}
}

// With hints on, inside a workspace, and a daemon that accepts and never
// answers, both clients' real entry points print nothing for a prompt and stay
// inside the hook's own budget (cross-review S4: the outage path through the
// real asker, not a stub).
func TestContextHint_WedgedDaemonIsSilentAndBounded(t *testing.T) {
	probeTestEnv(t)
	hintsConfigured(t, true)
	_, sub := hintWorkspace(t)
	release := make(chan struct{})
	asked := make(chan string, 4)
	fakeCtrlDaemon(t, func(_ net.Conn, line string) { asked <- line; <-release })
	t.Cleanup(func() { close(release) }) // runs before the listener's own cleanup
	limit := contextHintHookBudget + 500*time.Millisecond
	// Each entry point must actually reach the wedged daemon: silence for any
	// other reason (hints off, outside the workspace) would test nothing.
	reached := func(who string) {
		t.Helper()
		select {
		case <-asked:
		default:
			t.Fatalf("%s never asked the daemon", who)
		}
	}

	start := time.Now()
	got := claudeContextHintOutput(claudeHookInput{Event: "UserPromptSubmit", CWD: sub, SessionID: "c", Prompt: "fix `Cart.Total`"}, askContextHint)
	if took := time.Since(start); got != "" || took > limit {
		t.Fatalf("Claude Code: printed %q after %v, want nothing within %v", got, took, limit)
	}
	reached("Claude Code")

	start = time.Now()
	out := codexHookOutput(codexHookInput{Event: "UserPromptSubmit", CWD: sub, SessionID: "thr-1", TurnID: "t1", Prompt: "fix `Cart.Total`"}, nil, nil, askContextHint)
	if took := time.Since(start); out != nil || took > limit {
		t.Fatalf("Codex: output %v after %v, want none within %v", out, took, limit)
	}
	reached("Codex")
}
