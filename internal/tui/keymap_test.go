package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// warningsContain reports whether any warning contains all of the substrings.
func warningsContain(warnings []string, subs ...string) bool {
	for _, w := range warnings {
		all := true
		for _, s := range subs {
			if !strings.Contains(w, s) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

func TestResolveKeymap_DefaultsWhenAbsent(t *testing.T) {
	km, warnings := resolveKeymap(nil)
	if len(warnings) != 0 {
		t.Fatalf("defaults produced warnings: %v", warnings)
	}
	for _, d := range defaultKeymap {
		if got := km.normalise(d.key); got != d.key {
			t.Errorf("default %s: normalise(%q) = %q, want identity", d.action, d.key, got)
		}
		if got := km.display(d.action); got != d.key {
			t.Errorf("display(%s) = %q, want %q", d.action, got, d.key)
		}
	}
	// An unrelated, non-rebindable key passes through unchanged.
	if got := km.normalise("enter"); got != "enter" {
		t.Errorf("normalise(enter) = %q, want passthrough", got)
	}
}

func TestResolveKeymap_OverrideApplies(t *testing.T) {
	km, warnings := resolveKeymap(map[string]string{"quit": "x"})
	if len(warnings) != 0 {
		t.Fatalf("clean override produced warnings: %v", warnings)
	}
	if got := km.normalise("x"); got != "ctrl+q" {
		t.Errorf("normalise(x) = %q, want ctrl+q (rebound quit)", got)
	}
	// The displaced default no longer dispatches.
	if got := km.normalise("ctrl+q"); got != "" {
		t.Errorf("normalise(ctrl+q) = %q, want \"\" (displaced default is dead)", got)
	}
	if got := km.display(actQuit); got != "x" {
		t.Errorf("display(quit) = %q, want x", got)
	}
}

func TestResolveKeymap_UnknownActionWarnedAndIgnored(t *testing.T) {
	km, warnings := resolveKeymap(map[string]string{"frobnicate": "z"})
	if !warningsContain(warnings, "unknown action", "frobnicate") {
		t.Fatalf("no unknown-action warning: %v", warnings)
	}
	// The bogus binding did nothing; defaults are intact and "z" is inert.
	if got := km.normalise("z"); got != "z" {
		t.Errorf("normalise(z) = %q, want passthrough after ignored unknown action", got)
	}
	if got := km.normalise("ctrl+q"); got != "ctrl+q" {
		t.Errorf("normalise(ctrl+q) = %q, want default preserved", got)
	}
}

func TestResolveKeymap_DuplicateKeyWarnedLaterIgnored(t *testing.T) {
	// refresh and rename both request "z". Overrides apply in action-name order
	// (refresh < rename), so rename is the later binding and is dropped.
	km, warnings := resolveKeymap(map[string]string{"refresh": "z", "rename": "z"})
	if !warningsContain(warnings, "refresh", "rename", "z") {
		t.Fatalf("no duplicate-key warning naming both actions: %v", warnings)
	}
	if got := km.normalise("z"); got != "a" {
		t.Errorf("normalise(z) = %q, want a (refresh won the key)", got)
	}
	// rename kept its default.
	if got := km.display(actRename); got != "r" {
		t.Errorf("display(rename) = %q, want default r after being ignored", got)
	}
	if got := km.normalise("r"); got != "r" {
		t.Errorf("normalise(r) = %q, want r (rename default intact)", got)
	}
}

func TestResolveKeymap_FixedKeyCollisionWarnedAndIgnored(t *testing.T) {
	km, warnings := resolveKeymap(map[string]string{"quit": "esc"})
	if !warningsContain(warnings, "fixed key", "quit") {
		t.Fatalf("no fixed-key collision warning: %v", warnings)
	}
	if got := km.display(actQuit); got != "ctrl+q" {
		t.Errorf("display(quit) = %q, want default ctrl+q after ignored fixed collision", got)
	}
	if got := km.normalise("esc"); got != "esc" {
		t.Errorf("normalise(esc) = %q, want esc unchanged (fixed key not captured)", got)
	}
}

func TestResolveKeymap_ExplicitShadowsAnotherDefault(t *testing.T) {
	// Binding refresh to the arrow-up key (nav_up's default) is allowed: the
	// explicit binding wins, nav_up's default is shadowed, and that is reported.
	km, warnings := resolveKeymap(map[string]string{"refresh": "up"})
	if !warningsContain(warnings, "shadows", "refresh", "nav_up") {
		t.Fatalf("no shadowed-default warning: %v", warnings)
	}
	if got := km.normalise("up"); got != "a" {
		t.Errorf("normalise(up) = %q, want a (refresh took the arrow)", got)
	}
}

func TestResolveKeymap_EmptyKeyIgnored(t *testing.T) {
	km, warnings := resolveKeymap(map[string]string{"quit": "  "})
	if !warningsContain(warnings, "empty key", "quit") {
		t.Fatalf("no empty-key warning: %v", warnings)
	}
	if got := km.display(actQuit); got != "ctrl+q" {
		t.Errorf("display(quit) = %q, want default preserved on empty key", got)
	}
}

// TestKeymapDefaultsMatchHandlers is the guard that keeps the default keys in
// sync with the literal switch cases: every default key must normalise to
// itself under the default keymap (so the existing case labels still fire), and
// no default key may sit in the reserved (fixed) set.
func TestKeymapDefaultsMatchHandlers(t *testing.T) {
	km := defaultKeys()
	seen := map[string]keyAction{}
	for _, d := range defaultKeymap {
		if prev, dup := seen[d.key]; dup {
			t.Errorf("default key %q assigned to both %s and %s", d.key, prev, d.action)
		}
		seen[d.key] = d.action
		if _, reserved := reservedKeys[d.key]; reserved {
			t.Errorf("default key %q for %s is also in reservedKeys", d.key, d.action)
		}
		if got := km.normalise(d.key); got != d.key {
			t.Errorf("default key %q for %s does not normalise to itself (got %q)", d.key, d.action, got)
		}
	}
}

// TestReboundKeyDrivesActionInUpdate proves at the Update level that a rebound
// navigation key drives the action and the displaced default no longer does.
func TestReboundKeyDrivesActionInUpdate(t *testing.T) {
	km, _ := resolveKeymap(map[string]string{"nav_up": "w"})
	m := Model{currentSection: 0, dashScroll: 5, width: 100, height: 30, keys: km}

	// The rebound key "w" scrolls the dashboard up (nav_up).
	updated, _ := m.Update(keyPress("w"))
	m = updated.(Model)
	if m.dashScroll != 4 {
		t.Fatalf("dashScroll after rebound w = %d, want 4 (nav_up fired)", m.dashScroll)
	}

	// The displaced default (arrow up) no longer scrolls.
	updated, _ = m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyUp}))
	m = updated.(Model)
	if m.dashScroll != 4 {
		t.Fatalf("dashScroll after displaced arrow-up = %d, want 4 (default is dead)", m.dashScroll)
	}
}

// TestReboundQuitInUpdate proves the rebound quit key returns a quit command
// while the original ctrl+q no longer does.
func TestReboundQuitInUpdate(t *testing.T) {
	km, _ := resolveKeymap(map[string]string{"quit": "x"})
	m := Model{currentSection: 1, width: 100, height: 30, keys: km}

	_, cmd := m.Update(keyPress("x"))
	if cmd == nil {
		t.Fatal("rebound quit key x did not return a quit command")
	}

	_, cmd = m.Update(ctrlKey('q'))
	if cmd != nil {
		t.Fatal("displaced default ctrl+q still returned a command")
	}
}
