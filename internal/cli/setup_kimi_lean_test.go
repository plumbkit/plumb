package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/plumbkit/plumb/internal/tools"
)

// wantLeanTools is the allowlist kimiCodeInto must write, as it round-trips
// through JSON ([]any of string). Derived from tools.LeanToolNames(), never a
// literal count — the point is that the allowlist tracks the lean set.
func wantLeanTools() []any {
	names := tools.LeanToolNames()
	out := make([]any, len(names))
	for i, n := range names {
		out[i] = n
	}
	return out
}

// readKimiPlumbEntry returns the plumb server entry from a Kimi-shaped mcp.json.
func readKimiPlumbEntry(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	servers, ok := cfg["mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("mcpServers missing or not an object in %s: %v", path, cfg)
	}
	entry, ok := servers["plumb"].(map[string]any)
	if !ok {
		t.Fatalf("plumb entry missing or not an object in %s: %v", path, servers)
	}
	return entry
}

func assertLeanAllowlist(t *testing.T, path string) {
	t.Helper()
	got := readKimiPlumbEntry(t, path)["enabledTools"]
	if !reflect.DeepEqual(got, wantLeanTools()) {
		t.Errorf("enabledTools = %v, want %v", got, wantLeanTools())
	}
}

// TestKimiCodeInto_Lean walks the whole --lean contract. The cases that matter
// most are (b) and (d): (b) is the short-circuit defeat — without the
// lean-aware idempotence predicate, --lean would silently do nothing on an
// already-registered machine, which is every existing user; (d) is the
// preservation contract shared with Codex's approval tables — a later bare
// re-register (including `plumb setup --all`) must not drop the allowlist.
func TestKimiCodeInto_Lean(t *testing.T) {
	const bin = "/usr/local/bin/plumb"

	t.Run("fresh config with lean writes the sorted allowlist", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "mcp.json")
		added, preserved, err := kimiCodeInto(path, bin, true)
		if err != nil {
			t.Fatalf("kimiCodeInto: %v", err)
		}
		if !added {
			t.Error("expected added=true for a fresh config")
		}
		if len(preserved) != 0 {
			t.Errorf("expected no preserved servers, got %v", preserved)
		}
		entry := readKimiPlumbEntry(t, path)
		if entry["command"] != bin {
			t.Errorf("command = %v, want %q", entry["command"], bin)
		}
		assertLeanAllowlist(t, path)
	})

	t.Run("already registered without the key: lean adds it", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "mcp.json")
		if _, _, err := kimiCodeInto(path, bin, false); err != nil {
			t.Fatalf("bare register: %v", err)
		}
		if _, has := readKimiPlumbEntry(t, path)["enabledTools"]; has {
			t.Fatal("a bare register must not write enabledTools")
		}
		added, _, err := kimiCodeInto(path, bin, true)
		if err != nil {
			t.Fatalf("lean register: %v", err)
		}
		if !added {
			t.Error("expected added=true — the same-binary short-circuit must not swallow --lean")
		}
		assertLeanAllowlist(t, path)
	})

	t.Run("second lean run is a no-op", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "mcp.json")
		if _, _, err := kimiCodeInto(path, bin, true); err != nil {
			t.Fatalf("first lean run: %v", err)
		}
		added, _, err := kimiCodeInto(path, bin, true)
		if err != nil {
			t.Fatalf("second lean run: %v", err)
		}
		if added {
			t.Error("expected added=false on an identical second --lean run")
		}
		assertLeanAllowlist(t, path)
	})

	t.Run("bare re-register preserves the allowlist", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "mcp.json")
		if _, _, err := kimiCodeInto(path, bin, true); err != nil {
			t.Fatalf("lean register: %v", err)
		}
		added, _, err := kimiCodeInto(path, bin, false)
		if err != nil {
			t.Fatalf("bare re-register: %v", err)
		}
		if added {
			t.Error("expected added=false — the entry already points at this binary")
		}
		assertLeanAllowlist(t, path)
	})

	t.Run("repointing the binary preserves the allowlist", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "mcp.json")
		if _, _, err := kimiCodeInto(path, "/old/plumb", true); err != nil {
			t.Fatalf("lean register: %v", err)
		}
		added, _, err := kimiCodeInto(path, bin, false)
		if err != nil {
			t.Fatalf("repoint: %v", err)
		}
		if !added {
			t.Error("expected added=true — the binary changed")
		}
		entry := readKimiPlumbEntry(t, path)
		if entry["command"] != bin {
			t.Errorf("command = %v, want the new binary %q", entry["command"], bin)
		}
		assertLeanAllowlist(t, path)
	})

	t.Run("lean replaces a hand-edited allowlist", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "mcp.json")
		if _, _, err := kimiCodeInto(path, bin, false); err != nil {
			t.Fatalf("bare register: %v", err)
		}
		custom := map[string]any{"mcpServers": map[string]any{"plumb": map[string]any{
			"command":      bin,
			"args":         []string{"serve"},
			"enabledTools": []string{"read_file"},
		}}}
		if err := writeJSON(path, custom); err != nil {
			t.Fatalf("writing custom config: %v", err)
		}
		added, _, err := kimiCodeInto(path, bin, true)
		if err != nil {
			t.Fatalf("lean over custom: %v", err)
		}
		if !added {
			t.Error("expected added=true — a custom list must be replaced by --lean")
		}
		assertLeanAllowlist(t, path)
	})

	t.Run("other MCP servers are preserved", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "mcp.json")
		seed := map[string]any{"mcpServers": map[string]any{"other": map[string]any{"command": "/bin/other"}}}
		if err := writeJSON(path, seed); err != nil {
			t.Fatalf("seeding config: %v", err)
		}
		_, preserved, err := kimiCodeInto(path, bin, true)
		if err != nil {
			t.Fatalf("kimiCodeInto: %v", err)
		}
		if !reflect.DeepEqual(preserved, []string{"other"}) {
			t.Errorf("preserved = %v, want [other]", preserved)
		}
		assertLeanAllowlist(t, path)
	})
}

// setupSubcommand returns the generated `plumb setup <use>` command, failing the
// test when the subcommand does not exist.
func setupSubcommand(t *testing.T, use string) *cobra.Command {
	t.Helper()
	for _, c := range setupCmd.Commands() {
		if c.Name() == use {
			return c
		}
	}
	t.Fatalf("`plumb setup %s` subcommand not found on the command tree", use)
	return nil
}

// TestKimiLeanFlagOnTheCommand pins the CLI seam. Every other test in this file
// calls kimiCodeInto directly, so they all stay green if the generated cobra
// command loses the flag — and `plumb setup kimi-code --lean` then dies with
// "unknown flag" before any of the tested code runs. It asserts three things:
// the flag exists on the kimi-code command, it is bound to the var the target's
// intoFn actually reads, and it is per-target rather than global (a control
// client must not grow it).
func TestKimiLeanFlagOnTheCommand(t *testing.T) {
	kimi := setupSubcommand(t, "kimi-code")
	flag := kimi.Flags().Lookup("lean")
	if flag == nil {
		t.Fatal("`plumb setup kimi-code` must expose --lean — the setupTarget.flags hook is what registers it")
	}

	t.Cleanup(func() {
		setupKimiLeanFlag = false
		_ = kimi.Flags().Set("lean", "false")
	})
	if setupKimiLeanFlag {
		t.Fatal("setupKimiLeanFlag should default to false")
	}
	if err := kimi.Flags().Set("lean", "true"); err != nil {
		t.Fatalf("setting --lean: %v", err)
	}
	if !setupKimiLeanFlag {
		t.Error("--lean must be bound to setupKimiLeanFlag — the var the Kimi target's intoFn reads")
	}

	if f := setupSubcommand(t, "cursor").Flags().Lookup("lean"); f != nil {
		t.Error("--lean must be Kimi-only; a target without a flags hook must not get it")
	}
}

// TestRunSetupTargetPrintsTheNote pins the other half of the seam: the note hook
// fires on BOTH of runSetupTarget's exits. The already-registered path is the
// one that matters — a repeat `plumb setup kimi-code --lean` writes nothing, so
// the note is the only confirmation the user gets that the allowlist is in
// place. A nil hook must stay silent.
func TestRunSetupTargetPrintsTheNote(t *testing.T) {
	const marker = "::note-hook-fired::"
	path := filepath.Join(t.TempDir(), "mcp.json")
	target := setupTarget{
		use:    "kimi-code",
		name:   "Test Client",
		pathFn: func() (string, error) { return path, nil },
		intoFn: func(cfgPath, plumbBin string) (bool, []string, error) {
			return kimiCodeInto(cfgPath, plumbBin, true)
		},
		note: func() string { return marker },
	}

	run := func() string {
		return captureStdout(t, func() {
			if err := runSetupTarget(target); err != nil {
				t.Errorf("runSetupTarget: %v", err)
			}
		})
	}

	if out := run(); !strings.Contains(out, marker) {
		t.Errorf("the note must print after a fresh registration:\n%s", out)
	}

	out := run()
	if !strings.Contains(out, "already registered") {
		t.Fatalf("expected the second run to take the already-registered path:\n%s", out)
	}
	if !strings.Contains(out, marker) {
		t.Errorf("the note must print on the already-registered path too:\n%s", out)
	}

	target.note = nil
	if out := run(); strings.Contains(out, marker) {
		t.Errorf("a target with no note hook must print nothing extra:\n%s", out)
	}
}

// TestKimiLeanNote pins the hint to its trigger and its content: silent without
// --lean, and when it fires it must name the refresh command and the tool count,
// because a stale snapshot is the allowlist's only failure mode.
func TestKimiLeanNote(t *testing.T) {
	t.Cleanup(func() { setupKimiLeanFlag = false })

	setupKimiLeanFlag = false
	if got := kimiLeanNote(); got != "" {
		t.Errorf("note without --lean = %q, want empty", got)
	}

	setupKimiLeanFlag = true
	got := kimiLeanNote()
	for _, want := range []string{"enabledTools", "plumb setup kimi-code --lean", "snapshot"} {
		if !strings.Contains(got, want) {
			t.Errorf("note missing %q: %q", want, got)
		}
	}
	if !strings.Contains(got, strconv.Itoa(len(tools.LeanToolNames()))) {
		t.Errorf("note should state the pinned tool count: %q", got)
	}
}

// TestKimiLeanHintAt covers every state the doctor pass can see. The
// no-allowlist case must stay informational (ok, no warn, no fix): a full
// registration is a valid default, so a "!" there would put an unhealthy marker
// on a healthy machine. The degenerate cases are the opposite — an enabledTools
// value that is present but unusable means Kimi loads zero plumb tools, so a
// check keyed on key PRESENCE alone would report a dead integration as healthy.
func TestKimiLeanHintAt(t *testing.T) {
	const bin = "/usr/local/bin/plumb"

	t.Run("registered without an allowlist fires an informational pass", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "mcp.json")
		if _, _, err := kimiCodeInto(path, bin, false); err != nil {
			t.Fatalf("bare register: %v", err)
		}
		res, ok := kimiLeanHintAt(path)
		if !ok {
			t.Fatal("expected the hint to fire for a registration with no enabledTools")
		}
		if !res.ok || res.warn {
			t.Errorf("hint must be informational: ok=%v warn=%v", res.ok, res.warn)
		}
		if res.fix != "" {
			t.Errorf("an informational pass must carry no fix line, got %q", res.fix)
		}
		if !strings.Contains(res.detail, "plumb setup kimi-code --lean") {
			t.Errorf("detail should name the command: %q", res.detail)
		}
	})

	t.Run("allowlist present stays silent", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "mcp.json")
		if _, _, err := kimiCodeInto(path, bin, true); err != nil {
			t.Fatalf("lean register: %v", err)
		}
		if _, ok := kimiLeanHintAt(path); ok {
			t.Error("the hint must not fire once enabledTools is set")
		}
	})

	t.Run("config without a plumb entry stays silent", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "mcp.json")
		seed := map[string]any{"mcpServers": map[string]any{"other": map[string]any{"command": "/bin/other"}}}
		if err := writeJSON(path, seed); err != nil {
			t.Fatalf("seeding config: %v", err)
		}
		if _, ok := kimiLeanHintAt(path); ok {
			t.Error("the hint must not fire when plumb is not registered")
		}
	})

	t.Run("absent config stays silent and is not created", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "nested", "mcp.json")
		if _, ok := kimiLeanHintAt(path); ok {
			t.Error("the hint must not fire for an absent config")
		}
		if _, err := os.Stat(filepath.Join(dir, "nested")); !os.IsNotExist(err) {
			t.Error("a doctor check must not create directories while inspecting")
		}
	})

	// A hand-edited enabledTools that no tool name can pass. Each of these is a
	// config Kimi accepts and plumb never writes, and each leaves the plumb
	// server connected with nothing callable.
	for _, tc := range []struct {
		name      string
		allowlist any
		shape     string
	}{
		{"empty list", []any{}, "an empty list"},
		{"null", nil, "null"},
		{"wrong type", "read_file", "not a list"},
	} {
		t.Run("degenerate allowlist warns: "+tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "mcp.json")
			writeKimiAllowlist(t, path, bin, tc.allowlist)

			res, ok := kimiLeanHintAt(path)
			if !ok {
				t.Fatalf("enabledTools (%s) disables every plumb tool — doctor must not stay silent", tc.name)
			}
			if !res.warn {
				t.Errorf("a degenerate allowlist is a misconfiguration, not a preference — want warn=true, got %+v", res)
			}
			if !res.ok {
				t.Error("it must stay non-fatal (ok=true) — doctor's exit code is for plumb being broken")
			}
			if res.fix == "" {
				t.Error("a warning must carry a fix line — it is the only part that renders on attention")
			}
			if !strings.Contains(res.detail, tc.shape) {
				t.Errorf("detail should name the shape %q: %q", tc.shape, res.detail)
			}
		})
	}

	t.Run("a non-empty hand-edited allowlist stays silent", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "mcp.json")
		writeKimiAllowlist(t, path, bin, []any{"read_file", "edit_file"})
		if _, ok := kimiLeanHintAt(path); ok {
			t.Error("a working allowlist is the user's business — doctor has nothing to say")
		}
	})
}

// writeKimiAllowlist writes a Kimi-shaped mcp.json whose plumb entry carries the
// given enabledTools value verbatim — including shapes kimiCodeInto would never
// produce, which is exactly what the doctor check has to survive.
func writeKimiAllowlist(t *testing.T, path, bin string, allowlist any) {
	t.Helper()
	cfg := map[string]any{"mcpServers": map[string]any{"plumb": map[string]any{
		"command":      bin,
		"args":         []string{"serve"},
		"enabledTools": allowlist,
	}}}
	if err := writeJSON(path, cfg); err != nil {
		t.Fatalf("writing config: %v", err)
	}
}
