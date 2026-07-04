package cli

import (
	"context"
	"go/build"
	"os"
	"path/filepath"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/tools"
)

func TestBuildPathPolicy_ExtraAndReadRoots(t *testing.T) {
	ws := t.TempDir()
	extra := t.TempDir()
	readonly := t.TempDir()
	s := &connSession{}
	v := &sessionView{
		acquiredRoot: ws,
		ws: config.WorkspaceConfig{
			ExtraRoots:           []string{extra},
			ReadRoots:            []string{readonly},
			AllowDependencyReads: false,
		},
	}

	pol := s.buildPathPolicy(v)

	if _, err := pol.Check(filepath.Join(ws, "a.go"), tools.AccessReadWrite); err != nil {
		t.Errorf("workspace should be writable: %v", err)
	}
	if _, err := pol.Check(filepath.Join(extra, "a.go"), tools.AccessReadWrite); err != nil {
		t.Errorf("configured extra root should be writable: %v", err)
	}
	if _, err := pol.Check(filepath.Join(readonly, "b.go"), tools.AccessRead); err != nil {
		t.Errorf("configured read root should be readable: %v", err)
	}
	if _, err := pol.Check(filepath.Join(readonly, "b.go"), tools.AccessReadWrite); err == nil {
		t.Error("configured read root must not be writable")
	}
}

// TestBuildPathPolicy_TrustedWorkspaceRoots proves the manually-granted,
// out-of-repo per-workspace roots are folded into the allowlist: extra roots
// read-write, read roots read-only. XDG_DATA_HOME is sandboxed so the store
// resolves under a temp dir, never the developer's real data dir.
func TestBuildPathPolicy_TrustedWorkspaceRoots(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ws := t.TempDir()
	extra := t.TempDir()
	readonly := t.TempDir()
	store := config.NewWorkspaceRootsStore()
	if err := store.SetExtraRoots(ws, []string{extra}); err != nil {
		t.Fatalf("SetExtraRoots: %v", err)
	}
	if err := store.SetReadRoots(ws, []string{readonly}); err != nil {
		t.Fatalf("SetReadRoots: %v", err)
	}

	s := &connSession{}
	v := &sessionView{acquiredRoot: ws, ws: config.WorkspaceConfig{}}
	pol := s.buildPathPolicy(v)

	if _, err := pol.Check(filepath.Join(extra, "a.go"), tools.AccessReadWrite); err != nil {
		t.Errorf("granted extra root should be writable: %v", err)
	}
	if _, err := pol.Check(filepath.Join(readonly, "b.go"), tools.AccessRead); err != nil {
		t.Errorf("granted read root should be readable: %v", err)
	}
	if _, err := pol.Check(filepath.Join(readonly, "b.go"), tools.AccessReadWrite); err == nil {
		t.Error("granted read root must not be writable")
	}
	if label := pol.OutsideWorkspaceLabel(filepath.Join(extra, "a.go")); label != "workspace-root" {
		t.Errorf("outside-workspace label = %q, want workspace-root", label)
	}
	// A path under no root at all is still refused — the store only widens to the
	// exact granted paths.
	if _, err := pol.Check(filepath.Join(t.TempDir(), "c.go"), tools.AccessRead); err == nil {
		t.Error("a path under no granted root must still be refused")
	}
}

func TestBuildPathPolicy_GoDependencyReads(t *testing.T) {
	goroot := build.Default.GOROOT
	stdlib := filepath.Join(goroot, "src", "fmt", "print.go")
	if _, err := os.Stat(stdlib); err != nil {
		t.Skipf("GOROOT stdlib not present: %v", err)
	}

	ws := t.TempDir()
	s := &connSession{ctx: context.Background()}
	v := &sessionView{
		acquiredRoot:     ws,
		acquiredLanguage: "go",
		ws:               config.WorkspaceConfig{AllowDependencyReads: true},
		depRoots:         computeGoDependencyRoots(context.Background()),
		depRootsLang:     "go",
	}

	pol := s.buildPathPolicy(v)

	if _, err := pol.Check(stdlib, tools.AccessRead); err != nil {
		t.Errorf("GOROOT stdlib should be readable with dependency reads on: %v", err)
	}
	if _, err := pol.Check(stdlib, tools.AccessReadWrite); err == nil {
		t.Error("GOROOT must never be writable")
	}
	// Annotation: a GOROOT file is outside the workspace.
	if label := pol.OutsideWorkspaceLabel(stdlib); label != "GOROOT" {
		t.Errorf("outside-workspace label = %q, want GOROOT", label)
	}
}

// TestBuildPathPolicy_DepRootsGatedByLanguage proves the cross-language guard:
// dep roots resolved for one language are admitted only while the session stays
// on that language. A re-pin to another language must not leak the prior
// language's roots until warmDepRoots recomputes them.
func TestBuildPathPolicy_DepRootsGatedByLanguage(t *testing.T) {
	ws := t.TempDir()
	dep := t.TempDir()
	depFile := filepath.Join(dep, "lib.go")
	s := &connSession{ctx: context.Background()}

	// dep roots resolved for "go" but the session is currently on "python".
	v := &sessionView{
		acquiredRoot:     ws,
		acquiredLanguage: "python",
		ws:               config.WorkspaceConfig{AllowDependencyReads: true},
		depRoots:         []tools.AllowedRoot{{Path: dep, Access: tools.AccessRead, Label: "GOROOT"}},
		depRootsLang:     "go",
	}
	pol := s.buildPathPolicy(v)
	if _, err := pol.Check(depFile, tools.AccessRead); err == nil {
		t.Error("dep roots resolved for another language must not be readable (depRootsLang mismatch)")
	}

	// Once the recorded language matches the session language, the roots apply.
	v.depRootsLang = "python"
	pol = s.buildPathPolicy(v)
	if _, err := pol.Check(depFile, tools.AccessRead); err != nil {
		t.Errorf("dep roots resolved for the session language should be readable: %v", err)
	}
	if _, err := pol.Check(depFile, tools.AccessReadWrite); err == nil {
		t.Error("dependency roots must never be writable")
	}
}
