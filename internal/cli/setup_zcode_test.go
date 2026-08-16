package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// ZCode nests its MCP servers one level deeper than every other JSON client —
// mcp.servers.<name> in ~/.zcode/cli/config.json — and enforces a strict server
// schema: an unknown key silently drops the whole server, and an argv-array
// command crashes the desktop Settings → MCP page (ZCode's own bundled
// diagnosing-mcp guide documents both). These tests therefore pin the nested
// shape AND the exact entry keys: a plausibly-shaped entry with one extra key
// would still be a silent failure inside ZCode.

func TestSetupZCodeInto_FreshConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")

	added, preserved, err := setupZCodeInto(path, "/usr/local/bin/plumb")
	if err != nil {
		t.Fatalf("setupZCodeInto: %v", err)
	}
	if !added {
		t.Error("expected added=true for fresh config")
	}
	if len(preserved) != 0 {
		t.Errorf("expected no preserved servers, got %v", preserved)
	}

	cfg, _, err := readOrInitClaudeConfig(path)
	if err != nil {
		t.Fatalf("reading result: %v", err)
	}
	mcp, ok := cfg["mcp"].(map[string]any)
	if !ok {
		t.Fatalf("mcp is not an object: %T", cfg["mcp"])
	}
	plumb, ok := mcp["servers"].(map[string]any)["plumb"].(map[string]any)
	if !ok {
		t.Fatalf("mcp.servers.plumb is not an object: %T", mcp["servers"])
	}
	if plumb["type"] != "stdio" {
		t.Errorf("type: got %v, want stdio", plumb["type"])
	}
	// A string command, never an argv array — ZCode's desktop Settings page
	// renders an array command with `command.trim is not a function`.
	if plumb["command"] != "/usr/local/bin/plumb" {
		t.Errorf("command: got %v, want the binary as a plain string", plumb["command"])
	}
	if !reflect.DeepEqual(plumb["args"], []any{"serve"}) {
		t.Errorf("args: got %v, want [serve]", plumb["args"])
	}
	// The schema is strict: any key beyond type/command/args ZCode documents
	// for stdio would drop the server, so the fresh entry must be exactly these.
	if len(plumb) != 3 {
		t.Errorf("fresh entry must carry exactly type, command, args — got %d keys: %v", len(plumb), plumb)
	}
}

func TestSetupZCodeInto_PreservesSiblingsAndOtherResources(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	// ZCode keeps hooks and plugin state in the same file, and other servers
	// under mcp.servers — a registration that dropped any of them would break
	// the user's client, not just plumb's entry.
	existing := `{
	  "hooks": {"enabled": true},
	  "plugins": {"marketplace": {"enabled": true}},
	  "mcp": {"servers": {"other": {"type": "stdio", "command": "other-bin", "args": []}}}
	}`
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	added, preserved, err := setupZCodeInto(path, "/usr/local/bin/plumb")
	if err != nil {
		t.Fatalf("setupZCodeInto: %v", err)
	}
	if !added {
		t.Error("expected added=true")
	}
	if len(preserved) != 1 || preserved[0] != "other" {
		t.Errorf("expected preserved=[other], got %v", preserved)
	}

	cfg, _, err := readOrInitClaudeConfig(path)
	if err != nil {
		t.Fatalf("re-reading config: %v", err)
	}
	servers := cfg["mcp"].(map[string]any)["servers"].(map[string]any)
	if servers["other"] == nil {
		t.Error("pre-existing 'other' server was removed")
	}
	if _, ok := cfg["hooks"]; !ok {
		t.Error("top-level hooks key was removed")
	}
	if _, ok := cfg["plugins"]; !ok {
		t.Error("top-level plugins key was removed")
	}

	entries, _ := os.ReadDir(dir)
	var backups int
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".bak" {
			backups++
		}
	}
	if backups == 0 {
		t.Error("expected a .bak backup before modifying existing config")
	}
}

func TestSetupZCodeInto_Idempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	if _, _, err := setupZCodeInto(path, "/usr/local/bin/plumb"); err != nil {
		t.Fatalf("first run: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	added, _, err := setupZCodeInto(path, "/usr/local/bin/plumb")
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if added {
		t.Error("expected added=false on second run (already registered)")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("an already-current registration must not rewrite the file")
	}
}

func TestSetupZCodeInto_RepointsStaleEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	stale := `{"mcp": {"servers": {"plumb": {"type": "stdio", "command": "/old/plumb", "args": ["serve"]}}}}`
	if err := os.WriteFile(path, []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}

	added, _, err := setupZCodeInto(path, "/usr/local/bin/plumb")
	if err != nil {
		t.Fatalf("setupZCodeInto: %v", err)
	}
	if !added {
		t.Error("expected added=true when repointing a stale entry")
	}

	cfg, _, err := readOrInitClaudeConfig(path)
	if err != nil {
		t.Fatalf("re-reading config: %v", err)
	}
	plumb := cfg["mcp"].(map[string]any)["servers"].(map[string]any)["plumb"].(map[string]any)
	if plumb["command"] != "/usr/local/bin/plumb" {
		t.Errorf("command not repointed: got %v", plumb["command"])
	}
}

func TestSetupZCodeInto_NonObjectMCPRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	broken := `{"mcp": "not an object"}`
	if err := os.WriteFile(path, []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, err := setupZCodeInto(path, "/usr/local/bin/plumb")
	if err == nil {
		t.Fatal("a non-object mcp key must be refused, not clobbered")
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(after) != broken {
		t.Error("the refused write must leave the file untouched")
	}
}

func TestZCodeCommandExtractor(t *testing.T) {
	t.Run("reads the nested command back", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.json")
		if _, _, err := setupZCodeInto(path, "/usr/local/bin/plumb"); err != nil {
			t.Fatal(err)
		}
		bin, registered, err := zcodeCommandExtractor(path)
		if err != nil {
			t.Fatalf("extractor: %v", err)
		}
		if !registered || bin != "/usr/local/bin/plumb" {
			t.Errorf("got (%q, %v), want (/usr/local/bin/plumb, true)", bin, registered)
		}
	})
	t.Run("unregistered config reports false", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.json")
		if _, _, err := setupZCodeInto(path, "/usr/local/bin/plumb"); err != nil {
			t.Fatal(err)
		}
		cfg, _, err := readOrInitClaudeConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		delete(cfg["mcp"].(map[string]any)["servers"].(map[string]any), "plumb")
		if err := writeJSON(path, cfg); err != nil {
			t.Fatal(err)
		}
		if _, registered, err := zcodeCommandExtractor(path); err != nil || registered {
			t.Errorf("got registered=%v err=%v, want false nil", registered, err)
		}
	})
	t.Run("unparseable config surfaces the error", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(`{"mcp": `), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := zcodeCommandExtractor(path); err == nil {
			t.Error("a config that cannot be parsed must fail the doctor check, not pass it")
		}
	})
}

func TestZCodeInstalled_DirPresence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if zcodeInstalled() {
		t.Error("~/.zcode absent must report not installed")
	}
	// ZCode's desktop app creates ~/.zcode on first run; cli/config.json only
	// appears once something is configured there, so the dir is the presence
	// signal --all can rely on.
	if err := os.MkdirAll(filepath.Join(home, ".zcode"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !zcodeInstalled() {
		t.Error("~/.zcode present must report installed")
	}
}

// TestZCodeTargetWiring pins the shipped target's wiring: the entry must carry
// the installed-dir probe (so --all creates the config fresh) and the skills
// resolver (so `plumb skills sync` covers ZCode) — either can silently regress
// to a bare entry and only the bulk paths would notice.
func TestZCodeTargetWiring(t *testing.T) {
	var target *setupTarget
	for i, c := range allSetupClients() {
		if c.use == "zcode" {
			target = &allSetupClients()[i]
			break
		}
	}
	if target == nil {
		t.Fatal("no zcode entry in allSetupClients()")
	}
	if target.name != "ZCode" {
		t.Errorf("name: got %q, want ZCode", target.name)
	}
	if target.installedFn == nil {
		t.Error("zcode must set installedFn — its config file only exists once a server is configured")
	}
	if target.skillsDirFn == nil {
		t.Error("zcode must set skillsDirFn — it is a verified SKILL.md client")
	}
	if target.intoFn == nil || target.extractFn == nil {
		t.Error("zcode must set intoFn and extractFn")
	}
}
