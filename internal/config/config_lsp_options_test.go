package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadProject_LSPInitializationOptions verifies a [lsp.<lang>.initialization_options]
// table is parsed into the free-form map verbatim, with typed TOML scalars preserved
// and the rest of the language's config (command) still inherited from base.
func TestLoadProject_LSPInitializationOptions(t *testing.T) {
	ws := t.TempDir()
	plumbDir := filepath.Join(ws, ".plumb")
	if err := os.MkdirAll(plumbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `[lsp.zig.initialization_options]
enable_build_on_save = true
build_on_save_step = "check"
`
	if err := os.WriteFile(filepath.Join(plumbDir, "config.toml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := LoadProject(defaults, ws)
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	zig := got.LSP["zig"]
	// Base command must survive the partial [lsp.zig] override.
	if zig.Command != "zls" {
		t.Errorf("zig command = %q, want inherited \"zls\"", zig.Command)
	}
	opts := zig.InitializationOptions
	if opts == nil {
		t.Fatal("InitializationOptions is nil, want the configured table")
	}
	// go-toml/v2 decodes a bare `true` as bool and a quoted value as string.
	if v, ok := opts["enable_build_on_save"].(bool); !ok || !v {
		t.Errorf("enable_build_on_save = %#v, want bool true", opts["enable_build_on_save"])
	}
	if v, ok := opts["build_on_save_step"].(string); !ok || v != "check" {
		t.Errorf("build_on_save_step = %#v, want \"check\"", opts["build_on_save_step"])
	}
}

// TestLoadProject_LSPInitializationOptions_AbsentIsNil verifies that a language
// config with no initialization_options table leaves the field nil, so nothing
// is sent to the server (the byte-identical default).
func TestLoadProject_LSPInitializationOptions_AbsentIsNil(t *testing.T) {
	ws := t.TempDir()
	plumbDir := filepath.Join(ws, ".plumb")
	if err := os.MkdirAll(plumbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A [lsp.zig] table that sets an unrelated field, but no initialization_options.
	cfg := "[lsp.zig]\nenabled = true\n"
	if err := os.WriteFile(filepath.Join(plumbDir, "config.toml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := LoadProject(defaults, ws)
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	if got.LSP["zig"].InitializationOptions != nil {
		t.Errorf("InitializationOptions = %#v, want nil when unconfigured", got.LSP["zig"].InitializationOptions)
	}
	// Defaults carry no options for any language either.
	for name, lsp := range Defaults().LSP {
		if lsp.InitializationOptions != nil {
			t.Errorf("default [lsp.%s] has InitializationOptions %#v, want nil", name, lsp.InitializationOptions)
		}
	}
}

// TestCloneLSPConfig_InitializationOptions guards that cloneLSPConfig deep-copies
// the free-form options map so a cloned config never aliases the source, and
// preserves nil (so cloneConfig(defaults) stays DeepEqual to defaults).
func TestCloneLSPConfig_InitializationOptions(t *testing.T) {
	src := LSPConfig{
		Command:               "zls",
		InitializationOptions: map[string]any{"enable_build_on_save": true},
	}
	clone := cloneLSPConfig(src)
	clone.InitializationOptions["build_on_save_step"] = "check"
	if _, ok := src.InitializationOptions["build_on_save_step"]; ok {
		t.Error("cloneLSPConfig shared InitializationOptions with source (mutation leaked back)")
	}

	// nil in, nil out.
	if got := cloneLSPConfig(LSPConfig{Command: "zls"}).InitializationOptions; got != nil {
		t.Errorf("cloneLSPConfig of a nil-options config = %#v, want nil", got)
	}
}
