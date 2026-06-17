package tools

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// gitWriteDraining gates NEW index/ref-mutating git operations during daemon
// shutdown. Once BeginGitWriteDrain runs (wired to daemon-context cancellation)
// a fresh serialised git op is refused with errGitDraining instead of starting,
// so the shutdown window cannot spawn a new git child that a forced process exit
// would then SIGKILL mid-write and strand an .git/index.lock. In-flight ops are
// unaffected: their child runs under a cancellation-decoupled, bounded context
// (see runGit) so git finishes and releases the lock itself.
//
// Concurrency: a process-global atomic flag plus a WaitGroup counting in-flight
// serialised ops. Set-once for the daemon's lifetime; tests reset it directly.
var gitWriteDraining atomic.Bool

// gitWriteInflight counts serialised git ops currently executing so shutdown can
// wait a bounded window for them before the process exits.
var gitWriteInflight sync.WaitGroup

// errGitDraining is returned when a git write is attempted while the daemon is
// draining for shutdown.
var errGitDraining = errors.New("daemon is shutting down; git writes are paused — retry once it is back up")

// gitWriteGrace bounds an index/ref-mutating git child once it is decoupled from
// request/daemon cancellation. Generous enough for a slow pre-commit hook
// (go build + golangci-lint), so a normal commit always finishes rather than
// being SIGKILLed mid-write; a genuinely wedged child is still bounded.
const gitWriteGrace = 2 * time.Minute

// BeginGitWriteDrain marks the daemon as draining: no new serialised git op is
// admitted. Idempotent; safe to call from the shutdown goroutine.
func BeginGitWriteDrain() { gitWriteDraining.Store(true) }

// gitWriteDrainActive reports whether new git writes are currently paused.
func gitWriteDrainActive() bool { return gitWriteDraining.Load() }

// WaitGitWritesDrained blocks up to d for in-flight serialised git ops to finish,
// returning true if they all completed within the window. Best-effort: the
// decoupled exec context already lets a started commit finish past process exit,
// so a false return only means the daemon did not wait for them, not that they
// were aborted.
func WaitGitWritesDrained(d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		gitWriteInflight.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}
