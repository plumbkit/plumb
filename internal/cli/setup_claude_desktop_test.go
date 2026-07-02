package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// claudeDesktopTestBase points claudeDesktopConfigBaseDir at a temp directory
// for the duration of the test, regardless of OS (HOME on darwin/Linux,
// APPDATA on Windows).
func claudeDesktopTestBase(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if runtime.GOOS == "windows" {
		t.Setenv("APPDATA", home)
	}
	base, err := claudeDesktopConfigBaseDir()
	if err != nil {
		t.Fatalf("claudeDesktopConfigBaseDir: %v", err)
	}
	return base
}

func writeStubClaudeConfig(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "claude_desktop_config.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestClaudeDesktopExtraConfigPaths_DiscoversOnlyExistingSiblingProfiles guards
// the heuristic sibling-profile scan: it must pick up a "Claude*" directory that
// already has its own config (the unofficial multi-account convention), skip a
// "Claude*"-named directory with no config file, skip an unrelated app's
// directory, and never include the canonical path itself.
func TestClaudeDesktopExtraConfigPaths_DiscoversOnlyExistingSiblingProfiles(t *testing.T) {
	base := claudeDesktopTestBase(t)

	writeStubClaudeConfig(t, filepath.Join(base, "Claude")) // canonical
	sibling := writeStubClaudeConfig(t, filepath.Join(base, "Claude-Personal"))
	if err := os.MkdirAll(filepath.Join(base, "Claude-Empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeStubClaudeConfig(t, filepath.Join(base, "SomeOtherApp"))

	extras, err := claudeDesktopExtraConfigPaths()
	if err != nil {
		t.Fatalf("claudeDesktopExtraConfigPaths: %v", err)
	}
	if len(extras) != 1 || extras[0] != sibling {
		t.Errorf("extras = %v, want exactly [%s]", extras, sibling)
	}
}

func TestClaudeDesktopExtraConfigPaths_NoneFound(t *testing.T) {
	claudeDesktopTestBase(t) // fresh empty base dir, nothing written

	extras, err := claudeDesktopExtraConfigPaths()
	if err != nil {
		t.Fatalf("claudeDesktopExtraConfigPaths: %v", err)
	}
	if len(extras) != 0 {
		t.Errorf("expected no extras, got %v", extras)
	}
}

// TestClaudeDesktopConfigPaths_CanonicalFirstThenExtras guards the ordering and
// completeness contract that runSetupClaudeDesktop and refreshClient depend on:
// canonical path first (even before checking existence), extras after.
func TestClaudeDesktopConfigPaths_CanonicalFirstThenExtras(t *testing.T) {
	base := claudeDesktopTestBase(t)
	canonical := filepath.Join(base, "Claude", "claude_desktop_config.json")
	sibling := writeStubClaudeConfig(t, filepath.Join(base, "Claude-Work"))
	// Canonical directory deliberately left absent — a first-run install.

	paths, err := claudeDesktopConfigPaths()
	if err != nil {
		t.Fatalf("claudeDesktopConfigPaths: %v", err)
	}
	if len(paths) != 2 || paths[0] != canonical || paths[1] != sibling {
		t.Errorf("paths = %v, want [%s, %s]", paths, canonical, sibling)
	}
}

// TestRefreshClient_MultiplePaths guards refreshClient's pathsFn fan-out: every
// resolved path is refreshed independently, and the client is reported changed
// if any one of them was.
func TestRefreshClient_MultiplePaths(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "current.toml")
	stale := filepath.Join(dir, "stale.toml")
	if _, _, err := setupCodexInto(current, "/new/plumb"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := setupCodexInto(stale, "/old/plumb"); err != nil {
		t.Fatal(err)
	}

	c := setupTarget{
		use:       "codex",
		name:      "Codex (multi)",
		pathFn:    func() (string, error) { return current, nil },
		pathsFn:   func() ([]string, error) { return []string{current, stale}, nil },
		intoFn:    setupCodexInto,
		extractFn: mapCommandExtractor(readOrInitCodexConfig, "mcp_servers", "command"),
	}

	status, changed := refreshClient(c, "/new/plumb")
	if !changed {
		t.Errorf("expected changed=true when one of two paths was stale, got status %q", status)
	}

	bin, _, err := mapCommandExtractor(readOrInitCodexConfig, "mcp_servers", "command")(stale)
	if err != nil || bin != "/new/plumb" {
		t.Errorf("stale path not repointed: got %q (err %v)", bin, err)
	}

	// Second pass: both paths now current, no change.
	status, changed = refreshClient(c, "/new/plumb")
	if changed {
		t.Errorf("expected changed=false on second pass, got status %q", status)
	}
	_ = status
}

func TestCheckClaudeDesktopExtraProfiles(t *testing.T) {
	dir := t.TempDir()
	binA := filepath.Join(dir, "binA")
	binB := filepath.Join(dir, "binB")
	for _, p := range []string{binA, binB} {
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o700); err != nil { //nolint:gosec // test fixture
			t.Fatal(err)
		}
	}

	t.Run("no extras found is omitted", func(t *testing.T) {
		claudeDesktopTestBase(t)
		_, ok := checkClaudeDesktopExtraProfiles(binA)
		if ok {
			t.Error("expected ok=false when no extra profile exists")
		}
	})

	t.Run("missing registered binary is a failure", func(t *testing.T) {
		base := claudeDesktopTestBase(t)
		p := filepath.Join(base, "Claude-Personal", "claude_desktop_config.json")
		if _, _, err := setupClaudeDesktopInto(p, filepath.Join(dir, "gone")); err != nil {
			t.Fatal(err)
		}
		res, ok := checkClaudeDesktopExtraProfiles(binA)
		if !ok || res.ok {
			t.Errorf("got (ok=%v, present=%v), want a failing result", res.ok, ok)
		}
	})

	t.Run("mismatched binary is a warning", func(t *testing.T) {
		base := claudeDesktopTestBase(t)
		p := filepath.Join(base, "Claude-Personal", "claude_desktop_config.json")
		if _, _, err := setupClaudeDesktopInto(p, binA); err != nil {
			t.Fatal(err)
		}
		res, ok := checkClaudeDesktopExtraProfiles(binB)
		if !ok || !res.ok || !res.warn {
			t.Errorf("got (ok=%v, warn=%v, present=%v), want ok+warn", res.ok, res.warn, ok)
		}
	})

	t.Run("matching binary is a clean pass", func(t *testing.T) {
		base := claudeDesktopTestBase(t)
		p := filepath.Join(base, "Claude-Personal", "claude_desktop_config.json")
		if _, _, err := setupClaudeDesktopInto(p, binA); err != nil {
			t.Fatal(err)
		}
		res, ok := checkClaudeDesktopExtraProfiles(binA)
		if !ok || !res.ok || res.warn {
			t.Errorf("got (ok=%v, warn=%v, present=%v), want clean pass", res.ok, res.warn, ok)
		}
	})
}
