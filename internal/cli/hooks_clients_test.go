package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRemoveHooksAt_RemovesOnlyPlumbs is the uninstall's core promise: after it
// runs, the user's own hooks are exactly where they were and plumb's are gone.
func TestRemoveHooksAt_RemovesOnlyPlumbs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	writeJSONFixture(t, path, map[string]any{
		"model": "opus",
		"hooks": map[string]any{
			"SessionStart": []any{map[string]any{
				"matcher": "startup",
				"hooks":   []any{map[string]any{"type": "command", "command": "mine.sh"}},
			}},
			"PreToolUse": []any{map[string]any{
				"hooks": []any{map[string]any{"type": "command", "command": "audit.sh"}},
			}},
		},
	})

	if _, err := installHooksAt(path, claudeHookEntries("/opt/plumb"), claudeHookOwned); err != nil {
		t.Fatal(err)
	}
	removed, err := removeHooksAt(path, claudeHookOwned)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Errorf("removed = %d, want 2", removed)
	}

	got := readHookJSON(t, path)
	if got["model"] != "opus" {
		t.Errorf("unrelated settings lost: %v", got)
	}
	hooks := got["hooks"].(map[string]any)
	if !hasCommand(hooks, "SessionStart", "mine.sh") || !hasCommand(hooks, "PreToolUse", "audit.sh") {
		t.Errorf("user hooks did not survive the uninstall: %v", hooks)
	}
	if _, ok := hooks["Stop"]; ok {
		t.Error("Stop key survived with nothing in it — an empty event must go")
	}
	// The user's SessionStart group keeps its matcher and its position; only
	// plumb's separate group went.
	groups := hooks["SessionStart"].([]any)
	if len(groups) != 1 || groups[0].(map[string]any)["matcher"] != "startup" {
		t.Errorf("SessionStart groups = %v, want only the user's matcher group", groups)
	}
}

// TestRemoveHooksAt_NoPlumbHooksIsANoOp pins the "safe to repeat" property an
// uninstall shares with a registration removal: nothing matched, nothing
// written — not even a backup.
func TestRemoveHooksAt_NoPlumbHooksIsANoOp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	body := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"mine.sh"}]}]}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	removed, err := removeHooksAt(path, claudeHookOwned)
	if err != nil || removed != 0 {
		t.Fatalf("removed = %d, err = %v, want 0, nil", removed, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != body {
		t.Errorf("file was rewritten by a no-op uninstall:\n%s", data)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if containsBackup(entries) {
		t.Error("a no-op uninstall left a backup behind")
	}
}

// TestRemoveHooksAt_DropsAConfigItEmptied: a hooks file plumb created for its
// own handlers leaves no empty shell behind — the same call `plumb setup
// --uninstall` makes about a server map it emptied.
func TestRemoveHooksAt_DropsAConfigItEmptied(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.json")
	if _, err := installHooksAt(path, codexHookEntries("/opt/plumb"), codexHookOwned); err != nil {
		t.Fatal(err)
	}
	if _, err := removeHooksAt(path, codexHookOwned); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		data, _ := os.ReadFile(path)
		t.Errorf("config plumb created and then emptied survived as %q", data)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !containsBackup(entries) {
		t.Error("the removal is not recoverable — no backup was taken")
	}
}

// TestRemoveHooksAt_KeepsAConfigWithOtherContent is the other half: a file that
// holds anything besides plumb's hooks is edited, never deleted.
func TestRemoveHooksAt_KeepsAConfigWithOtherContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	writeJSONFixture(t, path, map[string]any{"model": "opus"})
	if _, err := installHooksAt(path, claudeHookEntries("/opt/plumb"), claudeHookOwned); err != nil {
		t.Fatal(err)
	}
	if _, err := removeHooksAt(path, claudeHookOwned); err != nil {
		t.Fatal(err)
	}
	got := readHookJSON(t, path)
	if got["model"] != "opus" {
		t.Errorf("settings = %v, want the user's own keys intact", got)
	}
	if _, ok := got["hooks"]; ok {
		t.Errorf("an emptied hooks key survived: %v", got)
	}
}

// TestRemoveHooksAt_AbsentConfig covers the ordinary case of uninstalling from
// a client that never had hooks installed.
func TestRemoveHooksAt_AbsentConfig(t *testing.T) {
	removed, err := removeHooksAt(filepath.Join(t.TempDir(), "nope.json"), claudeHookOwned)
	if err != nil || removed != 0 {
		t.Fatalf("removed = %d, err = %v, want 0, nil", removed, err)
	}
}

// TestInstallHooksAt_MigratesLegacyScriptHooks is the migration path for the
// hand-installed shell hooks plumb's own recipe documented: they are plumb's,
// so an install replaces them where they stand rather than adding a second pair
// that fires alongside them.
func TestInstallHooksAt_MigratesLegacyScriptHooks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	writeJSONFixture(t, path, map[string]any{
		"hooks": map[string]any{
			"SessionStart": []any{map[string]any{
				"hooks": []any{map[string]any{
					"type": "command", "command": "/home/u/.claude/hooks/plumb-session-link.sh", "timeout": float64(5),
				}},
			}},
			"Stop": []any{map[string]any{
				"hooks": []any{map[string]any{
					"type": "command", "command": "/home/u/.claude/hooks/plumb-mail-wake.sh",
					"timeout": float64(330), "async": true, "asyncRewake": true,
				}},
			}},
		},
	})

	// The legacy entries read as plumb's own, and as stale rather than missing.
	states, err := hookStatesAt(path, claudeHookEntries("/opt/plumb"), claudeHookOwned)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range states {
		if s.state != hookStateStale {
			t.Errorf("%s state = %q, want stale", s.entry.label, s.state)
		}
		if !strings.Contains(s.detail, ".sh") {
			t.Errorf("%s detail = %q, want the script it currently runs", s.entry.label, s.detail)
		}
	}

	if _, err := installHooksAt(path, claudeHookEntries("/opt/plumb"), claudeHookOwned); err != nil {
		t.Fatal(err)
	}
	hooks := readHookJSON(t, path)["hooks"].(map[string]any)
	for _, event := range []string{"SessionStart", "Stop"} {
		if hasCommand(hooks, event, "/home/u/.claude/hooks/plumb-session-link.sh") ||
			hasCommand(hooks, event, "/home/u/.claude/hooks/plumb-mail-wake.sh") {
			t.Errorf("%s still runs the legacy script — the migration duplicated instead of replacing", event)
		}
		if !hasCommand(hooks, event, `"/opt/plumb" hooks run-claude`) {
			t.Errorf("%s does not run the built-in verb after migration", event)
		}
		if groups := hooks[event].([]any); len(groups) != 1 {
			t.Errorf("%s has %d groups, want 1 — the migration added a second", event, len(groups))
		}
	}
}

func TestHookStatesAt_Classification(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	entries := claudeHookEntries("/opt/plumb")

	states, err := hookStatesAt(path, entries, claudeHookOwned)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range states {
		if s.state != hookStateMissing {
			t.Errorf("absent config: %s = %q, want missing", s.entry.label, s.state)
		}
	}

	if _, err := installHooksAt(path, entries, claudeHookOwned); err != nil {
		t.Fatal(err)
	}
	states, err = hookStatesAt(path, entries, claudeHookOwned)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range states {
		if s.state != hookStateInstalled {
			t.Errorf("after install: %s = %q, want installed", s.entry.label, s.state)
		}
	}

	// A binary that moved is stale, and the detail names the path it still runs
	// — the fact a reader needs to decide whether to refresh.
	states, err = hookStatesAt(path, claudeHookEntries("/new/plumb"), claudeHookOwned)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range states {
		if s.state != hookStateStale || !strings.Contains(s.detail, "/opt/plumb") {
			t.Errorf("moved binary: %s = %q (%s), want stale naming the old path", s.entry.label, s.state, s.detail)
		}
	}
}

// TestClaudeHookEntries_StopOutlivesItsWatcher pins the one number that would
// silently break the wake: a client-side timeout at or below the watcher's own
// window kills the watcher mid-watch, and nothing in any output would say so.
func TestClaudeHookEntries_StopOutlivesItsWatcher(t *testing.T) {
	// The entry is derived from the window in effect, so pin the window: a
	// developer or runner with PLUMB_WAKE_WINDOW exported would otherwise get a
	// red suite on correct code.
	t.Setenv("PLUMB_WAKE_WINDOW", "300")
	for _, e := range claudeHookEntries("/opt/plumb") {
		if e.event != "Stop" {
			continue
		}
		timeout, ok := e.handler["timeout"].(float64)
		if !ok {
			t.Fatalf("Stop timeout = %v, want a number", e.handler["timeout"])
		}
		if window := claudeWakeWindowDefault.Seconds(); timeout <= window {
			t.Errorf("Stop timeout %.0fs does not outlive the %.0fs watch window", timeout, window)
		}
		if e.handler["async"] != true || e.handler["asyncRewake"] != true {
			t.Errorf("Stop handler = %v, want async + asyncRewake (the pair that wakes an idle session)", e.handler)
		}
		return
	}
	t.Fatal("no Stop entry in the Claude Code pack")
}

// TestHooksTargets_AreRegistryDriven guards the property that makes this a
// registry rather than two hardcoded clients: every target must resolve by
// name, carry a setup target to gate on, and render entries.
func TestHooksTargets_AreRegistryDriven(t *testing.T) {
	for _, target := range hooksTargets() {
		found, ok := findHooksTarget(target.use)
		if !ok || found.name != target.name {
			t.Errorf("findHooksTarget(%q) did not resolve to %s", target.use, target.name)
		}
		if target.setup.use == "" || target.pathFn == nil || target.ours == nil {
			t.Errorf("%s is missing a registry field", target.name)
		}
		entries := target.entries("/opt/plumb")
		if len(entries) == 0 {
			t.Errorf("%s renders no hook entries", target.name)
		}
		for _, e := range entries {
			if e.event == "" || e.label == "" {
				t.Errorf("%s has an unlabelled entry: %+v", target.name, e)
			}
			if !target.ours(e.event, e.handler) {
				t.Errorf("%s does not recognise its own handler for %s — install would duplicate on every run", target.name, e.event)
			}
		}
	}
	if _, ok := findHooksTarget("nope"); ok {
		t.Error("findHooksTarget accepted an unknown client")
	}
}

// TestRemoveHooksAt_NeverTouchesLookalikeHooks is the defect an independent
// review of PR #396 found: ownership was a substring test over the whole
// command line, applied to every event, so `plumb setup claude-code
// --uninstall` deleted hooks belonging to a user who had never installed
// plumb's. Removing a hook plumb did not install is the one failure this
// command must not have.
func TestRemoveHooksAt_NeverTouchesLookalikeHooks(t *testing.T) {
	for _, tc := range []struct {
		name, event, command string
	}{
		{"user wrapper around the legacy script", "Stop", "/home/u/bin/wrap-plumb-mail-wake.sh"},
		{"legacy script name as an argument", "Stop", "/home/u/bin/runner --hook plumb-mail-wake.sh"},
		{"legacy script on an event plumb never installs on", "PreToolUse", "/usr/local/bin/plumb-session-link.sh --mine"},
		{"plumb's verb mentioned in an argument", "Stop", "/home/u/bin/logger --note 'hooks run-claude'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.json")
			writeJSONFixture(t, path, map[string]any{"hooks": map[string]any{
				tc.event: []any{map[string]any{"hooks": []any{
					map[string]any{"type": "command", "command": tc.command},
				}}},
			}})
			removed, err := removeHooksAt(path, claudeHookOwned)
			if err != nil {
				t.Fatal(err)
			}
			if removed != 0 {
				t.Errorf("removed %d handler(s) — %q on %s is the user's, not plumb's", removed, tc.command, tc.event)
			}
			hooks := readHookJSON(t, path)["hooks"].(map[string]any)
			if !hasCommand(hooks, tc.event, tc.command) {
				t.Errorf("the user's own hook was deleted: %v", hooks)
			}
		})
	}
}

// TestRemoveHooksAt_KeepsStructureItDidNotEmpty is the second blocking defect
// from that review: a group the user left empty (Claude Code's own /hooks
// editor does this) was read as plumb's residue, so the group, then the event
// key, then — when nothing else was in the file — the settings file itself were
// deleted.
func TestRemoveHooksAt_KeepsStructureItDidNotEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	writeJSONFixture(t, path, map[string]any{"hooks": map[string]any{
		"Notification": []any{map[string]any{"matcher": "*", "hooks": []any{}}},
		"PreCompact":   []any{},
		// A BARE empty group, with no keys of its own. Without one here the
		// plumbShapedGroup clause keeps every fixture group on its own and the
		// "did I empty it" half of the guard goes unpinned — which an
		// independent review demonstrated by mutating it out and watching this
		// test stay green.
		"Notification2": []any{map[string]any{"hooks": []any{}}},
	}})

	if _, err := installHooksAt(path, claudeHookEntries("/opt/plumb"), claudeHookOwned); err != nil {
		t.Fatal(err)
	}
	if _, err := removeHooksAt(path, claudeHookOwned); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the settings file was deleted: %v", err)
	}
	hooks, ok := readHookJSON(t, path)["hooks"].(map[string]any)
	if !ok {
		t.Fatal("the hooks key was deleted along with the user's empty groups")
	}
	// Assert the GROUP, not just the key: with only a key check, dropping the
	// user's empty group leaves "Notification": [] behind and the test passes
	// while the damage is done — which is exactly how this read before an
	// independent review mutated the guard out and watched it stay green.
	groups, ok := hooks["Notification"].([]any)
	if !ok || len(groups) != 1 {
		t.Fatalf("the user's empty matcher group was removed: %v", hooks)
	}
	if group, ok := groups[0].(map[string]any); !ok || group["matcher"] != "*" {
		t.Errorf("the user's group lost its matcher: %v", groups[0])
	}
	if _, ok := hooks["PreCompact"]; !ok {
		t.Errorf("the user's empty event was removed: %v", hooks)
	}
	if groups, ok := hooks["Notification2"].([]any); !ok || len(groups) != 1 {
		t.Errorf("a bare empty group the user already had was removed: %v", hooks)
	}
	for _, event := range []string{"SessionStart", "Stop"} {
		if _, ok := hooks[event]; ok {
			t.Errorf("%s survived the uninstall: %v", event, hooks)
		}
	}
}

// TestInstallHooksAt_RefreshesEveryPlumbHandlerOnAnEvent: the upsert stopped at
// the first match while the removal took them all, so a config holding both a
// legacy script entry and a current one kept the leftover — two hooks firing,
// reported as one clean install.
func TestInstallHooksAt_RefreshesEveryPlumbHandlerOnAnEvent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	writeJSONFixture(t, path, map[string]any{"hooks": map[string]any{
		"SessionStart": []any{
			map[string]any{"hooks": []any{map[string]any{
				"type": "command", "command": `"/old/plumb" hooks run-claude`, "timeout": float64(5),
			}}},
			map[string]any{"hooks": []any{map[string]any{
				"type": "command", "command": "/home/u/.claude/hooks/plumb-session-link.sh", "timeout": float64(5),
			}}},
		},
	}})

	if _, err := installHooksAt(path, claudeHookEntries("/opt/plumb"), claudeHookOwned); err != nil {
		t.Fatal(err)
	}
	hooks := readHookJSON(t, path)["hooks"].(map[string]any)
	if hasCommand(hooks, "SessionStart", "/home/u/.claude/hooks/plumb-session-link.sh") {
		t.Error("the legacy handler survived an install — it and the current hook would both fire")
	}
	if hasCommand(hooks, "SessionStart", `"/old/plumb" hooks run-claude`) {
		t.Error("the stale handler survived an install")
	}
	states, err := hookStatesAt(path, claudeHookEntries("/opt/plumb"), claudeHookOwned)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range states {
		if s.state != hookStateInstalled {
			t.Errorf("%s = %q after install, want installed", s.entry.label, s.state)
		}
	}
}

// TestRemoveHooksAt_KeepsAGroupTheUserOwns: plumb only ever writes a bare
// {"hooks":[handler]} group, so a group carrying a matcher — or any key of the
// user's — is theirs even when plumb's handler is the only thing inside it.
// Emptying it must not take the group, the event, or the file with it.
func TestRemoveHooksAt_KeepsAGroupTheUserOwns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	writeJSONFixture(t, path, map[string]any{"hooks": map[string]any{
		"SessionStart": []any{map[string]any{
			"matcher": "startup|resume",
			"myNote":  "do not lose me",
			"hooks": []any{map[string]any{
				"type": "command", "command": `"/opt/plumb" hooks run-claude`, "timeout": float64(5),
			}},
		}},
	}})

	removed, err := removeHooksAt(path, claudeHookOwned)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the settings file was deleted with the user's group: %v", err)
	}
	groups, ok := readHookJSON(t, path)["hooks"].(map[string]any)["SessionStart"].([]any)
	if !ok || len(groups) != 1 {
		t.Fatalf("the user's group was dropped: %v", readHookJSON(t, path))
	}
	group := groups[0].(map[string]any)
	if group["matcher"] != "startup|resume" || group["myNote"] != "do not lose me" {
		t.Errorf("the user's own keys went with plumb's handler: %v", group)
	}
	if handlers, _ := group["hooks"].([]any); len(handlers) != 0 {
		t.Errorf("plumb's handler survived: %v", handlers)
	}
}

// TestInstallHooksAt_CollapsesDuplicateHandlers: refreshing every plumb handler
// on an event turned "legacy + current" into "current + current" — still two
// handlers, both firing, still reported as one clean install. Only the first
// survives.
func TestInstallHooksAt_CollapsesDuplicateHandlers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	writeJSONFixture(t, path, map[string]any{"hooks": map[string]any{
		"SessionStart": []any{
			map[string]any{"hooks": []any{map[string]any{
				"type": "command", "command": `"/old/plumb" hooks run-claude`, "timeout": float64(5),
			}}},
			map[string]any{"hooks": []any{map[string]any{
				"type": "command", "command": "/home/u/.claude/hooks/plumb-session-link.sh", "timeout": float64(5),
			}}},
		},
	}})

	if _, err := installHooksAt(path, claudeHookEntries("/opt/plumb"), claudeHookOwned); err != nil {
		t.Fatal(err)
	}
	if n := countHandlers(readHookJSON(t, path), "SessionStart"); n != 1 {
		t.Errorf("SessionStart has %d handlers after install, want 1 — duplicates both fire", n)
	}

	// And a re-run of an already-correct install stays a no-op.
	changed, err := installHooksAt(path, claudeHookEntries("/opt/plumb"), claudeHookOwned)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("re-installing over the collapsed result reported a change")
	}
}

// TestRemoveHooksAt_CodexMarkerIsEventScoped: the status line is plumb's marker
// only on the events plumb installs on. A user's handler carrying that label on
// another event is theirs — and taking it would empty the file and delete it.
func TestRemoveHooksAt_CodexMarkerIsEventScoped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	writeJSONFixture(t, path, map[string]any{"hooks": map[string]any{
		"PreToolUse": []any{map[string]any{"hooks": []any{map[string]any{
			"type": "command", "command": "/home/u/bin/audit.sh", "statusMessage": codexMailboxHookStatus,
		}}}},
	}})

	removed, err := removeHooksAt(path, codexHookOwned)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Errorf("removed %d handler(s) — a user's PreToolUse hook is not plumb's", removed)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the user's hooks.json was deleted: %v", err)
	}
}

func countHandlers(cfg map[string]any, event string) int {
	n := 0
	groups, _ := cfg["hooks"].(map[string]any)[event].([]any)
	for _, groupAny := range groups {
		group, _ := groupAny.(map[string]any)
		handlers, _ := group["hooks"].([]any)
		n += len(handlers)
	}
	return n
}

// TestInstallHooksAt_KeepsABareEmptyGroupItDidNotEmpty is the install-side twin
// of the removal rule. The two writers share the "only what I emptied, only if
// it was mine" test, and only the removal path had it — so an install deleted a
// bare empty group the user already had, and reported a change for it.
func TestInstallHooksAt_KeepsABareEmptyGroupItDidNotEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	writeJSONFixture(t, path, map[string]any{"hooks": map[string]any{
		"SessionStart": []any{map[string]any{"hooks": []any{}}},
	}})

	if _, err := installHooksAt(path, claudeHookEntries("/opt/plumb"), claudeHookOwned); err != nil {
		t.Fatal(err)
	}
	groups, _ := readHookJSON(t, path)["hooks"].(map[string]any)["SessionStart"].([]any)
	if len(groups) != 2 {
		t.Errorf("SessionStart has %d group(s), want the user's empty one plus plumb's: %v", len(groups), groups)
	}
	if n := countHandlers(readHookJSON(t, path), "SessionStart"); n != 1 {
		t.Errorf("SessionStart has %d handlers, want 1", n)
	}
}

// TestCommandExecutable covers the hand-rolled command-line parsing that decides
// ownership. Nothing tested it directly, so the tab case and the quoting rules
// rested on inspection alone.
func TestCommandExecutable(t *testing.T) {
	for _, tc := range []struct{ name, cmd, want string }{
		{"quoted path", `"/opt/plumb" hooks run-claude`, "/opt/plumb"},
		{"quoted path with a space", `"/opt/my plumb/plumb" hooks run-claude`, "/opt/my plumb/plumb"},
		{"unquoted", "/opt/plumb hooks run-claude", "/opt/plumb"},
		{"tab separated", "/opt/plumb\thooks run-claude", "/opt/plumb"},
		{"bare name on PATH", "plumb hooks run-claude", "plumb"},
		{"leading whitespace", "   /opt/plumb hooks run-claude", "/opt/plumb"},
		{"no arguments", "/opt/plumb", "/opt/plumb"},
		{"script", "/home/u/.claude/hooks/plumb-mail-wake.sh", "/home/u/.claude/hooks/plumb-mail-wake.sh"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := commandExecutable(tc.cmd); got != tc.want {
				t.Errorf("commandExecutable(%q) = %q, want %q", tc.cmd, got, tc.want)
			}
		})
	}
}

// TestRunsPlumbVerb: the verb has to be what the command RUNS, not a string that
// appears somewhere in it.
func TestRunsPlumbVerb(t *testing.T) {
	for _, tc := range []struct {
		cmd  string
		want bool
	}{
		{`"/opt/plumb" hooks run-claude`, true},
		{"/opt/plumb hooks run-claude", true},
		{"/opt/plumb\thooks run-claude", true},
		{"plumb hooks run-claude --debug", true},
		{"/home/u/bin/logger --note 'hooks run-claude'", false},
		{"/home/u/bin/hooks run-claude.sh", false},
		{"", false},
	} {
		if got := runsPlumbVerb(tc.cmd, claudeHookVerb); got != tc.want {
			t.Errorf("runsPlumbVerb(%q) = %v, want %v", tc.cmd, got, tc.want)
		}
	}
}

func TestPlumbShapedGroup(t *testing.T) {
	if !plumbShapedGroup(map[string]any{"hooks": []any{}}) {
		t.Error("a bare hooks group is plumb's own shape")
	}
	if plumbShapedGroup(map[string]any{"hooks": []any{}, "matcher": "*"}) {
		t.Error("a group carrying a matcher is the user's, not plumb's shape")
	}
}
