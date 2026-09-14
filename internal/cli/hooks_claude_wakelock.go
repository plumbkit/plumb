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
		// saying so.
		return unstampedLockIsDebris(lock)
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

// claudeUnstampedLockGrace is how long an unstamped lock is treated as a
// watcher still mid-acquire rather than as debris.
//
// The gap being covered is the one between acquireWakeLockWith's os.Mkdir and
// the os.WriteFile that records the pid — adjacent statements, microseconds
// apart. A minute is six orders of magnitude of slack over that, and nothing
// about it should scale with the WATCH window: this lock's holder never lived
// long enough to watch anything. Deriving it from the window was always
// arbitrary, and once the ceiling became an hour it was actively harmful — a
// session that lost the race would have been silently unwakeable for that hour
// rather than for the few minutes it used to be.
const claudeUnstampedLockGrace = 60 * time.Second

// unstampedLockIsDebris reports whether a lock with no readable pid is old
// enough that no watcher can still be mid-acquire behind it.
func unstampedLockIsDebris(lock string) bool {
	info, err := os.Stat(lock)
	if err != nil {
		return false
	}
	return time.Since(info.ModTime()) > claudeUnstampedLockGrace
}

// supersededBy reports whether a LIVE watcher has taken over this session.
//
// A name that differs from this watcher's key is necessary but nowhere near
// sufficient, and each extra condition is load-bearing:
//
//   - A session can be renamed mid-watch (plumb's own self-test tells an agent to
//     rename and rename back) and a daemon restart can relabel one. The probe
//     then resolves the RIGHT session under a new name with nothing having
//     replaced this watcher, so retiring on the name alone strands it.
//   - The lock existing is not proof either, and in THIS file that is a trap
//     rather than a technicality: lock directories are deliberately never swept
//     (see sweepWakeDir), so one left by a watcher that was killed before it
//     could release — `plumb restart`, a SIGKILL, a reboot — sits there forever.
//     Trusting it would let a corpse retire the session's ONLY watcher: keyed by
//     conversation id because the daemon was down, resolving to a name whose
//     stale lock outlived its process, standing down, and leaving an idle session
//     with nothing watching and no next turn to re-arm.
//
// So the lock must be held by a live plumb. Every uncertain reading resolves to
// "not superseded", which keeps this watcher alive: the worst case is a transient
// duplicate, against a silently lost wake. That is why isPlumb's fail-open
// behaviour is safe here and was not in the sweep — there a failed `ps` deleted a
// live lock, here it merely keeps watching.
//
// The resolved name is run through wakeStampKey rather than used raw: it reaches
// here from the daemon and is about to be joined onto a path.
//
// supersededByWith takes the process-identity test as a parameter for the same
// reason acquireWakeLockWith does: a test binary is never a plumb process, so
// with the real check wired in the "a live plumb holds it" branch — the one that
// decides whether a watcher retires — could not be reached from a test at all.
func supersededByWith(report mailReport, key string, isPlumb func(int) bool) bool {
	name := wakeStampKey(report, "")
	if name == "" || name == key {
		return false
	}
	lock := filepath.Join(wakeDir(), name+".lock")
	if info, err := os.Stat(lock); err != nil || !info.IsDir() {
		return false
	}
	raw, err := os.ReadFile(filepath.Join(lock, "pid")) //nolint:gosec // G304: inside plumb's own wake dir
	if err != nil {
		// Unstamped: either a replacement mid-acquire, or debris. Both read as
		// "keep watching" — a replacement that really is arming will have its pid
		// recorded by the next poll.
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return false
	}
	return processAlive(pid) && isPlumb(pid)
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
