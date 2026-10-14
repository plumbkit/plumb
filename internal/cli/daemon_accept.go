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
	// Idle-session reaper: cancel connections that have not called any tool
	// for longer than the configured eviction TTL, prune expired persisted
	// per-connection state, and prune expired collab intents/notes on the same tick.
	go func() {
		ticker := time.NewTicker(reaperInterval)
		defer ticker.Stop()
		runIdleReaper(ctx, store, registry, sessState, collabPool, ticker.C)
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
	// Stop admitting new git writes the instant shutdown begins, so the shutdown
	// window cannot spawn a git child that a forced exit would strand mid-commit.
	// In-flight writes finish under their decoupled context (see tools.runGit).
	serveConnections(ctx, guard, bridge, handle, tools.BeginGitWriteDrain)

	// Give an in-flight git write a brief, bounded chance to finish (and release
	// index.lock) before the process exits, so a quick commit is not orphaned. A
	// slower one still completes under its decoupled context past exit.
	if !tools.WaitGitWritesDrained(gitWriteDrainGrace) {
		slog.Warn("daemon: a git write was still in flight at shutdown; it continues under its decoupled context")
	}
}

// serveConnections is the accept loop proper: every connection, on the account
// socket or a bridge socket, goes to handle on one WaitGroup, and when ctx ends
// the listeners close — the bridge's first, and its accept loops are waited for,
// so nothing can join the group once the drain has begun — and in-flight
// connections get acceptDrainGrace to finish. beginShutdown runs first.
func serveConnections(ctx context.Context, guard *socketGuard, bridge *legacyBridge, handle func(net.Conn), beginShutdown func()) {
	var wg sync.WaitGroup
	go func() {
		<-ctx.Done()
		beginShutdown()
		bridge.close()
		guard.close()
	}()
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
				return
			default:
				slog.Error("daemon: accept", "err", err)
				continue
			}
		}
		wg.Go(func() { handle(conn) })
	}
}
