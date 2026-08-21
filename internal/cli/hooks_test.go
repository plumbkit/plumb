package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallCodexHooks_MergesAndRefreshesOwnEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	original := map[string]any{
		"description": "user hooks",
		"hooks": map[string]any{
			"SessionStart": []any{map[string]any{
				"matcher": "startup",
				"hooks":   []any{map[string]any{"type": "command", "command": "keep-me"}},
			}},
		},
	}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	changed, err := installCodexHooks(path, "/opt/plumb")
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("first install reported no change")
	}

	got := readHookConfig(t, path)
	if got["description"] != "user hooks" {
		t.Errorf("description = %v, want user metadata preserved", got["description"])
	}
	hooks := got["hooks"].(map[string]any)
	if !hasCodexHook(hooks, "SessionStart", codexSessionHookStatus, `"/opt/plumb" hooks run-codex`) {
		t.Error("SessionStart Plumb hook missing")
	}
	if !hasCodexHook(hooks, "Stop", codexMailboxHookStatus, `"/opt/plumb" hooks run-codex`) {
		t.Error("Stop Plumb hook missing")
	}
	if !hasCommand(hooks, "SessionStart", "keep-me") {
		t.Error("existing SessionStart handler was not preserved")
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if !containsBackup(entries) {
		t.Error("changed existing config was not backed up")
	}

	changed, err = installCodexHooks(path, "/opt/plumb")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("second install was not idempotent")
	}

	changed, err = installCodexHooks(path, "/new/plumb")
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("new executable path did not refresh Plumb handlers")
	}
	got = readHookConfig(t, path)
	hooks = got["hooks"].(map[string]any)
	if !hasCodexHook(hooks, "Stop", codexMailboxHookStatus, `"/new/plumb" hooks run-codex`) {
		t.Error("Stop hook did not refresh to the new executable")
	}
}

func TestInstallCodexHooks_RefusesInvalidConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	if err := os.WriteFile(path, []byte(`{"hooks":"wrong"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := installCodexHooks(path, "/opt/plumb")
	if err == nil || !strings.Contains(err.Error(), "hooks must be an object") {
		t.Fatalf("installCodexHooks error = %v, want hooks type error", err)
	}
}

func TestCodexHookResult(t *testing.T) {
	session := codexHookResult(codexHookInput{Event: "SessionStart", SessionID: "thr_123"}, nil)
	if session == nil {
		t.Fatal("SessionStart produced no context")
	}
	context := session["hookSpecificOutput"].(map[string]any)["additionalContext"].(string)
	if !strings.Contains(context, `"thr_123"`) || !strings.Contains(context, "session_id") {
		t.Errorf("SessionStart context = %q, want session ID and linkage", context)
	}

	called := false
	probe := func(_, _ string) (mailReport, bool) {
		called = true
		return mailReport{Count: 2}, true
	}
	if out := codexHookResult(codexHookInput{Event: "Stop", StopHookActive: true}, probe); out != nil {
		t.Errorf("active Stop output = %v, want nil", out)
	}
	if called {
		t.Error("active Stop invoked the mailbox probe")
	}

	stop := codexHookResult(codexHookInput{Event: "Stop", SessionID: "thr_123", CWD: "/repo"}, probe)
	if stop == nil || stop["decision"] != "block" {
		t.Errorf("unread mail Stop output = %v, want block", stop)
	}
	if got := stop["reason"].(string); !strings.Contains(got, "2 unread") || !strings.Contains(got, "check_messages") {
		t.Errorf("Stop reason = %q", got)
	}

	if out := codexHookResult(codexHookInput{Event: "Stop"}, func(_, _ string) (mailReport, bool) {
		return mailReport{}, false
	}); out != nil {
		t.Errorf("failed probe output = %v, want nil", out)
	}
}

func readHookConfig(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func hasCodexHook(hooks map[string]any, event, status, command string) bool {
	groups, _ := hooks[event].([]any)
	for _, groupAny := range groups {
		group, _ := groupAny.(map[string]any)
		handlers, _ := group["hooks"].([]any)
		for _, handlerAny := range handlers {
			handler, _ := handlerAny.(map[string]any)
			if handler["statusMessage"] == status && handler["command"] == command && handler["timeout"] == float64(5) {
				return true
			}
		}
	}
	return false
}

func hasCommand(hooks map[string]any, event, command string) bool {
	groups, _ := hooks[event].([]any)
	for _, groupAny := range groups {
		group, _ := groupAny.(map[string]any)
		handlers, _ := group["hooks"].([]any)
		for _, handlerAny := range handlers {
			handler, _ := handlerAny.(map[string]any)
			if handler["command"] == command {
				return true
			}
		}
	}
	return false
}

func containsBackup(entries []os.DirEntry) bool {
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".bak") {
			return true
		}
	}
	return false
}
