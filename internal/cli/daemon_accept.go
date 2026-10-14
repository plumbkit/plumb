package cli

import (
	"context"
	"log/slog"
	"net"
	"runtime/debug"
	"sync"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/sessionstate"
	"github.com/plumbkit/plumb/internal/tools"
)

// runDaemonAcceptLoop serves connections until ctx is cancelled: those on the
// account socket (guard) and, through the same handler and drain, those on the
// legacy bridge sockets (PLAN-549).
func runDaemonAcceptLoop(ctx context.Context, guard *socketGuard, bridge *legacyBridge, pool *workspacePool, topoPool *topologyPool, memPool *memoryIndexPool, collabPool *collabPool, store *config.Store, statsStore *statsStore, historyStore *historyStore, sessState *sessionstate.Store, daemonStartedAt time.Time, budgets *sharedBudgets, registry *connRegistry, projCfgWatches *projectConfigWatchManager) {
	var wg sync.WaitGroup

	// Idle-session reaper: cancel connections that have not called any tool
	// for longer than the configured eviction TTL, prune expired persisted
	// per-connection state, and prune expired collab intents/notes on the same tick.
	go func() {
		ticker := time.NewTicker(reaperInterval)
		defer ticker.Stop()
		runIdleReaper(ctx, store, registry, sessState, collabPool, ticker.C)
	}()

	go func() {
		<-ctx.Done()
		// Stop admitting new git writes the instant shutdown begins, so the
		// shutdown window cannot spawn a git child that a forced exit would
		// strand mid-commit. In-flight writes finish under their decoupled
		// context (see tools.runGit).
		tools.BeginGitWriteDrain()
		guard.close()
		bridge.close()
	}()

	handle := func(conn net.Conn) {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("daemon: connection goroutine panic — daemon kept alive",
					"err", r,
					"stack", string(debug.Stack()))
			}
		}()
		handleConn(ctx, conn, pool, topoPool, memPool, collabPool, store, statsStore, historyStore, sessState, daemonStartedAt, budgets, registry, projCfgWatches)
	}
	// The legacy bridge's clients are served by the same handler and drained
	// with the same group as every other connection.
	bridge.serve(&wg, handle)

	for {
		conn, err := guard.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				if !drainConnections(&wg, acceptDrainGrace) {
					slog.Warn("daemon: in-flight requests did not drain before the shutdown deadline; proceeding")
				}
				// Give an in-flight git write a brief, bounded chance to finish
				// (and release index.lock) before the process exits, so a quick
				// commit is not orphaned. A slower one still completes under its
				// decoupled context past exit.
				if !tools.WaitGitWritesDrained(gitWriteDrainGrace) {
					slog.Warn("daemon: a git write was still in flight at shutdown; it continues under its decoupled context")
				}
				return
			default:
				slog.Error("daemon: accept", "err", err)
				continue
			}
		}
		wg.Go(func() { handle(conn) })
	}
}
