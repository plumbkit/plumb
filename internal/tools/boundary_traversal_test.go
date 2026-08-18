package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// boundary_traversal_test.go — the workspace boundary against ".." traversal
// through a symlink.
//
// canonicalPathForBoundary canonicalises with filepath.Abs, which Cleans
// "sub/.." away LEXICALLY, before any symlink is resolved. The kernel resolves
// left to right: it follows `sub` first, and applies ".." to wherever that
// landed. resolvePath passes an absolute argument through untouched, so the
// string the policy rules on and the string the syscall receives were the same
// text meaning two different files.
//
// Against the unfixed code every escape test below wrote to, read, or listed a
// file outside every allowed root; the root-symlink case served /etc/hosts to a
// session pinned to a temp directory. The controls at the bottom exist because
// each escape assertion must be able to fail — a refusal that came from the
// wrong cause, or a tool that finds nothing at all, would make them vacuous.

// traversalScene builds a workspace containing a committed symlink `sub`
// pointing into an outside tree, plus a victim file in that tree. The payload
// Cleans to an in-workspace path but resolves outside it.
func traversalScene(t *testing.T) (ws, victim, payload string, pol *PathPolicy) {
	t.Helper()
	ws = evalTempDir(t)
	outside := evalTempDir(t)
	if err := os.MkdirAll(filepath.Join(outside, "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	victim = filepath.Join(outside, "victim.txt")
	if err := os.WriteFile(victim, []byte("ORIGINAL\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "deep"), filepath.Join(ws, "sub")); err != nil {
		t.Skipf("symlinks unsupported on this filesystem: %v", err)
	}
	// Built by concatenation: filepath.Join would Clean the payload away.
	payload = ws + "/sub/../victim.txt"
	pol = NewPathPolicy(ws, []AllowedRoot{{Path: ws, Access: AccessReadWrite, Label: "workspace"}})
	return ws, victim, payload, pol
}

// evalTempDir resolves symlinks in a t.TempDir. On macOS the temp root is /var,
// itself a link to /private/var, and boundary comparison is on resolved paths —
// an unresolved root makes a policy refuse its own workspace, which turns a
// confinement test green for entirely the wrong reason.
func evalTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		return resolved
	}
	return dir
}

func victimIntact(t *testing.T, victim string) {
	t.Helper()
	b, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("reading victim: %v", err)
	}
	if string(b) != "ORIGINAL\n" {
		t.Errorf("a file outside every allowed root was modified: %s = %q", victim, b)
	}
}

func TestPathPolicy_RefusesAbsoluteParentTraversal(t *testing.T) {
	ws, _, payload, pol := traversalScene(t)

	_, err := pol.Check(payload, AccessReadWrite)
	if err == nil {
		t.Fatal("the policy admitted a path whose lexical and kernel readings differ")
	}
	var traversal ParentTraversalError
	if !errors.As(err, &traversal) {
		t.Fatalf("want ParentTraversalError, got %T: %v", err, err)
	}
	if traversal.Canonical != filepath.Join(ws, "victim.txt") {
		t.Errorf("the refusal should offer the cleaned form as the fix, got %q", traversal.Canonical)
	}
	if !IsWorkspaceBoundaryError(err) {
		t.Error("a traversal refusal must classify as a path refusal, so callers suppress their retry")
	}
}

func TestWriteFile_ParentTraversalCannotEscape(t *testing.T) {
	ws, victim, payload, pol := traversalScene(t)
	deps := WriteDeps{Boundary: pol.WriteGuard(), WorkspaceFn: func(context.Context) string { return ws }}
	raw, _ := json.Marshal(map[string]any{"file_path": payload, "content": "PWNED\n"})

	if _, err := NewWriteFile(deps).Execute(context.Background(), raw); err == nil {
		t.Error("write_file accepted a traversal path")
	}
	victimIntact(t, victim)
}

func TestReadFile_ParentTraversalCannotDisclose(t *testing.T) {
	ws, victim, payload, pol := traversalScene(t)
	if err := os.WriteFile(victim, []byte("SECRET MATERIAL\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	guard := BoundaryGuard(func(_ context.Context, p string) error { _, err := pol.Check(p, AccessRead); return err })
	rf := NewReadFile(nil).WithBoundary(guard)
	rf.ws = func(context.Context) string { return ws }
	raw, _ := json.Marshal(map[string]any{"file_path": payload})

	out, err := rf.Execute(context.Background(), raw)
	if err == nil {
		t.Error("read_file accepted a traversal path")
	}
	if strings.Contains(out, "SECRET MATERIAL") {
		t.Errorf("read_file disclosed %s", victim)
	}
}

func TestFindFiles_ParentTraversalCannotList(t *testing.T) {
	ws, _, _, pol := traversalScene(t)
	guard := BoundaryGuard(func(_ context.Context, p string) error { _, err := pol.Check(p, AccessRead); return err })
	// Pattern "*.txt", never "victim*": asserting on a string the tool echoes
	// back from the arguments would pass whether or not it walked anywhere.
	raw, _ := json.Marshal(map[string]any{"path": ws + "/sub/..", "pattern": "*.txt"})

	out, err := NewFindFiles(func(context.Context) string { return ws }).WithBoundary(guard).
		Execute(context.Background(), raw)
	if err == nil {
		t.Error("find_files accepted a traversal root")
	}
	if strings.Contains(out, "victim.txt") {
		t.Errorf("find_files disclosed an entry outside every allowed root:\n%s", out)
	}
}

func TestSearchInFiles_ParentTraversalCannotEscape(t *testing.T) {
	ws, victim, _, pol := traversalScene(t)
	if err := os.WriteFile(victim, []byte("SECRET MATERIAL\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	guard := BoundaryGuard(func(_ context.Context, p string) error { _, err := pol.Check(p, AccessRead); return err })
	raw, _ := json.Marshal(map[string]any{"pattern": "SECRET MATERIAL", "path": ws + "/sub/.."})

	out, err := NewSearchInFiles(func(context.Context) string { return ws }, nil, nil, time.Minute).
		WithBoundary(guard).Execute(context.Background(), raw)
	if err == nil {
		t.Error("search_in_files accepted a traversal root")
	}
	// Assert on the filename: the tool echoes the pattern in its "No matches"
	// line, so matching on the pattern would be vacuously true.
	if strings.Contains(out, "victim.txt") {
		t.Errorf("search_in_files reached outside every allowed root:\n%s", out)
	}
}

func TestFindReplace_ParentTraversalCannotEscape(t *testing.T) {
	ws, victim, _, pol := traversalScene(t)
	deps := WriteDeps{Boundary: pol.WriteGuard(), WorkspaceFn: func(context.Context) string { return ws }}
	raw, _ := json.Marshal(map[string]any{
		"path": ws + "/sub/..", "pattern": "ORIGINAL", "replacement": "PWNED", "dry_run": false,
	})

	if _, err := NewFindReplace(deps).Execute(context.Background(), raw); err == nil {
		t.Error("find_replace accepted a traversal root")
	}
	victimIntact(t, victim)
}

// TestReadFile_RootSymlinkCannotAddressTheFilesystem is the severity case. One
// committed symlink `sub -> /` made every absolute path on the machine into an
// in-workspace-looking address: <ws>/sub/../etc/hosts Cleaned to <ws>/etc/hosts.
// Read-only on purpose; the write half is covered above against a temp victim.
func TestReadFile_RootSymlinkCannotAddressTheFilesystem(t *testing.T) {
	if _, err := os.Stat("/etc/hosts"); err != nil {
		t.Skip("no /etc/hosts on this platform")
	}
	ws := evalTempDir(t)
	if err := os.Symlink("/", filepath.Join(ws, "sub")); err != nil {
		t.Skipf("symlinks unsupported on this filesystem: %v", err)
	}
	pol := NewPathPolicy(ws, []AllowedRoot{{Path: ws, Access: AccessReadWrite, Label: "workspace"}})
	guard := BoundaryGuard(func(_ context.Context, p string) error { _, err := pol.Check(p, AccessRead); return err })
	rf := NewReadFile(nil).WithBoundary(guard)
	rf.ws = func(context.Context) string { return ws }
	raw, _ := json.Marshal(map[string]any{"file_path": ws + "/sub/../etc/hosts"})

	out, err := rf.Execute(context.Background(), raw)
	if err == nil {
		t.Error("read_file accepted a path addressing the filesystem root through a symlink")
	}
	if strings.Contains(out, "localhost") {
		t.Errorf("read_file served /etc/hosts to a session pinned to %s", ws)
	}
}

// TestFileStatus_ParentTraversalIsRefused covers the one tool that cleaned the
// path BEFORE guarding it, so the refusal could never fire there. Not
// exploitable — the check and the os.Stat used the same cleaned string — but it
// answered about a file the caller had not named, reporting exists=false with no
// error, which is exactly the silent retargeting this refusal exists to prevent.
func TestFileStatus_ParentTraversalIsRefused(t *testing.T) {
	ws, _, payload, pol := traversalScene(t)
	guard := BoundaryGuard(func(_ context.Context, p string) error { _, err := pol.Check(p, AccessRead); return err })
	fs := NewFileStatus(nil).WithWorkspace(func(context.Context) string { return ws }).WithBoundary(guard)
	raw, _ := json.Marshal(map[string]any{"paths": []string{payload}})

	out, err := fs.Execute(context.Background(), raw)
	if err == nil && !strings.Contains(out, "not in canonical form") {
		t.Errorf("file_status answered about a path it should have refused:\n%s", out)
	}
	// Asserted on the EFFECT as well as the message, so a reworded refusal cannot
	// make this pass vacuously: the tool must not ANSWER. (Not asserted on the
	// cleaned path appearing in the output — the refusal itself quotes it, as the
	// fix to apply, and matching on that would fail against a correct refusal.)
	if strings.Contains(out, "exists:") {
		t.Errorf("file_status reported a status for a path it should have refused:\n%s", out)
	}
}

// --- controls: the refusals above must be specific, not a blanket outage ---

// TestRelativeTraversalStillResolves pins the asymmetry the fix depends on. A
// relative argument is anchored with filepath.Join, which Cleans, so the
// anchored result is the single path both the check and the write use. There is
// no divergence to refuse, and refusing it would break ordinary calls.
func TestRelativeTraversalStillResolves(t *testing.T) {
	ws, victim, _, pol := traversalScene(t)
	deps := WriteDeps{Boundary: pol.WriteGuard(), WorkspaceFn: func(context.Context) string { return ws }}
	raw, _ := json.Marshal(map[string]any{"file_path": "sub/../inside.txt", "content": "ok\n"})

	if _, err := NewWriteFile(deps).Execute(context.Background(), raw); err != nil {
		t.Fatalf("a relative path with .. is an ordinary in-workspace write: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "inside.txt")); string(b) != "ok\n" {
		t.Errorf("the relative form did not land in the workspace, got %q", b)
	}
	victimIntact(t, victim)
}

// TestOrdinaryAbsolutePathStillWrites is the blunt control: the fix must not
// have refused everything. Without it, every escape assertion above would pass
// against a policy that simply denied all access.
func TestOrdinaryAbsolutePathStillWrites(t *testing.T) {
	ws, _, _, pol := traversalScene(t)
	deps := WriteDeps{Boundary: pol.WriteGuard(), WorkspaceFn: func(context.Context) string { return ws }}
	target := filepath.Join(ws, "ordinary.txt")
	raw, _ := json.Marshal(map[string]any{"file_path": target, "content": "ok\n"})

	if _, err := NewWriteFile(deps).Execute(context.Background(), raw); err != nil {
		t.Fatalf("an ordinary in-workspace absolute write must still succeed: %v", err)
	}
	if b, _ := os.ReadFile(target); string(b) != "ok\n" {
		t.Errorf("unexpected content: %q", b)
	}
}

// TestWalkFindsInWorkspaceFile proves the walk-tool refusals above are about the
// traversal root and not about a tool that finds nothing under any root.
func TestWalkFindsInWorkspaceFile(t *testing.T) {
	ws, _, _, pol := traversalScene(t)
	if err := os.WriteFile(filepath.Join(ws, "inside.txt"), []byte("SECRET MATERIAL\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	guard := BoundaryGuard(func(_ context.Context, p string) error { _, err := pol.Check(p, AccessRead); return err })
	raw, _ := json.Marshal(map[string]any{"pattern": "SECRET MATERIAL", "path": ws})

	out, err := NewSearchInFiles(func(context.Context) string { return ws }, nil, nil, time.Minute).
		WithBoundary(guard).Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("control failed: %v", err)
	}
	if !strings.Contains(out, "inside.txt") {
		t.Fatalf("control failed — the tool finds nothing even inside the workspace, so the "+
			"escape tests prove nothing:\n%s", out)
	}
}
