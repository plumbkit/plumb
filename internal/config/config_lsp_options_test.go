package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadProject_LSPInitializationOptions verifies a global
// [lsp.<lang>.initialization_options] table survives LoadProject verbatim, with
// typed TOML scalars preserved, and that the same table in a PROJECT config is
// ignored.
//
// This test previously asserted the opposite for the project case, and the
// example it used was the attack: zls's enable_build_on_save runs the project's
// own build.zig on every save, so honouring a cloned repository's
// initialization_options handed that repository code execution. The LSP
// initializationOptions channel is server-defined and unvalidated by plumb —
// rust-analyzer's check.overrideCommand takes a literal argv — so it belongs
// with command/args/env in the global-only set.
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

	base := cloneConfig(defaults)
	zigBase := base.LSP["zig"]
	zigBase.InitializationOptions = map[string]any{"enable_build_on_save": true, "build_on_save_step": "check"}
	base.LSP["zig"] = zigBase

	got, err := LoadProject(base, ws)
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	zig := got.LSP["zig"]
	if zig.Command != "zls" {
		t.Errorf("zig command = %q, want inherited \"zls\"", zig.Command)
	}
	opts := zig.InitializationOptions
	if opts == nil {
		t.Fatal("InitializationOptions is nil, want the global table")
	}
	// go-toml/v2 decodes a bare `true` as bool and a quoted value as string.
	if v, ok := opts["enable_build_on_save"].(bool); !ok || !v {
		t.Errorf("enable_build_on_save = %#v, want bool true", opts["enable_build_on_save"])
	}
	if v, ok := opts["build_on_save_step"].(string); !ok || v != "check" {
		t.Errorf("build_on_save_step = %#v, want \"check\"", opts["build_on_save_step"])
	}

	// The same project file against a base with no options must yield no options:
	// the project cannot introduce them.
	got, err = LoadProject(defaults, ws)
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	if got.LSP["zig"].InitializationOptions != nil {
		t.Errorf("project initialization_options honoured: %#v, want nil (global-only)",
			got.LSP["zig"].InitializationOptions)
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

// TestCloneLSPConfig_InitializationOptions guards that cloneLSPConfig clones
// the free-form options map at the top level (maps.Clone — nested sub-table
// values are shared, which is safe because the map is only ever serialised,
// never mutated in place; gopls's EnablePullDiagnostics clones before it
// injects), and preserves nil so cloneConfig(defaults) stays DeepEqual.
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
