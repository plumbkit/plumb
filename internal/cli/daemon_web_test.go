package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/sqlitex"
	"github.com/plumbkit/plumb/internal/topology"
	"github.com/plumbkit/plumb/internal/web"
)

// TestDaemonWebDeps_TopologyHealthIsTheLivePool is PLAN-489's end-to-end pin for
// the dashboard's failing flag. It builds the web deps with daemonWebDeps — the
// SAME function runDaemon serves from — so deleting the TopologyHealth wiring
// there fails this test, where a test that assembled its own web.Deps would not.
//
// It drives a REAL index into its failing state (SQLite triggers that make every
// write to topology_files raise, the technique internal/tools uses) and asserts
// the accessor reports it, then reports recovery, and reports ok=false — "not
// known", never "fine" — for a workspace the pool does not hold.
//
// This is HALF of the indexer-to-JSON chain: it never renders the DTO. The other
// half, the overlay of that health onto the wire DTO, is pinned in internal/web's
// TestHandleTopology_FailingFlag. Delete either and the chain is no longer covered.
func TestDaemonWebDeps_TopologyHealthIsTheLivePool(t *testing.T) {
	ws := t.TempDir()
	writeFixture := func(name, src string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(ws, name), []byte(src), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	writeFixture("go.mod", "module example.com/demo\n\ngo 1.22\n")
	writeFixture("demo.go", "package demo\n\nfunc Alpha() {}\n")

	cfg := enabledTopologyConfig()
	pool := newTopologyPool(cfg)
	t.Cleanup(pool.StopAll)

	deps := daemonWebDeps(nil, newCollabPool(), pool, time.Time{})
	if deps.TopologyHealth == nil {
		t.Fatal("daemonWebDeps left TopologyHealth nil: the dashboard would fall back to a snapshot whose failing flag is always false")
	}

	// Not held: the pool has no store for this workspace, so the answer is
	// "not known" (ok=false), never a healthy-looking zero value.
	if h, ok := deps.TopologyHealth(ws); ok {
		t.Fatalf("TopologyHealth for an unheld workspace = (%+v, true), want ok=false", h)
	}

	store := pool.Acquire(ws, cfg)
	if store == nil {
		t.Fatal("expected a store from an enabled pool")
	}
	waitForDepsHealth(t, deps, ws, "synced", func(h topology.Health) bool {
		return h.State == "idle" && !h.LastSync.IsZero() && !h.Failing
	})

	heal := injectTopologyFilesFault(t, ws)
	writeFixture("demo.go", "package demo\n\nfunc Alpha() {}\n\nfunc Beta() {}\n")
	store.Enqueue("demo.go")
	waitForDepsHealth(t, deps, ws, "failing", func(h topology.Health) bool { return h.Failing })

	heal()
	store.Enqueue("demo.go")
	waitForDepsHealth(t, deps, ws, "recovered", func(h topology.Health) bool {
		return !h.Failing && h.State == "idle"
	})
}

// waitForDepsHealth polls the deps' TopologyHealth accessor — not the store
// directly — so every observation goes through the wiring under test.
func waitForDepsHealth(t *testing.T, deps web.Deps, ws, what string, cond func(topology.Health) bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var last topology.Health
	for time.Now().Before(deadline) {
		h, ok := deps.TopologyHealth(ws)
		if !ok {
			t.Fatalf("waiting for %s: TopologyHealth(%s) = ok false for a workspace the pool holds", what, ws)
		}
		if cond(h) {
			return
		}
		last = h
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for the index to be %s; last health = %+v", what, last)
}

// injectTopologyFilesFault makes every write to topology_files raise, so the
// next indexing cycle that touches a file fails. It returns the function that
// heals it. The same trigger SQL lives in two other places: internal/tools'
// injectPersistFault (topology_health_test.go) and internal/topology's
// indexer_health_test.go. A change to one copy should be a decision about all
// three, not a fix to one.
func injectTopologyFilesFault(t *testing.T, ws string) (heal func()) {
	t.Helper()
	db, err := sqlitex.Open(topology.DBPath(ws), sqlitex.Options{})
	if err != nil {
		t.Fatalf("open topology db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, stmt := range []string{
		`CREATE TRIGGER inject_files_insert BEFORE INSERT ON topology_files BEGIN SELECT RAISE(ABORT, 'disk I/O error (injected)'); END`,
		`CREATE TRIGGER inject_files_update BEFORE UPDATE ON topology_files BEGIN SELECT RAISE(ABORT, 'disk I/O error (injected)'); END`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("install fault trigger: %v", err)
		}
	}
	return func() {
		for _, stmt := range []string{`DROP TRIGGER inject_files_insert`, `DROP TRIGGER inject_files_update`} {
			if _, err := db.Exec(stmt); err != nil {
				t.Fatalf("drop fault trigger: %v", err)
			}
		}
	}
}
