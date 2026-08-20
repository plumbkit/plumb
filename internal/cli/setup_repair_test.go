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

// newRefreshCodexTarget builds a codex setupTarget pointed at cfgPath, shared
// by every TestRefreshClient scenario below.
func newRefreshCodexTarget(cfgPath string) setupTarget {
	return setupTarget{
		use:       "codex",
		name:      "Codex",
		pathFn:    func() (string, error) { return cfgPath, nil },
		intoFn:    setupCodexInto,
		extractFn: mapCommandExtractor(readOrInitCodexConfig, "mcp_servers", "command"),
	}
}

// requireClientRow asserts the single-row shape refreshClient returns for
// these scenarios (a codex target always resolves exactly one config path).
func requireClientRow(t *testing.T, rows []clientRow, changed bool, wantStatus string, wantChanged bool) {
	t.Helper()
	if changed != wantChanged || len(rows) != 1 || rows[0].status != wantStatus {
		t.Errorf("got (%+v, %v), want status %q, changed %v", rows, changed, wantStatus, wantChanged)
	}
}

func testRefreshClientNotInstalled(t *testing.T) {
	c := newRefreshCodexTarget(filepath.Join(t.TempDir(), "config.toml")) // never created
	rows, changed := refreshClient(c, "/new/plumb", false)
	requireClientRow(t, rows, changed, "not installed", false)
}

func testRefreshClientNotInstalledInstallMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml") // never created
	rows, changed := refreshClient(newRefreshCodexTarget(path), "/new/plumb", true)
	requireClientRow(t, rows, changed, "not installed", false)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("install-missing must not fabricate a config for an absent client")
	}
}

func testRefreshClientNotRegistered(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[mcp_servers.other]\ncommand = \"x\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rows, changed := refreshClient(newRefreshCodexTarget(path), "/new/plumb", false)
	requireClientRow(t, rows, changed, "not registered", false)
}

func testRefreshClientInstallMissingRegisters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[mcp_servers.other]\ncommand = \"x\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rows, changed := refreshClient(newRefreshCodexTarget(path), "/new/plumb", true)
	requireClientRow(t, rows, changed, "registered", true)

	bin, ok, err := mapCommandExtractor(readOrInitCodexConfig, "mcp_servers", "command")(path)
	if err != nil || !ok || bin != "/new/plumb" {
		t.Errorf("plumb not registered: got %q ok=%v (err %v)", bin, ok, err)
	}
	// Pre-existing server is preserved.
	cfg, _, err := readOrInitCodexConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg["mcp_servers"].(map[string]any)["other"]; !ok {
		t.Error("existing mcp server dropped on install-missing register")
	}

	// Second pass is a no-op.
	rows, changed = refreshClient(newRefreshCodexTarget(path), "/new/plumb", true)
	requireClientRow(t, rows, changed, "already current", false)
}

func testRefreshClientStaleBinaryRepointed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if _, _, err := setupCodexInto(path, "/old/plumb"); err != nil {
		t.Fatal(err)
	}
	rows, changed := refreshClient(newRefreshCodexTarget(path), "/new/plumb", false)
	requireClientRow(t, rows, changed, "updated", true)

	bin, _, err := mapCommandExtractor(readOrInitCodexConfig, "mcp_servers", "command")(path)
	if err != nil || bin != "/new/plumb" {
		t.Errorf("binary not repointed: got %q (err %v)", bin, err)
	}

	// Second pass is a no-op.
	rows, changed = refreshClient(newRefreshCodexTarget(path), "/new/plumb", false)
	requireClientRow(t, rows, changed, "already current", false)
}

// newRefreshKimiTarget builds a Kimi Code setupTarget pointed at cfgPath with a
// controllable install detector, for the absent-config scenarios below. Kimi
// Code's mcp.json only exists once an MCP server is configured, so detection
// rides installedFn rather than the config file's presence.
func newRefreshKimiTarget(cfgPath string, installed bool) setupTarget {
	return setupTarget{
		use:         "kimi-code",
		name:        "Kimi Code",
		pathFn:      func() (string, error) { return cfgPath, nil },
		installedFn: func() bool { return installed },
		intoFn:      setupClaudeDesktopInto,
		extractFn:   claudeDesktopCommandExtractor,
	}
}

func testRefreshClientKimiDetectedButUnregistered(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json") // never created
	rows, changed := refreshClient(newRefreshKimiTarget(path, true), "/new/plumb", false)
	requireClientRow(t, rows, changed, "not registered", false)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("bare --all must not create a config for an unregistered client")
	}
}

func testRefreshClientKimiInstallMissingCreatesConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json") // never created
	rows, changed := refreshClient(newRefreshKimiTarget(path, true), "/new/plumb", true)
	requireClientRow(t, rows, changed, "registered", true)

	bin, ok, err := claudeDesktopCommandExtractor(path)
	if err != nil || !ok || bin != "/new/plumb" {
		t.Errorf("plumb not registered in created config: got %q ok=%v (err %v)", bin, ok, err)
	}

	// Second pass is a no-op.
	rows, changed = refreshClient(newRefreshKimiTarget(path, true), "/new/plumb", true)
	requireClientRow(t, rows, changed, "already current", false)
}

func testRefreshClientKimiDetectorNegative(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json") // never created
	rows, changed := refreshClient(newRefreshKimiTarget(path, false), "/new/plumb", true)
	requireClientRow(t, rows, changed, "not installed", false)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("install-missing must not fabricate a config when the detector says not installed")
	}
}

func TestRefreshClient(t *testing.T) {
	t.Run("not installed is skipped", testRefreshClientNotInstalled)
	t.Run("not installed stays untouched even with install-missing", testRefreshClientNotInstalledInstallMissing)
	t.Run("plumb not registered is skipped without install-missing", testRefreshClientNotRegistered)
	t.Run("install-missing registers a config-present client", testRefreshClientInstallMissingRegisters)
	t.Run("stale binary is repointed", testRefreshClientStaleBinaryRepointed)
	t.Run("kimi detected via data dir reports not registered without install-missing", testRefreshClientKimiDetectedButUnregistered)
	t.Run("kimi install-missing creates the absent config", testRefreshClientKimiInstallMissingCreatesConfig)
	t.Run("kimi detector negative stays not installed", testRefreshClientKimiDetectorNegative)
}

func TestKimiCodeInstalled(t *testing.T) {
	t.Run("data dir present", func(t *testing.T) {
		t.Setenv("KIMI_CODE_HOME", t.TempDir())
		if !kimiCodeInstalled() {
			t.Error("expected installed=true when the data dir exists")
		}
	})
	t.Run("data dir absent", func(t *testing.T) {
		t.Setenv("KIMI_CODE_HOME", filepath.Join(t.TempDir(), "nope"))
		if kimiCodeInstalled() {
			t.Error("expected installed=false when the data dir does not exist")
		}
	})
	t.Run("data path is a file", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "kimi-code")
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("KIMI_CODE_HOME", f)
		if kimiCodeInstalled() {
			t.Error("expected installed=false when the data path is a file, not a dir")
		}
	})
}

// TestCheckOneClientKimiInstalledNoConfig pins the doctor side of the Kimi Code
// detection: an absent mcp.json with the data dir present is "not registered"
// with a setup fix, not "not installed".
func TestCheckOneClientKimiInstalledNoConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json") // never created
	c := newRefreshKimiTarget(path, true)
	res := checkOneClient(c, "")
	if res.ok {
		t.Error("expected ok=false for an installed-but-unregistered client")
	}
	if res.detail != "installed, but plumb is not registered (no config yet)" {
		t.Errorf("unexpected detail: %q", res.detail)
	}
	if res.fix != "run `plumb setup kimi-code`" {
		t.Errorf("unexpected fix: %q", res.fix)
	}

	res = checkOneClient(newRefreshKimiTarget(path, false), "")
	if res.ok || res.detail != "not installed or config not found" {
		t.Errorf("detector-negative client should report not installed: got %+v", res)
	}
}

func TestKimiWorkInstalled(t *testing.T) {
	t.Run("kernel home present", func(t *testing.T) {
		t.Setenv("KIMI_WORK_HOME", t.TempDir())
		if !kimiWorkInstalled() {
			t.Error("expected installed=true when the kernel home exists")
		}
	})
	t.Run("kernel home absent", func(t *testing.T) {
		t.Setenv("KIMI_WORK_HOME", filepath.Join(t.TempDir(), "nope"))
		if kimiWorkInstalled() {
			t.Error("expected installed=false when the kernel home does not exist")
		}
	})
	t.Run("kernel home path is a file", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "kimi-work")
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("KIMI_WORK_HOME", f)
		if kimiWorkInstalled() {
			t.Error("expected installed=false when the kernel home path is a file, not a dir")
		}
	})
}

// TestCheckOneClientKimiWorkFixNamesKimiWork pins that the desktop target's
// doctor fix points at `plumb setup kimi-work`, not the CLI's subcommand — the
// two registrations are independent, so sending the user to kimi-code would
// leave the app unregistered.
func TestCheckOneClientKimiWorkFixNamesKimiWork(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json") // never created
	c := newRefreshKimiTarget(path, true)
	c.use = "kimi-work"
	c.name = "Kimi Work"
	res := checkOneClient(c, "")
	if res.ok {
		t.Error("expected ok=false for an installed-but-unregistered client")
	}
	if res.fix != "run `plumb setup kimi-work`" {
		t.Errorf("unexpected fix: %q", res.fix)
	}
}
