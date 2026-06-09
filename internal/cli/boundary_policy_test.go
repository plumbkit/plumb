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

func TestBuildPathPolicy_NonGoSkipsDependencyReads(t *testing.T) {
	goroot := build.Default.GOROOT
	stdlib := filepath.Join(goroot, "src", "fmt", "print.go")
	if _, err := os.Stat(stdlib); err != nil {
		t.Skipf("GOROOT stdlib not present: %v", err)
	}
	ws := t.TempDir()
	s := &connSession{ctx: context.Background()}
	v := &sessionView{
		acquiredRoot:     ws,
		acquiredLanguage: "python",
		ws:               config.WorkspaceConfig{AllowDependencyReads: true},
		depRoots:         computeGoDependencyRoots(context.Background()),
	}

	pol := s.buildPathPolicy(v)
	if _, err := pol.Check(stdlib, tools.AccessRead); err == nil {
		t.Error("non-Go session must not gain Go dependency read roots")
	}
}
