package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/tools"
	"github.com/plumbkit/plumb/internal/tools/txlog"
)

// txlog_replay_redesign_test.go — the defects an adversarial review found in the
// FIRST version of this fix, each pinned against the REAL PathPolicy.
//
// Every test here was mutation-verified: the production change it covers was
// reverted and the test observed to fail with the real payload.

// TestTxlogReplayGuard_ParentTraversalCannotEscape is the escape the first
// version did not close.
//
// A policy canonicalises with filepath.Abs, which cancels "sub/.." LEXICALLY
// before resolving anything; the kernel follows `sub` first and applies ".." to
// wherever that landed. With `sub` a committed symlink the guard admitted an
// in-workspace path while os.WriteFile wrote outside every allowed root.
// refuseSymlink did not cover it either: it Lstats the final component, which
// here is an ordinary file.
func TestTxlogReplayGuard_ParentTraversalCannotEscape(t *testing.T) {
	ws := evalTempDir(t)
	outside := evalTempDir(t)
	if err := os.MkdirAll(filepath.Join(outside, "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(outside, "victim.txt")
	if err := os.WriteFile(victim, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "deep"), filepath.Join(ws, "sub")); err != nil {
		t.Skipf("symlinks unsupported on this filesystem: %v", err)
	}
	// Concatenated, not filepath.Join: Join would Clean the payload away.
	writeReplayOrphan(t, ws, ws+"/sub/../victim.txt", "pwned\n")

	txlog.Scan(ws, time.Now(), txlogReplayGuard(workspacePolicy(ws)))

	if b, _ := os.ReadFile(victim); string(b) != "original\n" {
		t.Errorf("replay wrote outside every allowed root via a \"..\" traversal: %s = %q", victim, b)
	}
}

// TestTxlogReplayGuard_DemandsWriteAccess pins that the guard asks for WRITE.
// Nothing did: changing AccessReadWrite to AccessRead in txlogReplayGuard left
// the whole suite green, so a read-only root would have been restorable.
func TestTxlogReplayGuard_DemandsWriteAccess(t *testing.T) {
	ws := evalTempDir(t)
	readOnly := evalTempDir(t)
	target := filepath.Join(readOnly, "lib.go")
	writeReplayOrphan(t, ws, target, "package lib\n")

	pol := tools.NewPathPolicy(ws, []tools.AllowedRoot{
		{Path: ws, Access: tools.AccessReadWrite, Label: "workspace"},
		{Path: readOnly, Access: tools.AccessRead, Label: "read-root"},
	})
	// Premise: the path IS under an allowed root, so only the access level can
	// be what refuses it. Without this the test would pass for the wrong reason.
	if _, err := pol.Check(target, tools.AccessRead); err != nil {
		t.Fatalf("premise failed — the read-only root no longer admits reads: %v", err)
	}

	txlog.Scan(ws, time.Now(), txlogReplayGuard(pol))

	if _, err := os.Stat(target); err == nil {
		t.Error("replay wrote into a read-only root — the guard is not demanding write access")
	}
}

// TestTxlogReplayGuard_RestoresThroughAnInWorkspaceSymlink is the regression the
// blanket symlink refusal introduced, and the reason this fix resolves instead.
//
// safeWrite FOLLOWS a resolvable link ("resolve to the real target so we write
// through the link"); only a dangling one is replaced. A symlinked source file
// pointing elsewhere inside an allowed root is ordinary in a monorepo, and
// refusing every link silently dropped its crash recovery.
func TestTxlogReplayGuard_RestoresThroughAnInWorkspaceSymlink(t *testing.T) {
	ws := evalTempDir(t)
	target := filepath.Join(ws, "pkg", "real.go")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("damaged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(ws, "linked.go")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unsupported on this filesystem: %v", err)
	}
	writeReplayOrphan(t, ws, link, "package real\n")

	txlog.Scan(ws, time.Now(), txlogReplayGuard(workspacePolicy(ws)))

	if got, _ := os.ReadFile(target); string(got) != "package real\n" {
		t.Errorf("a legitimate symlinked in-workspace file must still be recovered, got %q", got)
	}
	// The link itself must survive: writing through it, not over it.
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the symlink was replaced rather than written through (err=%v)", err)
	}
}

// TestTxlogReplayGuard_ResolvedTargetIsRechecked pins that the replay puts the
// RESOLVED file through the guard, not only the path as written.
//
// Stated honestly: against the real PathPolicy this re-check is redundant today,
// because canonicalRoot already EvalSymlinks an existing link, so the policy
// refuses an outward link unaided — TestTxlogReplayGuard_SymlinkedAncestorCannotEscape
// covers that. The re-check exists because txlog takes an INJECTED guard and
// cannot know it resolves; a guard that compares lexically would otherwise admit
// the link and let os.WriteFile follow it.
//
// So it is driven with exactly such a guard, which is what makes this assertion
// able to fail. With the resolved-target re-check removed, it does.
func TestTxlogReplayGuard_ResolvedTargetIsRechecked(t *testing.T) {
	ws := evalTempDir(t)
	outside := evalTempDir(t)
	victim := filepath.Join(outside, "victim.txt")
	if err := os.WriteFile(victim, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(ws, "innocent.txt")
	if err := os.Symlink(victim, link); err != nil {
		t.Skipf("symlinks unsupported on this filesystem: %v", err)
	}

	// A guard that judges the path as written, without resolving it: the shape
	// the re-check defends against.
	lexical := func(path string) error {
		rel, err := filepath.Rel(ws, filepath.Clean(path))
		if err != nil || rel == ".." || len(rel) > 2 && rel[:3] == "../" {
			return os.ErrPermission
		}
		return nil
	}
	if err := lexical(link); err != nil {
		t.Fatalf("premise failed — the lexical guard should admit the in-workspace link: %v", err)
	}
	writeReplayOrphan(t, ws, link, "pwned\n")

	txlog.Scan(ws, time.Now(), lexical)

	if b, _ := os.ReadFile(victim); string(b) != "original\n" {
		t.Errorf("replay followed a link out of every allowed root: %s = %q", victim, b)
	}
}
