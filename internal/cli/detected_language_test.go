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

// TestDetectedLabel_HomeRootNamesTheLSPSkipCause pins issue #316: a workspace
// root that IS the home directory gets no language (every detection walk stops
// at $HOME), and the previous "" left the session record silent about WHY the
// LSP was off. The label must name the cause — even with a stray marker at
// home, which must not turn into a detection answer.
func TestDetectedLabel_HomeRootNamesTheLSPSkipCause(t *testing.T) {
	home := freshTempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	mustWrite(t, filepath.Join(home, "go.mod"), "module stray\n")

	if got := detectedLabel(home, LanguageNone, nil, config.Defaults()); got != homeLSPSkipNote {
		t.Fatalf("detectedLabel(home, none) = %q, want the LSP-skip note %q", got, homeLSPSkipNote)
	}

	// Control: an ordinary markerless root keeps the historical "" — there is
	// no deliberate skip to name there.
	other := freshTempDir(t)
	if got := detectedLabel(other, LanguageNone, nil, config.Defaults()); got != "" {
		t.Fatalf("detectedLabel(markerless non-home, none) = %q, want \"\"", got)
	}
}

func TestAdapterForLanguageIncludesJava(t *testing.T) {
	if got := adapterForLanguage("java"); got != "jdtls" {
		t.Fatalf("adapterForLanguage(java) = %q, want jdtls", got)
	}
}
