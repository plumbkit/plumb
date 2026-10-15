package lsp

import (
	"log/slog"
	"sync"

	"github.com/plumbkit/plumb/internal/procgroup"
)

// groupRegistry is the set of process groups the supervisors' language servers
// currently lead. Each server is the leader of its own group (see
// Supervisor.newCommand), so a group id is the server's pid.
//
// It exists so a path that cannot reach the supervisors, chiefly the daemon's
// shutdown watchdog, which fires precisely when an orderly Stop has wedged, can
// still kill every server and its descendants before the process exits. A
// supervisor adds its group right after a successful start and removes it only
// after the final sweep, so an id in the set always names a group this process
// started and has not yet reaped.
//
// Concurrency: safe for concurrent use; the mutex guards the map only. Signals
// are sent after the lock is released, so a slow kill never blocks a start.
type groupRegistry struct {
	mu    sync.Mutex
	pgids map[int]struct{}
}

// liveGroups is process-wide on purpose, one of the few globals plumb allows:
// the watchdog that calls KillAllGroups has no handle on the pools and
// supervisors it is backstopping (an entry abandoned at its budget has already
// left every pool map), and the point of the call is to need none.
var liveGroups groupRegistry

func (r *groupRegistry) add(pgid int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pgids == nil {
		r.pgids = make(map[int]struct{})
	}
	r.pgids[pgid] = struct{}{}
}

func (r *groupRegistry) remove(pgid int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.pgids, pgid)
}

func (r *groupRegistry) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pgids)
}

// snapshot returns the live group ids without holding the lock while the caller
// signals them.
func (r *groupRegistry) snapshot() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]int, 0, len(r.pgids))
	for id := range r.pgids {
		ids = append(ids, id)
	}
	return ids
}

// KillAllGroups SIGKILLs every language-server process group this process still
// owns and returns how many it signalled. It is the daemon's last act before
// exiting, from the shutdown watchdog and after the pools have closed, so a
// server that an orderly Stop abandoned does not outlive the daemon.
//
// There is no graceful phase: the caller is about to exit and has already spent
// its grace. It does not stop the supervisors, which will see their servers
// exit and schedule a restart, so call it only when the process is going away.
// A server spawned concurrently with the call may miss it; on Linux the
// parent-death signal then covers the direct child, and on every platform the
// server sees its stdin close when the daemon exits.
func KillAllGroups() int {
	killed := 0
	for _, pgid := range liveGroups.snapshot() {
		err := procgroup.Kill(pgid)
		if err == nil {
			killed++
			continue
		}
		slog.Debug("lsp: killing process group failed", "pgid", pgid, "err", err)
	}
	return killed
}

// reapGroup runs after a server's leader has been waited for. The leader is
// gone, but descendants that stayed in its group are not: SIGKILL them now (an
// ESRCH for an empty group is the normal case) and forget the group. Doing it
// right after Wait, rather than leaving it to a timer, is what keeps the group
// id from being recycled for an unrelated process before it is signalled.
func reapGroup(pgid int) {
	_ = procgroup.Kill(pgid)
	liveGroups.remove(pgid)
}
