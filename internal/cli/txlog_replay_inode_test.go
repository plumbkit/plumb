package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/tools/txlog"
)

// txlog_replay_inode_test.go — the confinement at the INODE level.
//
// An independent review of the redesign found the guard true of the NAME and
// false of the FILE. os.WriteFile opens the existing inode and truncates it, so
// a hardlink at an admitted in-workspace name wrote through to a file outside
// every allowed root; the symlink refusal cannot see a hardlink, because Lstat
// reports an ordinary regular file. The write now goes through the same
// stage-and-rename primitive as every other durable write in the tree, which
// replaces the directory entry and leaves any other link to the old inode
// alone, and a multiply-linked snapshot is refused outright.
func TestTxlogReplayGuard_HardlinkCannotBeWrittenThrough(t *testing.T) {
	ws := evalTempDir(t)
	outside := evalTempDir(t)
	secret := filepath.Join(outside, "authorized_keys")
	if err := os.WriteFile(secret, []byte("original\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A hardlink: same inode, in-workspace name. Not a symlink, so Lstat sees a
	// regular file and replayTarget returns the path unchanged.
	link := filepath.Join(ws, "innocent.txt")
	if err := os.Link(secret, link); err != nil {
		t.Skipf("hardlinks unsupported: %v", err)
	}
	writeReplayOrphan(t, ws, link, "ssh-rsa ATTACKER\n")

	txlogScanForTest(t, ws)

	if b, _ := os.ReadFile(secret); string(b) == "ssh-rsa ATTACKER\n" {
		t.Errorf("replay wrote through a hardlink into %s, outside every allowed root", secret)
	}
}

func TestTxlogReplayGuard_HardlinkedSnapshotIsNotRead(t *testing.T) {
	ws := evalTempDir(t)
	outside := evalTempDir(t)
	secret := filepath.Join(outside, "id_ed25519")
	if err := os.WriteFile(secret, []byte("PRIVATE KEY MATERIAL\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(ws, "harmless.txt")
	writeReplayOrphan(t, ws, target, "placeholder\n")

	snap := filepath.Join(ws, ".plumb", "tx-log", "orphan", "0-before")
	if err := os.Remove(snap); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(secret, snap); err != nil {
		t.Skipf("hardlinks unsupported: %v", err)
	}

	txlogScanForTest(t, ws)

	if b, _ := os.ReadFile(target); string(b) == "PRIVATE KEY MATERIAL\n" {
		t.Errorf("a hardlinked snapshot copied %s into the workspace", secret)
	}
}

// TestTxlogReplayGuard_ResolvesSymlinkedAncestors covers the asymmetry the
// review found in the re-check: replayTarget used to call EvalSymlinks only when
// the FINAL component was a link, so a link in an ANCESTOR left the target equal
// to the input and the re-check never ran — the exact hole it exists to close.
// Driven with a lexical guard, which is the shape the re-check defends against.
func TestTxlogReplayGuard_ResolvesSymlinkedAncestors(t *testing.T) {
	ws := evalTempDir(t)
	outside := evalTempDir(t)
	victim := filepath.Join(outside, "victim.txt")
	if err := os.WriteFile(victim, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(ws, "sub")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	// A guard that judges the path AS WRITTEN without resolving it: admits
	// in-workspace spellings, refuses out-of-workspace ones. (A guard that
	// admitted everything would prove nothing.)
	lexical := func(p string) error {
		rel, err := filepath.Rel(ws, filepath.Clean(p))
		if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
			return os.ErrPermission
		}
		return nil
	}
	writeReplayOrphan(t, ws, filepath.Join(ws, "sub", "victim.txt"), "pwned\n")

	txlogScanWithGuard(t, ws, lexical)

	if b, _ := os.ReadFile(victim); string(b) == "pwned\n" {
		t.Errorf("replay wrote through a symlinked ancestor to %s", victim)
	}
}

func txlogScanForTest(t *testing.T, ws string) {
	t.Helper()
	txlogScanWithGuard(t, ws, txlogReplayGuard(workspacePolicy(ws)))
}

func txlogScanWithGuard(t *testing.T, ws string, g func(string) error) {
	t.Helper()
	txlog.Scan(ws, time.Now(), g)
}
