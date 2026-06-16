package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCommandString(t *testing.T) {
	cases := []struct {
		name    string
		in      any
		wantBin string
		wantOK  bool
	}{
		{"string", "/usr/local/bin/plumb", "/usr/local/bin/plumb", true},
		{"argv array", []any{"/usr/local/bin/plumb", "serve"}, "/usr/local/bin/plumb", true},
		{"empty string", "", "", false},
		{"empty array", []any{}, "", false},
		{"non-string head", []any{42, "serve"}, "", false},
		{"wrong type", 42, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bin, ok := commandString(tc.in)
			if bin != tc.wantBin || ok != tc.wantOK {
				t.Errorf("commandString(%v) = (%q, %v), want (%q, %v)", tc.in, bin, ok, tc.wantBin, tc.wantOK)
			}
		})
	}
}

func TestRegisteredCommand(t *testing.T) {
	cfg := map[string]any{
		"mcpServers": map[string]any{
			"plumb": map[string]any{"command": "/opt/plumb", "args": []any{"serve"}},
		},
	}
	if bin, ok := registeredCommand(cfg, "mcpServers", "command"); !ok || bin != "/opt/plumb" {
		t.Errorf("registeredCommand = (%q, %v), want (/opt/plumb, true)", bin, ok)
	}
	// Missing plumb entry.
	empty := map[string]any{"mcpServers": map[string]any{}}
	if _, ok := registeredCommand(empty, "mcpServers", "command"); ok {
		t.Error("expected ok=false when no plumb entry present")
	}
	// Missing servers key.
	if _, ok := registeredCommand(map[string]any{}, "mcpServers", "command"); ok {
		t.Error("expected ok=false when servers key absent")
	}
}

// TestSetupCodexInto_PreservesPerToolTables guards the non-destructive merge: a
// re-register that repoints the binary must keep the user's per-tool
// [mcp_servers.plumb.tools.*] approval tables, which a wholesale replace dropped.
func TestSetupCodexInto_PreservesPerToolTables(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	existing := `[mcp_servers.plumb]
command = "/old/path/plumb"
args = ["serve"]

[mcp_servers.plumb.tools.session_start]
approval_mode = "approve"
`
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	added, _, err := setupCodexInto(path, "/new/path/plumb")
	if err != nil {
		t.Fatalf("setupCodexInto: %v", err)
	}
	if !added {
		t.Fatal("expected added=true when repointing the binary")
	}

	result, _, err := readOrInitCodexConfig(path)
	if err != nil {
		t.Fatalf("re-reading config: %v", err)
	}
	plumb := result["mcp_servers"].(map[string]any)["plumb"].(map[string]any)
	if plumb["command"] != "/new/path/plumb" {
		t.Errorf("command not repointed: got %v", plumb["command"])
	}
	tools, ok := plumb["tools"].(map[string]any)
	if !ok {
		t.Fatalf("per-tool tables dropped on merge: %#v", plumb["tools"])
	}
	sessionStart, ok := tools["session_start"].(map[string]any)
	if !ok || sessionStart["approval_mode"] != "approve" {
		t.Errorf("session_start approval table not preserved: %#v", tools["session_start"])
	}
}

func TestClassifyClientBinary(t *testing.T) {
	dir := t.TempDir()
	binA := filepath.Join(dir, "binA")
	binB := filepath.Join(dir, "binB")
	for _, p := range []string{binA, binB} {
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o700); err != nil { //nolint:gosec // test fixture
			t.Fatal(err)
		}
	}

	codex := func() setupTarget {
		return setupTarget{use: "codex", extractFn: mapCommandExtractor(readOrInitCodexConfig, "mcp_servers", "command")}
	}
	writeCodex := func(cmd string) string {
		p := filepath.Join(t.TempDir(), "config.toml")
		if _, _, err := setupCodexInto(p, cmd); err != nil {
			t.Fatal(err)
		}
		return p
	}

	t.Run("match is a clean pass", func(t *testing.T) {
		res := classifyClientBinary(codex(), writeCodex(binA), binA)
		if !res.ok || res.warn {
			t.Errorf("match: got ok=%v warn=%v, want clean pass", res.ok, res.warn)
		}
	})

	t.Run("mismatch is a warning", func(t *testing.T) {
		res := classifyClientBinary(codex(), writeCodex(binA), binB)
		if !res.ok || !res.warn {
			t.Errorf("mismatch: got ok=%v warn=%v, want ok+warn", res.ok, res.warn)
		}
		if res.fix == "" {
			t.Error("mismatch warning should carry a fix hint")
		}
	})

	t.Run("missing binary is a failure", func(t *testing.T) {
		res := classifyClientBinary(codex(), writeCodex(filepath.Join(dir, "gone")), binA)
		if res.ok {
			t.Error("missing registered binary should fail (ok=false)")
		}
		if res.fix == "" {
			t.Error("missing-binary failure should carry a fix hint")
		}
	})

	t.Run("no extractor falls back to registered pass", func(t *testing.T) {
		res := classifyClientBinary(setupTarget{use: "x"}, writeCodex(binA), binB)
		if !res.ok || res.warn {
			t.Errorf("no extractor: got ok=%v warn=%v, want clean pass", res.ok, res.warn)
		}
	})
}

func TestSameBinary(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "plumb")
	if err := os.WriteFile(target, []byte("x"), 0o700); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	link := filepath.Join(dir, "plumb-link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	if !sameBinary(target, link) {
		t.Error("a symlink and its target should resolve to the same binary")
	}
	other := filepath.Join(dir, "other")
	if err := os.WriteFile(other, []byte("y"), 0o700); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	if sameBinary(target, other) {
		t.Error("distinct binaries should not be reported equal")
	}
}

func TestExpandRegisteredPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}
	t.Setenv("PLUMB_TEST_BIN", "/opt/plumb/plumb")
	cases := map[string]string{
		"/usr/local/bin/plumb": "/usr/local/bin/plumb", // absolute unchanged
		"~/bin/plumb":          filepath.Join(home, "bin", "plumb"),
		"~":                    home,
		"$PLUMB_TEST_BIN":      "/opt/plumb/plumb",
		"${PLUMB_TEST_BIN}":    "/opt/plumb/plumb",
	}
	for in, want := range cases {
		if got := expandRegisteredPath(in); got != want {
			t.Errorf("expandRegisteredPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRefreshClient(t *testing.T) {
	newCodexTarget := func(cfgPath string) setupTarget {
		return setupTarget{
			use:       "codex",
			name:      "Codex",
			pathFn:    func() (string, error) { return cfgPath, nil },
			intoFn:    setupCodexInto,
			extractFn: mapCommandExtractor(readOrInitCodexConfig, "mcp_servers", "command"),
		}
	}

	t.Run("not installed is skipped", func(t *testing.T) {
		c := newCodexTarget(filepath.Join(t.TempDir(), "config.toml")) // never created
		status, changed := refreshClient(c, "/new/plumb")
		if changed || status != "not installed — skipped" {
			t.Errorf("got (%q, %v), want (not installed — skipped, false)", status, changed)
		}
	})

	t.Run("plumb not registered is skipped", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(path, []byte("[mcp_servers.other]\ncommand = \"x\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		status, changed := refreshClient(newCodexTarget(path), "/new/plumb")
		if changed || status != "plumb not registered — skipped" {
			t.Errorf("got (%q, %v), want (plumb not registered — skipped, false)", status, changed)
		}
	})

	t.Run("stale binary is repointed", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.toml")
		if _, _, err := setupCodexInto(path, "/old/plumb"); err != nil {
			t.Fatal(err)
		}
		status, changed := refreshClient(newCodexTarget(path), "/new/plumb")
		if !changed {
			t.Errorf("expected changed=true, got status %q", status)
		}
		bin, _, err := mapCommandExtractor(readOrInitCodexConfig, "mcp_servers", "command")(path)
		if err != nil || bin != "/new/plumb" {
			t.Errorf("binary not repointed: got %q (err %v)", bin, err)
		}

		// Second pass is a no-op.
		status, changed = refreshClient(newCodexTarget(path), "/new/plumb")
		if changed || status != "already current" {
			t.Errorf("second pass: got (%q, %v), want (already current, false)", status, changed)
		}
	})
}
