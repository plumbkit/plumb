package cli

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The Stop hook's single-instance guard: one watcher process per session,
// reclaimable when its holder is dead, a stranger, or a previous tenant of a
// reused session name. Split from hooks_claude.go, which owns the handlers and
// the watch loop; everything here is about who may hold the lock directory.

// wakeLock is the single-instance guard. Repeated turns must not stack
// watchers: a busy workspace runs many sessions, and one leaked watcher process
// per turn is not acceptable. mkdir is the atomic primitive; the pid inside
// lets a dead lock be reclaimed.
type wakeLock struct {
	dir      string
	conv     string
	released bool
}

// release drops this watcher's lock, once — but only while the lock still
// records the conversation that took it. A lock reclaimed by a later tenant of
// a reused session name (see acquireWakeLock) belongs to that watcher, and
// releasing it out from under them would leave the session with no
// single-instance guard at all. The conversation is the identity that
// distinguishes them; the pid does not, since the evicted watcher may be a
// goroutine of the very process that took over.
func (l *wakeLock) release() {
	if l == nil || l.released {
		return
	}
	l.released = true
	// Delete only when ownership is PROVABLE. An unreadable conv file is the
	// successor's window between mkdir and its own stamp — failing open here
	// would delete the lock it has just taken, which is the bug this guard
	// exists to prevent. A lock left behind is reclaimed by age instead.
	owner, err := os.ReadFile(filepath.Join(l.dir, "conv")) //nolint:gosec // G304: inside plumb's own wake dir
	if err != nil || strings.TrimSpace(string(owner)) != l.conv {
		return
	}
	_ = os.RemoveAll(l.dir)
}

// acquireWakeLock takes this session's watcher slot, or reports that another
// watcher already holds it.
//
// The lock records the conversation that owns it, and that is load-bearing. The
// lock is keyed by the plumb session name once linkage resolves one — and plumb
// session names are explicitly reusable, while an async hook's watcher outlives
// its own client (reparented to init, in its own process group, so killing the
// session's process group does not reach it). A watcher outliving session
// "swift-heron" therefore still holds swift-heron.lock, with a LIVE pid, for the
// rest of its window. Without the conversation check the next session to take
// that name would find a live pid, stand down, and never arm a watcher —
// silently unwakeable, with nothing in any output saying so. A lock held by a
// different conversation is stale by definition: if the name resolved to us, we
// ARE that session now.
func acquireWakeLock(dir, key, sessionID string) (*wakeLock, bool) {
	return acquireWakeLockWith(dir, key, sessionID, isPlumbProcess)
}

// acquireWakeLockWith takes the process-identity test as a parameter so both of
// its directions are reachable from a test: a test binary is never a plumb
// process, so with the real check wired in, "the holder is a live plumb" could
// not be exercised at all — and that is the branch that decides whether a
// session can ever arm a watcher again.
func acquireWakeLockWith(dir, key, sessionID string, isPlumb func(int) bool) (*wakeLock, bool) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, false
	}
	lock := filepath.Join(dir, key+".lock")
	if err := os.Mkdir(lock, 0o755); err != nil {
		if !reclaimableLock(lock, sessionID, isPlumb) {
			return nil, false
		}
		_ = os.RemoveAll(lock)
		if err := os.Mkdir(lock, 0o755); err != nil {
			return nil, false
		}
	}
	_ = os.WriteFile(filepath.Join(lock, "pid"), []byte(strconv.Itoa(os.Getpid())), 0o600)
	_ = os.WriteFile(filepath.Join(lock, "conv"), []byte(orDash(sessionID)), 0o600)
	return &wakeLock{dir: lock, conv: orDash(sessionID)}, true
}

// reclaimableLock decides whether an existing lock may be taken over.
func reclaimableLock(lock, sessionID string, isPlumb func(int) bool) bool {
	pidRaw, _ := os.ReadFile(filepath.Join(lock, "pid"))   //nolint:gosec // G304: inside plumb's own wake dir
	convRaw, _ := os.ReadFile(filepath.Join(lock, "conv")) //nolint:gosec // G304: inside plumb's own wake dir

	pid, err := strconv.Atoi(strings.TrimSpace(string(pidRaw)))
	if err != nil {
		// No readable pid. Most likely a watcher that took the lock moments ago
		// and has not finished stamping it, so stand down — stealing it would
		// let two watchers run for one session. But a watcher that DIED in that
		// window leaves a lock nothing can ever claim, and a session that can
		// never arm a watcher is silently unwakeable with nothing in any output
		// saying so. No live watcher outlives its own window, so an unstamped
		// lock older than one is debris, not a tenant.
		return lockOutlivedAnyWatcher(lock)
	}
	// A pid that is alive but is not a plumb cannot be our watcher: the lock
	// outlived a crash or a reboot and something unrelated has taken the number.
	// Without this the stand-down is permanent whenever the same conversation
	// resumes — the third form of a defect this file has now had twice, and the
	// same check terminate() already refuses to signal without.
	if !processAlive(pid) || !isPlumb(pid) {
		return true
	}
	owner := strings.TrimSpace(string(convRaw))
	if sessionID == "" || owner == "" || owner == sessionID {
		return false // our own watcher is already running
	}
	_ = terminate(pid, isPlumb) // a previous tenant of this reused session name
	return true
}

// lockOutlivedAnyWatcher reports whether a lock is older than the longest a live
// watcher could still be holding it.
func lockOutlivedAnyWatcher(lock string) bool {
	info, err := os.Stat(lock)
	if err != nil {
		return false
	}
	return time.Since(info.ModTime()) > wakeWindow()+claudeStopTimeoutSlack
}

// terminate asks a stale watcher to stop. Failure is ignored: the lock is
// reclaimed either way, and a watcher that outlives its signal only polls a
// mailbox it can no longer wake anyone for.
//
// Our own pid is never signalled. One process is one hook run and therefore one
// conversation, so a lock recording this pid under a different conversation is
// not a stale tenant to evict — and signalling it would kill the very watcher
// about to be armed.
//
// Nor is a pid that is no longer a plumb process. A lock survives a crash, a
// SIGKILL and a reboot, after which the recorded pid is very likely to belong to
// something unrelated — "the pid exists" is not evidence it is ours. Signalling
// a stranger's process is a far worse failure than leaving a dead lock behind,
// which the caller reclaims anyway.
// It reports whether it signalled, so the refusal is observable — a guard whose
// only effect is NOT doing something is otherwise untestable, and this one
// stands between plumb and a stranger's process.
func terminate(pid int, isPlumb func(int) bool) bool {
	if pid <= 0 || pid == os.Getpid() || !isPlumb(pid) {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.SIGTERM) == nil
}
