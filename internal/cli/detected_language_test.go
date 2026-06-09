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

func TestAdapterForLanguageIncludesJava(t *testing.T) {
	if got := adapterForLanguage("java"); got != "jdtls" {
		t.Fatalf("adapterForLanguage(java) = %q, want jdtls", got)
	}
}
