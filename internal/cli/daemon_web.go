package cli

import (
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/monitor"
	"github.com/plumbkit/plumb/internal/web"
)

// daemonWebDeps is the web dashboard's view of the daemon: the live config
// store, the read-path snapshot files, and the never-create accessors for the
// cross-project collab store and each workspace's LIVE topology health.
//
// It is a function rather than a literal inside runDaemon so a test can build
// the SAME deps the daemon serves from (PLAN-489). A test that assembled its own
// web.Deps would keep passing with the TopologyHealth wiring deleted from here,
// and without that wiring the dashboard's failing flag is structurally false —
// the "index is fine" answer the flag exists to stop giving.
//
// Only HALF of it is injectable, deliberately: store, collabPool, topoPool and
// startedAt are parameters, while MetricsPath and LogPath are resolved here from
// process-wide state (monitor.SnapshotPath, daemonLogPath) that a caller cannot
// vary. A test that needs a different metrics or log path must add a parameter;
// it cannot pass one through this signature today.
func daemonWebDeps(store *config.Store, collabPool *collabPool, topoPool *topologyPool, startedAt time.Time) web.Deps {
	return web.Deps{
		Store:       store,
		MetricsPath: monitor.SnapshotPath(),
		LogPath:     daemonLogPath(),
		StartedAt:   startedAt,
		// getGlobal never creates collab-xproject.db — a daemon whose sessions
		// have never exchanged a cross-project message never materialises it,
		// including from the dashboard reading this.
		CollabGlobalStore: collabPool.getGlobal,
		// healthFor reads the LIVE indexer's health for a workspace this daemon
		// holds a store for (never opening one, so a dashboard poll cannot
		// materialise an index); ok=false leaves the DTO's failing flag false —
		// "not known", not "fine".
		TopologyHealth: topoPool.healthFor,
	}
}
