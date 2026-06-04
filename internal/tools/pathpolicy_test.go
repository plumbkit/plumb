package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPathPolicy_ReadWriteAccess(t *testing.T) {
	ws := t.TempDir()
	dep := t.TempDir()
	pol := NewPathPolicy(ws, []AllowedRoot{
		{Path: ws, Access: AccessReadWrite, Label: "workspace"},
		{Path: dep, Access: AccessRead, Label: "GOMODCACHE"},
	})

	wsFile := filepath.Join(ws, "a.go")
	depFile := filepath.Join(dep, "lib.go")
	outside := filepath.Join(t.TempDir(), "x.go")

	if _, err := pol.Check(wsFile, AccessRead); err != nil {
		t.Errorf("read inside workspace: %v", err)
	}
	if _, err := pol.Check(wsFile, AccessReadWrite); err != nil {
		t.Errorf("write inside workspace: %v", err)
	}
	// Dependency root: read allowed, write refused by construction (D7).
	if _, err := pol.Check(depFile, AccessRead); err != nil {
		t.Errorf("read under dep root should be allowed: %v", err)
	}
	if _, err := pol.Check(depFile, AccessReadWrite); err == nil {
		t.Error("write under read-only dep root must be refused")
	}
	// Fully outside any root: refused.
	if _, err := pol.Check(outside, AccessRead); err == nil {
		t.Error("read fully outside the allowlist must be refused")
	}
}

func TestPathPolicy_OutsideWorkspaceLabel(t *testing.T) {
	ws := t.TempDir()
	dep := t.TempDir()
	pol := NewPathPolicy(ws, []AllowedRoot{
		{Path: ws, Access: AccessReadWrite, Label: "workspace"},
		{Path: dep, Access: AccessRead, Label: "GOMODCACHE"},
	})
	if got := pol.OutsideWorkspaceLabel(filepath.Join(ws, "a.go")); got != "" {
		t.Errorf("workspace path label = %q, want empty", got)
	}
	if got := pol.OutsideWorkspaceLabel(filepath.Join(dep, "lib.go")); got != "GOMODCACHE" {
		t.Errorf("dep path label = %q, want GOMODCACHE", got)
	}
	if got := pol.OutsideWorkspaceLabel(filepath.Join(t.TempDir(), "x.go")); got != "" {
		t.Errorf("unmatched path label = %q, want empty", got)
	}
}

func TestPathPolicy_NilAllowsAll(t *testing.T) {
	var pol *PathPolicy
	if _, err := pol.Check("/etc/passwd", AccessReadWrite); err != nil {
		t.Errorf("nil policy should be a no-op: %v", err)
	}
	if got := pol.OutsideWorkspaceLabel("/etc/passwd"); got != "" {
		t.Errorf("nil policy label = %q, want empty", got)
	}
}

func TestPathPolicy_LongestPrefixWins(t *testing.T) {
	ws := t.TempDir()
	// A read-only sub-tree nested inside a read-write workspace: the nested
	// (longer) root wins, so a write into it is refused even though the parent
	// is writable.
	nested := filepath.Join(ws, "vendored")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	pol := NewPathPolicy(ws, []AllowedRoot{
		{Path: ws, Access: AccessReadWrite, Label: "workspace"},
		{Path: nested, Access: AccessRead, Label: "read-root"},
	})
	if _, err := pol.Check(filepath.Join(nested, "f.go"), AccessReadWrite); err == nil {
		t.Error("write into nested read-only root must be refused (longest-prefix wins)")
	}
	if _, err := pol.Check(filepath.Join(ws, "f.go"), AccessReadWrite); err != nil {
		t.Errorf("write into workspace (outside nested) should be allowed: %v", err)
	}
}

func TestPathPolicy_SymlinkEscapeBlocked(t *testing.T) {
	ws := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(ws, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	pol := NewPathPolicy(ws, []AllowedRoot{{Path: ws, Access: AccessReadWrite, Label: "workspace"}})
	// A path traversing the in-workspace symlink resolves to outside the
	// workspace and must be refused.
	if _, err := pol.Check(filepath.Join(link, "secret.txt"), AccessRead); err == nil {
		t.Error("path via in-workspace symlink pointing outside must be refused")
	}
}

func TestPathPolicy_ReadOnlyWriteDenialMessage(t *testing.T) {
	ws := t.TempDir()
	dep := t.TempDir()
	pol := NewPathPolicy(ws, []AllowedRoot{
		{Path: ws, Access: AccessReadWrite, Label: "workspace"},
		{Path: dep, Access: AccessRead, Label: "GOMODCACHE"},
	})
	depFile := filepath.Join(dep, "lib.go")
	_, err := pol.Check(depFile, AccessReadWrite)
	if err == nil {
		t.Fatal("expected denial for write to read-only root")
	}
	msg := err.Error()
	if !strings.Contains(msg, "read-only root (GOMODCACHE)") {
		t.Errorf("denial message should name the read-only root; got: %s", msg)
	}
	if strings.Contains(msg, "different project") {
		t.Errorf("denial for a known read-only root must not say 'different project'; got: %s", msg)
	}
	// The error must still be a WorkspaceBoundaryError so IsWorkspaceBoundaryError holds.
	if !IsWorkspaceBoundaryError(err) {
		t.Errorf("error should still satisfy IsWorkspaceBoundaryError")
	}
	// A fully-outside path (no match) should still produce the generic message.
	outside := filepath.Join(t.TempDir(), "x.go")
	_, errOut := pol.Check(outside, AccessRead)
	if errOut == nil {
		t.Fatal("expected denial for path outside all roots")
	}
	if !strings.Contains(errOut.Error(), "different project") {
		t.Errorf("unmatched-path denial should say 'different project'; got: %s", errOut.Error())
	}
}

func TestReadFile_OutsideWorkspaceAnnotation(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "lib.go")
	if err := os.WriteFile(p, []byte("package lib\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := NewReadFile(nil).WithOutsideLabel(func(string) string { return "GOMODCACHE" })
	raw, _ := json.Marshal(map[string]string{"file_path": p})
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("read_file: %v", err)
	}
	if !strings.Contains(out, "outside the workspace (GOMODCACHE)") {
		t.Errorf("missing out-of-workspace annotation:\n%s", out)
	}
}
