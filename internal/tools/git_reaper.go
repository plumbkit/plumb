package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

// gitLockOwner records the GIT CHILD process responsible for an index.lock, so a
// later op (or a fresh daemon) can attribute a stranded lock and reap it safely.
// Written beside the lock for the window the git child runs; removed on
// completion. A hard SIGKILL of the git child leaves it behind — exactly the
// signal the reaper needs. It records the git child's pid, NOT the daemon's: the
// child is what holds the lock, and on a `plumb restart` the daemon pid dies
// while a cancellation-decoupled child may still be finishing the commit — using
// the daemon pid would falsely reap that live child's lock.
type gitLockOwner struct {
	PID  int    `json:"pid"`
	Host string `json:"host"`
}

var (
	gitHostOnce sync.Once
	gitHostname string
)

func thisHost() string {
	gitHostOnce.Do(func() {
		h, err := os.Hostname()
		if err != nil || h == "" {
			h = "unknown"
		}
		gitHostname = h
	})
	return gitHostname
}

// gitLockPath and gitOwnerPath name the index lock and our owner sidecar for the
// main worktree of repoRoot. (Linked-worktree index locks live under
// .git/worktrees/<name>/; matching the existing indexLockHint, the main worktree
// is handled — a linked-worktree strand still falls back to the manual hint.)
func gitLockPath(repoRoot string) string { return filepath.Join(repoRoot, ".git", "index.lock") }

func gitOwnerPath(repoRoot string) string {
	return filepath.Join(repoRoot, ".git", "plumb-index-owner.json")
}

// recordGitLockOwner stamps the owner sidecar with the running git child's pid.
// Best-effort: a write failure never blocks the git op (it only forgoes later
// attributable recovery). Called after the child has started, while holding the
// per-repo lock.
func recordGitLockOwner(repoRoot string, childPID int) {
	owner := gitLockOwner{PID: childPID, Host: thisHost()}
	data, err := json.Marshal(owner)
	if err != nil {
		return
	}
	_ = os.WriteFile(gitOwnerPath(repoRoot), data, 0o600)
}

// clearGitLockOwner removes the owner sidecar once the git op has finished (the
// lock is gone). Best-effort. A missing file is fine.
func clearGitLockOwner(repoRoot string) {
	_ = os.Remove(gitOwnerPath(repoRoot))
}

// reapStaleGitLock removes an .git/index.lock that is provably owned by a DEAD
// plumb-spawned process on THIS machine, and returns true when it did. It is the
// principled form of the auto-remove plumb otherwise refuses (indexLockHint):
// the ownership sidecar makes the lock attributable, so a real concurrent commit
// — external git (no sidecar), a live process (pid alive), or another machine
// (host differs) — is always left untouched. The bar is conservative by design:
// a false negative merely reproduces today's manual-hint behaviour; a false
// positive risks index corruption.
//
// It must be called while holding the per-repo lock, so no in-daemon peer is
// mid-write on this repo. Because a stranded plumb lock blocks every other git
// process, an index.lock seen here alongside a dead-owner sidecar is that same
// stranded lock (an external git could not have replaced it while it existed).
func reapStaleGitLock(repoRoot string) bool {
	lock := gitLockPath(repoRoot)
	if _, err := os.Stat(lock); err != nil {
		return false // no lock present — nothing to reap
	}
	data, err := os.ReadFile(gitOwnerPath(repoRoot))
	if err != nil {
		return false // not plumb-attributable — leave it (today's safe default)
	}
	var owner gitLockOwner
	if err := json.Unmarshal(data, &owner); err != nil {
		return false
	}
	if owner.Host != thisHost() {
		return false // a different machine (shared/network worktree) may hold it
	}
	if gitProcessAlive(owner.PID) {
		return false // a live process holds it — possibly this very daemon
	}
	// The owning plumb process is dead and was on this host: the lock is stale.
	if err := os.Remove(lock); err != nil {
		return false
	}
	_ = os.Remove(gitOwnerPath(repoRoot))
	return true
}

// gitProcessAlive reports whether a process with the given pid is running.
// Conservative: an error or a non-positive pid reads as "not alive" so the
// reaper only ever removes a lock whose owner is provably gone.
func gitProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}
