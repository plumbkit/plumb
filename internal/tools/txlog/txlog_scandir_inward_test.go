package txlog

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// txlog_scandir_inward_test.go — the payloads that point BACK INTO the
// workspace.
//
// The first version of this fix asked whether the resolved tx-log directory was
// INSIDE the workspace. That admits the workspace root itself (`rel == "."`) and
// every directory beneath it, so a link one character shorter than the `../..`
// its own regression test used still handed Scan a directory full of
// "orphaned transactions" to RemoveAll. Found by an independent review.
//
// The predicate is now identity: exactly <workspace>/.plumb/tx-log, or refuse.

// plantVictimTree builds a workspace with the directories a real repository has,
// plus a committed `.plumb/tx-log` symlink pointing at target.
func plantVictimTree(t *testing.T, target string) string {
	t.Helper()
	ws := resolvedTempDir(t)
	for _, d := range []string{".git/objects", ".git/refs", ".plumb/memories", "src"} {
		if err := os.MkdirAll(filepath.Join(ws, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(target, filepath.Join(ws, ".plumb", "tx-log")); err != nil {
		t.Skipf("symlinks unsupported on this filesystem: %v", err)
	}
	return ws
}

func TestScan_RefusesTxLogPointingAtTheWorkspaceRoot(t *testing.T) {
	// From <ws>/.plumb/tx-log, ".." is <ws>/.plumb's parent — the workspace root.
	// Under a containment check this is rel == "." and was ALLOWED.
	ws := plantVictimTree(t, "..")

	Scan(ws, time.Now())

	for _, survivor := range []string{".git", "src", ".plumb/memories"} {
		if _, err := os.Stat(filepath.Join(ws, survivor)); os.IsNotExist(err) {
			t.Errorf("Scan deleted %s via `.plumb/tx-log -> ..`", survivor)
		}
	}
}

func TestScan_RefusesTxLogPointingAtTheGitDir(t *testing.T) {
	// The maximal payload: every subdirectory of .git is an "orphan".
	ws := plantVictimTree(t, "../.git")

	Scan(ws, time.Now())

	for _, survivor := range []string{".git/objects", ".git/refs"} {
		if _, err := os.Stat(filepath.Join(ws, survivor)); os.IsNotExist(err) {
			t.Errorf("Scan deleted %s via `.plumb/tx-log -> ../.git` — "+
				"local branches, stashes and the reflog go with it", survivor)
		}
	}
}

func TestScan_RefusesTxLogPointingAtThePlumbDir(t *testing.T) {
	ws := plantVictimTree(t, ".")

	Scan(ws, time.Now())

	if _, err := os.Stat(filepath.Join(ws, ".plumb", "memories")); os.IsNotExist(err) {
		t.Error("Scan deleted .plumb/memories via `.plumb/tx-log -> .`")
	}
}

// TestScan_MissingTxLogDirIsSilent pins the other half of the review's finding:
// the refusal is logged at ERROR, and a workspace that has never run a
// transaction has no tx-log at all. Firing the alarm on every attach of every
// clean workspace is how the one message that signals a live attack stops being
// read.
//
// The log output is CAPTURED and asserted on. An earlier version of this test
// checked only that Scan did not panic, which passed just as well with the
// early return deleted — i.e. it did not test the thing it was named for.
func TestScan_MissingTxLogDirIsSilent(t *testing.T) {
	ws := resolvedTempDir(t)
	logDir := filepath.Join(ws, ".plumb", "tx-log")

	if logDirIsTheRealTxLogDir(ws, logDir) {
		t.Fatal("premise: a missing tx-log dir cannot resolve, so the guard says refuse — " +
			"which is why Scan must return before consulting it")
	}
	if _, err := os.Lstat(logDir); !os.IsNotExist(err) {
		t.Fatalf("premise failed: the tx-log dir should not exist, got %v", err)
	}

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	Scan(ws, time.Now())

	if strings.Contains(buf.String(), "refusing to scan") {
		t.Errorf("a workspace that has never run a transaction logged an attack refusal:\n%s", buf.String())
	}
}

// TestScan_RealAttackStillLogs is the other side of the pair: silencing the
// missing-directory case must not silence the case worth alerting on.
func TestScan_RealAttackStillLogs(t *testing.T) {
	ws := plantVictimTree(t, "..")

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	Scan(ws, time.Now())

	if !strings.Contains(buf.String(), "refusing to scan") {
		t.Errorf("a hostile tx-log symlink was refused without saying so:\n%s", buf.String())
	}
}

// TestScan_StillRecoversThroughASymlinkedWorkspaceRoot is the control for the
// identity predicate. macOS temp dirs live under /var -> /private/var, so the
// workspace root is routinely a symlink; resolving only one side would refuse
// every genuine workspace on this platform.
func TestScan_StillRecoversThroughASymlinkedWorkspaceRoot(t *testing.T) {
	targetWS := resolvedTempDir(t)
	link := filepath.Join(resolvedTempDir(t), "ws-link")
	if err := os.Symlink(targetWS, link); err != nil {
		t.Skipf("symlinks unsupported on this filesystem: %v", err)
	}
	orphan := filepath.Join(targetWS, ".plumb", "tx-log", "old-tx")
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}

	Scan(link, time.Now())

	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Error("a genuine orphan under a symlinked workspace root was not recovered — " +
			"the identity check must resolve both sides, not just the log dir")
	}
}
