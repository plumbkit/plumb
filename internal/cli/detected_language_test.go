package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
)

func TestDetectAnyLanguageAtUsesDisabledAdapterMarkers(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pom.xml"), []byte("<project/>"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := detectAnyLanguageAt(dir, config.Defaults()); got != "java" {
		t.Fatalf("detectAnyLanguageAt = %q, want java", got)
	}
}

// TestDetectAnyLanguageAt_StopsAtHome guards the display path the way the
// authoritative Detect/detectLanguageAt walks are guarded: a stray language
// marker in $HOME (e.g. a global ~/package.json) must not be reported as the
// detected language for a markerless workspace beneath it.
func TestDetectAnyLanguageAt_StopsAtHome(t *testing.T) {
	home := freshTempDir(t)
	t.Setenv("HOME", home)
	mustWrite(t, filepath.Join(home, "go.mod"), "module stray\n")
	ws := filepath.Join(home, "Projects", "app")
	mustMkdir(t, ws)

	if got := detectAnyLanguageAt(ws, config.Defaults()); got != "" {
		t.Fatalf("detectAnyLanguageAt = %q, want \"\" (a stray ~/go.mod must not be the detected language)", got)
	}
}

func TestAdapterForLanguageIncludesJava(t *testing.T) {
	if got := adapterForLanguage("java"); got != "jdtls" {
		t.Fatalf("adapterForLanguage(java) = %q, want jdtls", got)
	}
}
