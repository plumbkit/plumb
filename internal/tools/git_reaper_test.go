package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// deadPID runs `true` to completion and returns its (now-reaped, dead) pid.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Skipf("cannot run a throwaway process: %v", err)
	}
	return cmd.Process.Pid
}

// stageLock writes a fake .git/index.lock and, when owner != nil, an owner
// sidecar, under a fresh temp repo root. Returns the root and the lock path.
func stageLock(t *testing.T, owner *gitLockOwner) (string, string) {
	t.Helper()
	root := t.TempDir()
	gitDir := filepath.Join(root, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(gitDir, "index.lock")
	if err := os.WriteFile(lock, []byte("0"), 0o644); err != nil {
		t.Fatal(err)
	}
	if owner != nil {
		data, _ := json.Marshal(owner)
		if err := os.WriteFile(gitOwnerPath(root), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root, lock
}

func TestReapStaleGitLock_DeadOwnerReaped(t *testing.T) {
	root, lock := stageLock(t, &gitLockOwner{PID: deadPID(t), Host: thisHost()})
	if !reapStaleGitLock(root) {
		t.Fatal("expected the stale lock of a dead, this-host plumb owner to be reaped")
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Error("index.lock should be gone after a reap")
	}
	if _, err := os.Stat(gitOwnerPath(root)); !os.IsNotExist(err) {
		t.Error("owner sidecar should be removed with the lock")
	}
}

func TestReapStaleGitLock_LiveOwnerKept(t *testing.T) {
	// os.Getpid() is this live test process — a live owner must never be reaped.
	root, lock := stageLock(t, &gitLockOwner{PID: os.Getpid(), Host: thisHost()})
	if reapStaleGitLock(root) {
		t.Fatal("a lock owned by a live process must not be reaped")
	}
	if _, err := os.Stat(lock); err != nil {
		t.Error("index.lock should remain when the owner is alive")
	}
}

func TestReapStaleGitLock_NoSidecarKept(t *testing.T) {
	// An external git (no plumb sidecar) holds it — leave it (today's safe default).
	root, lock := stageLock(t, nil)
	if reapStaleGitLock(root) {
		t.Fatal("a lock with no attributable owner must not be reaped")
	}
	if _, err := os.Stat(lock); err != nil {
		t.Error("index.lock should remain when unattributable")
	}
}

func TestReapStaleGitLock_DifferentHostKept(t *testing.T) {
	root, lock := stageLock(t, &gitLockOwner{PID: deadPID(t), Host: thisHost() + "-elsewhere"})
	if reapStaleGitLock(root) {
		t.Fatal("a lock owned on a different host (shared worktree) must not be reaped")
	}
	if _, err := os.Stat(lock); err != nil {
		t.Error("index.lock should remain when the owner host differs")
	}
}

func TestReapStaleGitLock_NoLockNoop(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if reapStaleGitLock(root) {
		t.Fatal("reap must be a no-op when no index.lock exists")
	}
}

func TestRecordAndClearGitLockOwner(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	recordGitLockOwner(root, 4242)
	data, err := os.ReadFile(gitOwnerPath(root))
	if err != nil {
		t.Fatalf("sidecar should exist after record: %v", err)
	}
	var owner gitLockOwner
	if err := json.Unmarshal(data, &owner); err != nil {
		t.Fatalf("sidecar JSON: %v", err)
	}
	if owner.PID != 4242 || owner.Host != thisHost() {
		t.Errorf("sidecar = %+v, want pid 4242 / host %q", owner, thisHost())
	}
	clearGitLockOwner(root)
	if _, err := os.Stat(gitOwnerPath(root)); !os.IsNotExist(err) {
		t.Error("sidecar should be gone after clear")
	}
}

func TestBeginSerialisedGit_RefusedWhileDraining(t *testing.T) {
	gitWriteDraining.Store(true)
	t.Cleanup(func() { gitWriteDraining.Store(false) })

	_, cleanup, err := beginSerialisedGit(context.Background(), t.TempDir(), "commit", tierWrite)
	if err == nil {
		if cleanup != nil {
			cleanup()
		}
		t.Fatal("expected a draining daemon to refuse a new git write")
	}
}

func TestWaitGitWritesDrained_NoInflightReturnsTrue(t *testing.T) {
	if !WaitGitWritesDrained(2 * time.Second) {
		t.Fatal("WaitGitWritesDrained should return true when nothing is in flight")
	}
}
