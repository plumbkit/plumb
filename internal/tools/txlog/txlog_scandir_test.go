package txlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// txlog_scandir_test.go — Scan's DESTRUCTIVE half.
//
// The replay's write half gets the attention, but Scan deletes before it writes:
// it walks <workspace>/.plumb/tx-log and RemoveAlls every entry that looks like
// an orphan. Both `.plumb` and `tx-log` are ordinary paths inside the workspace,
// and git stores a symlink natively, so a cloned repository chooses where that
// walk lands. No manifest is involved — attaching is enough.

// plantSymlinkedTxLog points <ws>/.plumb/tx-log at target and returns ws.
func plantSymlinkedTxLog(t *testing.T, target string) string {
	t.Helper()
	ws := resolvedTempDir(t)
	if err := os.MkdirAll(filepath.Join(ws, ".plumb"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(ws, ".plumb", "tx-log")); err != nil {
		t.Skipf("symlinks unsupported on this filesystem: %v", err)
	}
	return ws
}

// resolvedTempDir returns a t.TempDir with symlinks resolved. On macOS the temp
// root is /var, itself a link to /private/var, and the containment comparison is
// on resolved paths — an unresolved root makes the check reject the workspace's
// own tx-log dir, which would turn these tests green for the wrong reason.
func resolvedTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		return resolved
	}
	return dir
}

// TestScan_RefusesSymlinkedTxLogDir is the regression test for arbitrary
// recursive directory deletion. Against the unfixed code this removes every
// subdirectory of the link target.
func TestScan_RefusesSymlinkedTxLogDir(t *testing.T) {
	victim := resolvedTempDir(t)
	for _, name := range []string{"Documents", "Desktop", ".ssh"} {
		if err := os.MkdirAll(filepath.Join(victim, name, "keep"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ws := plantSymlinkedTxLog(t, victim)

	Scan(ws, time.Now(), confinedTo(ws))

	for _, name := range []string{"Documents", "Desktop", ".ssh"} {
		if _, err := os.Stat(filepath.Join(victim, name)); os.IsNotExist(err) {
			t.Errorf("Scan deleted %s, outside the workspace entirely", name)
		}
	}
}

// TestScan_RefusesRelativeSymlinkedTxLogDir covers the same shape without the
// attacker needing to know any absolute path: `.plumb/tx-log -> ../..` walks out
// of the workspace by relative steps alone, which is what makes the payload
// portable across machines.
func TestScan_RefusesRelativeSymlinkedTxLogDir(t *testing.T) {
	parent := resolvedTempDir(t)
	sibling := filepath.Join(parent, "other-project")
	if err := os.MkdirAll(filepath.Join(sibling, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(parent, "workspace")
	if err := os.MkdirAll(filepath.Join(ws, ".plumb"), 0o755); err != nil {
		t.Fatal(err)
	}
	// From <ws>/.plumb/tx-log, "../.." is <parent>.
	if err := os.Symlink("../..", filepath.Join(ws, ".plumb", "tx-log")); err != nil {
		t.Skipf("symlinks unsupported on this filesystem: %v", err)
	}

	Scan(ws, time.Now(), confinedTo(ws))

	if _, err := os.Stat(sibling); os.IsNotExist(err) {
		t.Error("Scan deleted a sibling project reached by a relative symlink")
	}
}

// TestScan_RefusesSymlinkedPlumbDir pins the intermediate-component case. Only
// `.plumb` is a link here, so `tx-log` itself is a real directory and an
// Lstat-the-final-component check would wave it through.
func TestScan_RefusesSymlinkedPlumbDir(t *testing.T) {
	victim := resolvedTempDir(t)
	if err := os.MkdirAll(filepath.Join(victim, "tx-log", "doomed"), 0o755); err != nil {
		t.Fatal(err)
	}
	ws := resolvedTempDir(t)
	if err := os.Symlink(victim, filepath.Join(ws, ".plumb")); err != nil {
		t.Skipf("symlinks unsupported on this filesystem: %v", err)
	}

	Scan(ws, time.Now(), confinedTo(ws))

	if _, err := os.Stat(filepath.Join(victim, "tx-log", "doomed")); os.IsNotExist(err) {
		t.Error("Scan deleted through a symlinked .plumb directory")
	}
}

// TestScan_StillRecoversAGenuineOrphan is the positive control. Without it every
// test above would pass just as well against a Scan that did nothing at all —
// which is the failure mode a refusal-shaped fix invites.
func TestScan_StillRecoversAGenuineOrphan(t *testing.T) {
	ws := initWorkspace(t)
	target := filepath.Join(ws, "file.txt")
	if err := os.WriteFile(target, []byte("after"), 0o644); err != nil {
		t.Fatal(err)
	}
	txDir := filepath.Join(ws, ".plumb", "tx-log", "orphan")
	if err := os.MkdirAll(txDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeOrphanManifest(t, txDir, ws, target)

	Scan(ws, time.Now().Add(time.Hour), confinedTo(ws))

	if got, _ := os.ReadFile(target); string(got) != "before" {
		t.Errorf("a real orphan must still be recovered, got %q", got)
	}
	if _, err := os.Stat(txDir); !os.IsNotExist(err) {
		t.Error("a recovered orphan's directory should be removed")
	}
}

// writeOrphanManifest writes a past-dated manifest restoring target to "before".
func writeOrphanManifest(t *testing.T, txDir, ws, target string) {
	t.Helper()
	m := txManifest{
		TxID:      "orphan",
		StartedAt: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC),
		Workspace: ws,
		Ops:       []opMeta{{N: 0, Path: target, Perm: 0o644, Snapshotted: true}},
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(txDir, "manifest.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(txDir, "0-before"), []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
}
