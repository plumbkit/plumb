package fsguard

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestRefuseWalk_DisabledByFlag(t *testing.T) {
	home, _ := os.UserHomeDir()
	if got, _ := RefuseWalk(home, false); got {
		t.Errorf("RefuseWalk(home, false) = true, want false (guard disabled)")
	}
}

func TestRefuseWalk_NonDarwinShortCircuits(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("Darwin-specific behaviour validated in TestRefuseWalk_DarwinProtected")
	}
	home, _ := os.UserHomeDir()
	if home == "" {
		t.Skip("no HOME")
	}
	if got, reason := RefuseWalk(home, true); got {
		t.Errorf("RefuseWalk(home, true) on %s = true (%s), want false (guard is macOS-only)",
			runtime.GOOS, reason)
	}
}

func TestRefuseWalk_DarwinProtected(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Darwin-only")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no HOME")
	}
	for _, sub := range []string{"", "Desktop", "Documents", "Downloads", "Pictures", "Music", "Movies", "Public"} {
		p := home
		if sub != "" {
			p = filepath.Join(home, sub)
		}
		if got, reason := RefuseWalk(p, true); !got {
			t.Errorf("RefuseWalk(%s, true) = false (%s), want true", p, reason)
		}
	}
}

func TestRefuseWalk_AllowsSubpath(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Darwin-only")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no HOME")
	}
	// A real project nested inside Documents should NOT be refused — only
	// the protected directory itself is.
	nested := filepath.Join(home, "Documents", "SomeProject")
	if got, reason := RefuseWalk(nested, true); got {
		t.Errorf("RefuseWalk(%s, true) = true (%s), want false (subpaths are allowed)", nested, reason)
	}
}

func TestRefuseWalk_AllowsUnrelatedPath(t *testing.T) {
	if got, _ := RefuseWalk("/tmp", true); got {
		t.Errorf("RefuseWalk(/tmp, true) = true, want false")
	}
}

func TestRefuseWalk_EmptyRoot(t *testing.T) {
	if got, _ := RefuseWalk("", true); got {
		t.Errorf("RefuseWalk(\"\", true) = true, want false")
	}
}
