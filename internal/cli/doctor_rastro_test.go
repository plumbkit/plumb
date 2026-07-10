package cli

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
)

// rastroResults is the pure half of checkRastro, so these exercise every branch
// without touching the user's real global config.

func TestRastroResults_DisabledReturnsOK(t *testing.T) {
	cfg := config.Config{Rastro: config.RastroConfig{Enabled: false, Path: "rastro"}}

	got := rastroResults(cfg)
	if len(got) != 1 {
		t.Fatalf("want exactly one result, got %d", len(got))
	}
	if !got[0].ok || got[0].warn {
		t.Errorf("a disabled integration is a clean pass, got ok=%v warn=%v", got[0].ok, got[0].warn)
	}
	if got[0].detail != "disabled in config" {
		t.Errorf("detail = %q, want %q", got[0].detail, "disabled in config")
	}
	if got[0].fix != "" {
		t.Errorf("a clean pass must not suggest a fix, got %q", got[0].fix)
	}
}

func TestRastroResults_MissingBinaryFails(t *testing.T) {
	// An empty PATH guarantees the lookup fails whatever is installed.
	t.Setenv("PATH", t.TempDir())
	cfg := config.Config{Rastro: config.RastroConfig{Enabled: true, Path: "definitely-not-a-real-binary"}}

	got := rastroResults(cfg)
	if len(got) != 1 {
		t.Fatalf("want exactly one result, got %d", len(got))
	}
	if got[0].ok {
		t.Error("an enabled integration whose binary is absent must fail the run (ok=false)")
	}
	if !strings.Contains(got[0].detail, "definitely-not-a-real-binary") {
		t.Errorf("detail should name the binary it looked for, got %q", got[0].detail)
	}
	if got[0].fix == "" {
		t.Error("a failing check must offer a fix hint")
	}
}

func TestRastroResults_FoundOnPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executable-bit semantics differ on Windows")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "rastro")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	cfg := config.Config{Rastro: config.RastroConfig{Enabled: true, Path: "rastro"}}

	got := rastroResults(cfg)
	if len(got) != 1 {
		t.Fatalf("want exactly one result, got %d", len(got))
	}
	if !got[0].ok || got[0].warn {
		t.Errorf("a resolvable binary is a clean pass, got ok=%v warn=%v detail=%q", got[0].ok, got[0].warn, got[0].detail)
	}
	if !strings.Contains(got[0].detail, "rastro") {
		t.Errorf("detail should report the resolved path, got %q", got[0].detail)
	}
}

// An empty Path must fall back to the "rastro" binary name rather than
// LookPath("") — which fails with a confusing error naming no binary at all.
func TestRastroResults_EmptyPathFallsBackToDefaultName(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	cfg := config.Config{Rastro: config.RastroConfig{Enabled: true, Path: ""}}

	got := rastroResults(cfg)
	if len(got) != 1 {
		t.Fatalf("want exactly one result, got %d", len(got))
	}
	if !strings.Contains(got[0].detail, `"rastro"`) {
		t.Errorf("an empty path must be reported as the default name %q, got %q", "rastro", got[0].detail)
	}
}

// An unloadable config must warn, never fail and never vanish: checkConfigs
// already fails the run for the same fault, and returning nil would drop the
// whole Integrations section silently.
func TestRastroConfigErr_WarnsWithoutFailing(t *testing.T) {
	got := rastroConfigErr(errors.New("bad toml at line 3"))
	if len(got) != 1 {
		t.Fatalf("want exactly one result, got %d", len(got))
	}
	if !got[0].ok {
		t.Error("a config-load fault must not fail the run twice; checkConfigs owns that failure")
	}
	if !got[0].warn {
		t.Error("a config-load fault must be visible as a warning, not a clean pass")
	}
	if !strings.Contains(got[0].detail, "bad toml at line 3") {
		t.Errorf("detail should carry the underlying error, got %q", got[0].detail)
	}
}
