package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// dshPatchText reads a patch file back as raw text for assertions.
func dshPatchText(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(data)
}

func TestDSHConfigPath(t *testing.T) {
	t.Run("default home", func(t *testing.T) {
		t.Setenv("DSH_HOME", "")
		path, err := DSHConfigPath()
		if err != nil {
			t.Fatalf("DSHConfigPath: %v", err)
		}
		if filepath.Base(path) != "cordis.patch.yml" {
			t.Errorf("base: got %q, want cordis.patch.yml", filepath.Base(path))
		}
		if filepath.Base(filepath.Dir(path)) != ".dsh" {
			t.Errorf("parent dir: got %q, want .dsh", filepath.Base(filepath.Dir(path)))
		}
	})
	t.Run("DSH_HOME override", func(t *testing.T) {
		t.Setenv("DSH_HOME", "/custom/dsh-home")
		path, err := DSHConfigPath()
		if err != nil {
			t.Fatalf("DSHConfigPath: %v", err)
		}
		want := filepath.Join("/custom/dsh-home", "cordis.patch.yml")
		if path != want {
			t.Errorf("path: got %q, want %q", path, want)
		}
	})
	t.Run("blank DSH_HOME treated as unset", func(t *testing.T) {
		t.Setenv("DSH_HOME", "   ")
		path, err := DSHConfigPath()
		if err != nil {
			t.Fatalf("DSHConfigPath: %v", err)
		}
		if filepath.Base(filepath.Dir(path)) != ".dsh" {
			t.Errorf("blank DSH_HOME should fall back to ~/.dsh, got %q", path)
		}
	})
}

func TestDshInstalled(t *testing.T) {
	t.Run("home dir present", func(t *testing.T) {
		t.Setenv("DSH_HOME", t.TempDir())
		if !dshInstalled() {
			t.Error("expected installed=true when the home dir exists")
		}
	})
	t.Run("home dir absent", func(t *testing.T) {
		t.Setenv("DSH_HOME", filepath.Join(t.TempDir(), "nope"))
		if dshInstalled() {
			t.Error("expected installed=false when the home dir does not exist")
		}
	})
	t.Run("home path is a file", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "dsh-home")
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("DSH_HOME", f)
		if dshInstalled() {
			t.Error("expected installed=false when the home path is a file, not a dir")
		}
	})
}

func TestSetupDSHInto_FreshConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cordis.patch.yml")

	added, preserved, err := setupDSHInto(path, "/usr/local/bin/plumb")
	if err != nil {
		t.Fatalf("setupDSHInto: %v", err)
	}
	if !added {
		t.Error("expected added=true for fresh config")
	}
	if len(preserved) != 0 {
		t.Errorf("expected no preserved entries, got %v", preserved)
	}

	bin, ok, err := dshCommandExtractor(path)
	if err != nil || !ok || bin != "/usr/local/bin/plumb" {
		t.Errorf("extractor: got %q ok=%v (err %v)", bin, ok, err)
	}

	text := dshPatchText(t, path)
	for _, want := range []string{
		"- insert:",
		"- id: mcp-plumb",
		"name: '@deepseek-ai/dsh-mcp-client'",
		"serverName: plumb",
		"transport: stdio",
		"command: /usr/local/bin/plumb",
		"args:",
		"- serve",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("written patch missing %q:\n%s", want, text)
		}
	}
}

func TestSetupDSHInto_Idempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cordis.patch.yml")

	if _, _, err := setupDSHInto(path, "/usr/local/bin/plumb"); err != nil {
		t.Fatalf("first run: %v", err)
	}
	added, _, err := setupDSHInto(path, "/usr/local/bin/plumb")
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if added {
		t.Error("expected added=false on second run (already registered)")
	}
	// No backup should be produced by an idempotent re-run.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".bak" {
			t.Errorf("idempotent re-run must not back up: found %s", e.Name())
		}
	}
}

func TestSetupDSHInto_RepointsStaleBinary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cordis.patch.yml")

	if _, _, err := setupDSHInto(path, "/old/plumb"); err != nil {
		t.Fatal(err)
	}
	added, _, err := setupDSHInto(path, "/new/plumb")
	if err != nil {
		t.Fatalf("setupDSHInto: %v", err)
	}
	if !added {
		t.Error("expected added=true when repointing the binary")
	}
	bin, _, err := dshCommandExtractor(path)
	if err != nil || bin != "/new/plumb" {
		t.Errorf("binary not repointed: got %q (err %v)", bin, err)
	}
}

func TestSetupDSHInto_PreservesUnrelatedEntriesAndComments(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cordis.patch.yml")

	existing := "# machine-local preferences\n" +
		"- id: mcp-github\n" +
		"  name: '@deepseek-ai/dsh-mcp-client'\n" +
		"  config:\n" +
		"    serverName: github\n" +
		"    transport: stdio\n" +
		"    command: npx\n" +
		"    args: ['-y', '@modelcontextprotocol/server-github']\n" +
		"    env:\n" +
		"      GITHUB_TOKEN: !!js process.env.GITHUB_TOKEN\n"
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	added, _, err := setupDSHInto(path, "/usr/local/bin/plumb")
	if err != nil {
		t.Fatalf("setupDSHInto: %v", err)
	}
	if !added {
		t.Error("expected added=true")
	}

	text := dshPatchText(t, path)
	if !strings.Contains(text, "# machine-local preferences") {
		t.Errorf("header comment lost:\n%s", text)
	}
	if !strings.Contains(text, "!!js process.env.GITHUB_TOKEN") {
		t.Errorf("!!js expression lost:\n%s", text)
	}
	if !strings.Contains(text, "mcp-github") {
		t.Errorf("unrelated mcp-github entry lost:\n%s", text)
	}
	if !strings.Contains(text, "mcp-plumb") {
		t.Errorf("plumb row missing:\n%s", text)
	}

	entries, _ := os.ReadDir(dir)
	var backups int
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".bak" {
			backups++
		}
	}
	if backups == 0 {
		t.Error("expected a .bak backup before modifying an existing config")
	}
}

func TestSetupDSHInto_NotAListErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cordis.patch.yml")
	if err := os.WriteFile(path, []byte("settings:\n  foo: bar\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, _, err := setupDSHInto(path, "/usr/local/bin/plumb"); err == nil {
		t.Fatal("expected an error for a non-list patch file")
	}
	// The non-list file must be left untouched.
	if text := dshPatchText(t, path); !strings.Contains(text, "settings:") {
		t.Errorf("non-list config was overwritten:\n%s", text)
	}
}

func TestSetupDSHInto_RepointPreservesExtraKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cordis.patch.yml")

	existing := "- insert:\n" +
		"    - id: mcp-plumb\n" +
		"      name: '@deepseek-ai/dsh-mcp-client'\n" +
		"      config:\n" +
		"        serverName: plumb\n" +
		"        transport: stdio\n" +
		"        command: /old/plumb\n" +
		"        args: ['serve']\n" +
		"        toolCallTimeoutMs: 90000\n"
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, _, err := setupDSHInto(path, "/new/plumb"); err != nil {
		t.Fatalf("setupDSHInto: %v", err)
	}

	text := dshPatchText(t, path)
	if !strings.Contains(text, "toolCallTimeoutMs: 90000") {
		t.Errorf("user-added key lost on repoint:\n%s", text)
	}
	if !strings.Contains(text, "command: /new/plumb") {
		t.Errorf("command not repointed:\n%s", text)
	}
}

func TestDSHCommandExtractor(t *testing.T) {
	dir := t.TempDir()
	t.Run("absent file", func(t *testing.T) {
		if _, ok, err := dshCommandExtractor(filepath.Join(dir, "nope.yml")); ok || err == nil {
			t.Errorf("absent file: got ok=%v err=%v, want ok=false with error", ok, err)
		}
	})
	t.Run("no plumb row", func(t *testing.T) {
		path := filepath.Join(dir, "cordis.patch.yml")
		if err := os.WriteFile(path, []byte("- id: mcp-github\n  name: x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, ok, err := dshCommandExtractor(path); ok || err != nil {
			t.Errorf("no plumb row: got ok=%v err=%v, want ok=false", ok, err)
		}
	})
	t.Run("empty command", func(t *testing.T) {
		path := filepath.Join(dir, "cordis2.yml")
		if err := os.WriteFile(path, []byte("- insert:\n    - id: mcp-plumb\n      name: x\n      config:\n        command: \"\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, ok, err := dshCommandExtractor(path); ok || err != nil {
			t.Errorf("empty command: got ok=%v err=%v, want ok=false", ok, err)
		}
	})
	t.Run("bare row is inert", func(t *testing.T) {
		path := filepath.Join(dir, "cordis3.yml")
		if err := os.WriteFile(path, []byte("- id: mcp-plumb\n  name: x\n  config:\n    command: /bare/plumb\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, ok, err := dshCommandExtractor(path); ok || err != nil {
			t.Errorf("bare id row must not count as registered: got ok=%v err=%v", ok, err)
		}
	})
}

// newRefreshDSHTarget builds a dsh setupTarget pointed at cfgPath with a
// controllable install detector, for the absent-config scenarios below. dsh's
// home patch only exists once a patch entry is configured, so detection rides
// installedFn rather than the config file's presence — mirroring Kimi Code.
func newRefreshDSHTarget(cfgPath string, installed bool) setupTarget {
	return setupTarget{
		use:         "dsh",
		name:        "DeepSeek Harness",
		pathFn:      func() (string, error) { return cfgPath, nil },
		installedFn: func() bool { return installed },
		intoFn:      setupDSHInto,
		extractFn:   dshCommandExtractor,
	}
}

func TestRefreshClientDSH(t *testing.T) {
	t.Run("not installed is skipped even with install-missing", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "cordis.patch.yml")
		rows, changed := refreshClient(newRefreshDSHTarget(path, false), "/new/plumb", true)
		requireClientRow(t, rows, changed, "not installed", false)
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Error("install-missing must not fabricate a config when the detector says not installed")
		}
	})
	t.Run("installed but unregistered without install-missing", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "cordis.patch.yml") // never created
		rows, changed := refreshClient(newRefreshDSHTarget(path, true), "/new/plumb", false)
		requireClientRow(t, rows, changed, "not registered", false)
	})
	t.Run("install-missing creates the absent config", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "cordis.patch.yml") // never created
		rows, changed := refreshClient(newRefreshDSHTarget(path, true), "/new/plumb", true)
		requireClientRow(t, rows, changed, "registered", true)

		bin, ok, err := dshCommandExtractor(path)
		if err != nil || !ok || bin != "/new/plumb" {
			t.Errorf("plumb not registered in created config: got %q ok=%v (err %v)", bin, ok, err)
		}

		// Second pass is a no-op.
		rows, changed = refreshClient(newRefreshDSHTarget(path, true), "/new/plumb", true)
		requireClientRow(t, rows, changed, "already current", false)
	})
	t.Run("stale binary is repointed", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "cordis.patch.yml")
		if _, _, err := setupDSHInto(path, "/old/plumb"); err != nil {
			t.Fatal(err)
		}
		rows, changed := refreshClient(newRefreshDSHTarget(path, true), "/new/plumb", false)
		requireClientRow(t, rows, changed, "updated", true)

		bin, _, err := dshCommandExtractor(path)
		if err != nil || bin != "/new/plumb" {
			t.Errorf("binary not repointed: got %q (err %v)", bin, err)
		}
	})
}

// TestCheckOneClientDSHInstalledNoConfig pins the doctor side of the dsh
// detection: an absent home patch with the home dir present is "not
// registered" with a setup fix, not "not installed".
func TestCheckOneClientDSHInstalledNoConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cordis.patch.yml") // never created
	c := newRefreshDSHTarget(path, true)
	res := checkOneClient(c, "")
	if res.ok {
		t.Error("expected ok=false for an installed-but-unregistered client")
	}
	if res.detail != "installed, but plumb is not registered (no config yet)" {
		t.Errorf("unexpected detail: %q", res.detail)
	}
	if res.fix != "run `plumb setup dsh`" {
		t.Errorf("unexpected fix: %q", res.fix)
	}

	res = checkOneClient(newRefreshDSHTarget(path, false), "")
	if res.ok || res.detail != "not installed or config not found" {
		t.Errorf("detector-negative client should report not installed: got %+v", res)
	}
}
