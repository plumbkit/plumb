package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/tools"
	"github.com/plumbkit/plumb/internal/tools/txlog"
)

// txlog_replay_guard_test.go — the replay boundary against the REAL PathPolicy.
//
// The txlog package's own tests use a hand-rolled guard, which is unavoidable
// there (internal/tools imports txlog, so txlog cannot import it back). That
// stand-in is weaker than production on exactly the axis that matters: it
// compares paths lexically, while PathPolicy canonicalises through the nearest
// existing ancestor. An independent review found a dangling-symlink escape that
// no test in that package could have caught for precisely that reason. These
// tests live here, where both halves are importable, and drive the wiring the
// daemon actually uses.

// writeReplayOrphan builds an orphaned tx-log directory whose manifest names
// target, dated in the past so Scan treats it as recoverable.
func writeReplayOrphan(t *testing.T, ws, target, content string) {
	t.Helper()
	dir := filepath.Join(ws, ".plumb", "tx-log", "orphan")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	manifest := map[string]any{
		"tx_id":      "orphan",
		"started_at": "2000-01-01T00:00:00Z",
		"workspace":  ws,
		"ops": []map[string]any{
			{"n": 0, "path": target, "perm": 0o644, "snapshotted": true},
		},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshalling manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0o600); err != nil {
		t.Fatalf("writing manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "0-before"), []byte(content), 0o600); err != nil {
		t.Fatalf("writing snapshot: %v", err)
	}
}

// workspacePolicy mirrors buildPathPolicy's shape for a plain pinned workspace:
// the workspace itself as a read-write root.
func workspacePolicy(ws string) *tools.PathPolicy {
	return tools.NewPathPolicy(ws, []tools.AllowedRoot{
		{Path: ws, Access: tools.AccessReadWrite, Label: "workspace"},
	})
}

// TestTxlogReplayGuard_DanglingSymlinkCannotEscape is the regression test for
// the escape an independent review found in the first version of this fix.
//
// PathPolicy canonicalises a not-yet-existing path against its nearest EXISTING
// ancestor, so a DANGLING link at <ws>/payload resolves to <ws>/payload and the
// policy admits it — which is right for a caller that replaces the link, as
// safeWrite does. The replay used os.WriteFile, which follows it instead and
// creates the target wherever it points. The guard alone therefore did not close
// the hole: a repository committing `payload -> <anywhere>` still escaped.
func TestTxlogReplayGuard_DanglingSymlinkCannotEscape(t *testing.T) {
	ws := evalTempDir(t)
	outside := evalTempDir(t)
	victim := filepath.Join(outside, "victim.txt") // deliberately does NOT exist
	link := filepath.Join(ws, "payload")
	if err := os.Symlink(victim, link); err != nil {
		t.Skipf("symlinks unsupported on this filesystem: %v", err)
	}

	// The policy admits the link: that is the premise, not a bug in the policy.
	if _, err := workspacePolicy(ws).Check(link, tools.AccessReadWrite); err != nil {
		t.Fatalf("premise failed — the policy no longer admits a dangling in-workspace link "+
			"(%v). If that changed deliberately, this test's reasoning needs revisiting.", err)
	}

	writeReplayOrphan(t, ws, link, "pwned\n")
	txlog.Scan(ws, time.Now(), txlogReplayGuard(workspacePolicy(ws)))

	if b, err := os.ReadFile(victim); err == nil {
		t.Errorf("replay followed a dangling symlink and wrote outside every allowed root: %s (content %q)",
			victim, b)
	}
}

// TestTxlogReplayGuard_SymlinkedSnapshotIsNotRead covers the other direction:
// the snapshot file is read with os.ReadFile, which follows links too, and its
// content is written to an admitted in-workspace path. A repository shipping
// `0-before -> ~/.ssh/id_ed25519` would otherwise copy that file into the
// workspace, where an agent reads it or a later commit carries it away.
func TestTxlogReplayGuard_SymlinkedSnapshotIsNotRead(t *testing.T) {
	ws := evalTempDir(t)
	secretDir := evalTempDir(t)
	secret := filepath.Join(secretDir, "id_ed25519")
	if err := os.WriteFile(secret, []byte("PRIVATE KEY MATERIAL\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(ws, "harmless.txt")
	writeReplayOrphan(t, ws, target, "placeholder\n")

	// Replace the snapshot with a link to the secret.
	snap := filepath.Join(ws, ".plumb", "tx-log", "orphan", "0-before")
	if err := os.Remove(snap); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, snap); err != nil {
		t.Skipf("symlinks unsupported on this filesystem: %v", err)
	}

	txlog.Scan(ws, time.Now(), txlogReplayGuard(workspacePolicy(ws)))

	if b, err := os.ReadFile(target); err == nil && string(b) == "PRIVATE KEY MATERIAL\n" {
		t.Errorf("replay read through a symlinked snapshot and copied %s into the workspace", secret)
	}
}

// TestTxlogReplayGuard_RestoresInsideTheWorkspace is the other half: the guard
// must still permit an ordinary recovery, or the fix has traded a vulnerability
// for a silently broken feature.
func TestTxlogReplayGuard_RestoresInsideTheWorkspace(t *testing.T) {
	ws := evalTempDir(t)
	target := filepath.Join(ws, "src.go")
	writeReplayOrphan(t, ws, target, "package main\n")

	txlog.Scan(ws, time.Now(), txlogReplayGuard(workspacePolicy(ws)))

	if got, _ := os.ReadFile(target); string(got) != "package main\n" {
		t.Errorf("a legitimate in-workspace crash recovery must still restore, got %q", got)
	}
}

// TestTxlogReplayGuard_RestoresInAnExtraRoot proves the design choice that a
// plain workspace-containment check would have broken: transaction_apply
// legitimately writes to configured extra roots and --allow-dir grants, so crash
// recovery must reach them too.
func TestTxlogReplayGuard_RestoresInAnExtraRoot(t *testing.T) {
	ws := evalTempDir(t)
	extra := evalTempDir(t)
	target := filepath.Join(extra, "lib.go")
	writeReplayOrphan(t, ws, target, "package lib\n")

	pol := tools.NewPathPolicy(ws, []tools.AllowedRoot{
		{Path: ws, Access: tools.AccessReadWrite, Label: "workspace"},
		{Path: extra, Access: tools.AccessReadWrite, Label: "extra root"},
	})
	txlog.Scan(ws, time.Now(), txlogReplayGuard(pol))

	if got, _ := os.ReadFile(target); string(got) != "package lib\n" {
		t.Errorf("a path the session's policy admits must still be recoverable, got %q", got)
	}
}

// TestTxlogReplayGuard_NilPolicyFailsClosed pins the fail-closed contract at the
// wiring level rather than only inside txlog.
func TestTxlogReplayGuard_NilPolicyFailsClosed(t *testing.T) {
	if g := txlogReplayGuard(nil); g != nil {
		t.Fatal("a nil policy must yield a nil guard, which txlog.Scan treats as fail-closed")
	}
	ws := evalTempDir(t)
	target := filepath.Join(ws, "file.txt")
	writeReplayOrphan(t, ws, target, "restored\n")

	txlog.Scan(ws, time.Now(), txlogReplayGuard(nil))

	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Error("a nil policy must refuse every op, even one inside the workspace")
	}
}

// evalTempDir returns a t.TempDir with symlinks resolved. On macOS the temp root
// is /var, itself a link to /private/var, and the boundary comparison is on
// resolved paths — an unresolved root makes a policy refuse its own workspace,
// which silently turns a confinement test green for the wrong reason.
func evalTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return dir
	}
	return resolved
}
