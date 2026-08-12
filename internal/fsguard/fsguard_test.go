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

// TestRefuseWalk_RelativeRootIsAnchored pins the one thing this package's
// canonicaliser does that the shared paths.Canonical deliberately refuses to do
// (issue #273): it anchors a relative root to the process working directory.
//
// paths.Canonical leaves a relative path relative, because giving one a location
// is the silent cross-repository write of issue #181. Here that same refusal
// would fail OPEN: RefuseWalk matches protected directories by exact path, so an
// unanchored "." equals no protected root and the walk proceeds — in exactly the
// case where the working directory IS the protected directory. plumb would then
// crawl $HOME and raise the TCC prompt this package exists to prevent. A refusal
// that fails open is not a refusal.
func TestRefuseWalk_RelativeRootIsAnchored(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Darwin-only")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(home)

	// Control: without this the relative assertions below could pass for the
	// wrong reason (or fail because the fixture's HOME was never protected).
	if got, _ := RefuseWalk(home, true); !got {
		t.Fatalf("control failed: RefuseWalk(%s, true) = false — the fixture's $HOME is not being treated as protected, so the relative cases prove nothing", home)
	}
	for _, rel := range []string{".", "./", "sub/.."} {
		if got, _ := RefuseWalk(rel, true); !got {
			t.Errorf("RefuseWalk(%q, true) = false with the working directory AT $HOME; a relative root must be anchored before it is matched", rel)
		}
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
