package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/sqlitex"
	"github.com/plumbkit/plumb/internal/topology"
	goext "github.com/plumbkit/plumb/internal/topology/extractors/golang"
)

func TestIndexHealthNote(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

	if got := indexHealthNote(topology.Health{State: "idle", LastError: "old error"}, now); got != "" {
		t.Errorf("a healthy index with a historical error got a notice: %q", got)
	}

	never := indexHealthNote(topology.Health{State: "error", Failing: true, LastError: "boom"}, now)
	if !strings.HasPrefix(never, staleIndexMarker) || !strings.Contains(never, "no indexing cycle has succeeded") {
		t.Errorf("failing with no good sync: %q", never)
	}

	synced := indexHealthNote(topology.Health{
		State:     "running",
		Failing:   true,
		LastSync:  now.Add(-21 * time.Hour),
		LastError: "topology: link imports: clear: disk I/O error (8714)",
	}, now)
	for _, want := range []string{"2026-10-08T15:00:00Z", "21h0m0s ago", "disk I/O error (8714)", "not evidence of absence"} {
		if !strings.Contains(synced, want) {
			t.Errorf("notice lacks %q:\n%s", want, synced)
		}
	}

	multiline := indexHealthNote(topology.Health{Failing: true, LastError: "rebuild failed:\n  disk I/O error"}, now)
	if strings.Contains(multiline, "\n") || !strings.Contains(multiline, "rebuild failed: disk I/O error") {
		t.Errorf("a multi-line error was not collapsed onto the notice's line: %q", multiline)
	}

	long := indexHealthNote(topology.Health{Failing: true, LastError: strings.Repeat("é", 500)}, now)
	if !utf8.ValidString(long) {
		t.Error("a long multi-byte error was cut inside a rune")
	}
	if strings.Count(long, "é") > maxHealthErrorBytes/2 {
		t.Errorf("the quoted error was not bounded to %d bytes", maxHealthErrorBytes)
	}
}

func TestIndexFreshness(t *testing.T) {
	tests := []struct {
		h    topology.Health
		want string
	}{
		{topology.Health{State: "idle"}, "fresh"},
		{topology.Health{State: "running"}, "building"},
		{topology.Health{State: "error", Failing: true}, "stale"},
		{topology.Health{State: "stopped"}, "stale"},
		// A retry cycle after a failure is running, but it is still serving the
		// pre-failure snapshot: stale, not building.
		{topology.Health{State: "running", Failing: true}, "stale"},
	}
	for _, tt := range tests {
		if got := indexFreshness(tt.h); got != tt.want {
			t.Errorf("indexFreshness(%+v) = %q, want %q", tt.h, got, tt.want)
		}
	}
}

// injectPersistFault makes every write to topology_files raise, so the next
// indexing cycle that touches a file fails — standing in for the disk I/O error
// that held the live index in error (PLAN-467). It returns the function that
// removes the fault. The same trigger SQL lives in internal/topology's
// indexer_health_test.go and internal/cli's injectTopologyFilesFault
// (daemon_web_test.go): a change here should be a decision about all three.
func injectPersistFault(t *testing.T, ws string) (heal func()) {
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

func waitForHealth(t *testing.T, s *topology.Store, what string, cond func(topology.Health) bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond(s.Health()) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for the index to be %s; health = %+v", what, s.Health())
}

// TestTopologyTools_LabelAFailingIndex is the PLAN-467 regression for the tool
// surface: every topology-backed answer served while the indexer is failing
// leads with the stale-index notice — errors included, since "not found" from a
// failing index is the absence answer that most needs it — and the notice goes
// away once a cycle succeeds.
func TestTopologyTools_LabelAFailingIndex(t *testing.T) {
	ws := t.TempDir()
	writeFixture := func(name, src string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(ws, name), []byte(src), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	writeFixture("go.mod", "module example.com/demo\n\ngo 1.22\n")
	writeFixture("demo.go", "package demo\n\nfunc Alpha() { Beta() }\n\nfunc Beta() {}\n")
	writeFixture("demo_test.go", "package demo\n\nimport \"testing\"\n\nfunc TestAlpha(t *testing.T) { Alpha() }\n")

	store, err := topology.Open(ws, config.TopologyConfig{MaxFileSizeBytes: 512 * 1024}, []topology.Extractor{goext.New()})
	if err != nil {
		t.Fatalf("topology.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	waitForHealth(t, store, "synced", func(h topology.Health) bool {
		return h.State == "idle" && !h.LastSync.IsZero()
	})
	storeFn := func() *topology.Store { return store }
	wsFn := func(context.Context) string { return ws }

	type executor interface {
		Execute(context.Context, json.RawMessage) (string, error)
	}
	cases := []struct {
		name string
		tool executor
		args string
	}{
		{"topology_explore", NewTopologyExplore(storeFn), `{"name":"Alpha"}`},
		{"topology_explore not found", NewTopologyExplore(storeFn), `{"name":"NoSuchSymbol"}`},
		{"topology_affected", NewTopologyAffected(storeFn), `{"files":["demo.go"]}`},
		{"topology_impact", NewTopologyImpact(storeFn), `{"name":"Beta"}`},
		{"topology_impact not found", NewTopologyImpact(storeFn), `{"name":"NoSuchSymbol"}`},
		{"topology_impact package reachability", NewTopologyImpact(storeFn), `{"mode":"reachability"}`},
		{"topology_impact function reachability", NewTopologyImpact(storeFn), `{"mode":"reachability","granularity":"function"}`},
		{"topology_routes", NewTopologyRoutes(storeFn), `{}`},
		{"topology_search", NewTopologySearch(storeFn), `{"query":"Alpha"}`},
		{"structural_query", NewStructuralQuery(storeFn, wsFn), `{"query":"undocumented-exports"}`},
	}
	// answer returns what the caller reads: the result, or the error text.
	answer := func(t *testing.T, tool executor, args string) (text string, isErr bool) {
		t.Helper()
		out, err := tool.Execute(context.Background(), json.RawMessage(args))
		if err != nil {
			return err.Error(), true
		}
		return out, false
	}

	// Positive control: a healthy index carries no notice.
	for _, tc := range cases {
		if text, _ := answer(t, tc.tool, tc.args); strings.Contains(text, staleIndexMarker) {
			t.Errorf("%s: healthy index carries the stale notice:\n%s", tc.name, text)
		}
	}
	if got := topologyIndexStatus(store); got != "fresh" {
		t.Errorf("workspace_search freshness on a healthy index = %q, want fresh", got)
	}
	errSentinel := errors.New("sentinel")
	if err := withIndexHealthErr(store, errSentinel); !errors.Is(err, errSentinel) || err.Error() != errSentinel.Error() {
		t.Errorf("withIndexHealthErr on a healthy index = %v, want the error untouched", err)
	}

	heal := injectPersistFault(t, ws)
	writeFixture("demo.go", "package demo\n\nfunc Alpha() { Beta() }\n\nfunc Beta() {}\n\nfunc Gamma() {}\n")
	store.Enqueue("demo.go")
	waitForHealth(t, store, "failing", func(h topology.Health) bool { return h.Failing })

	for _, tc := range cases {
		text, isErr := answer(t, tc.tool, tc.args)
		if isErr && !strings.Contains(text, staleIndexMarker) {
			t.Errorf("%s: error from a failing index lacks the stale notice:\n%s", tc.name, text)
		}
		if !isErr && !strings.HasPrefix(text, staleIndexMarker) {
			t.Errorf("%s: answer from a failing index does not LEAD with the stale notice:\n%s", tc.name, text)
		}
	}
	if got := topologyIndexStatus(store); got != "stale" {
		t.Errorf("workspace_search freshness on a failing index = %q, want stale", got)
	}
	if err := withIndexHealthErr(store, errSentinel); !errors.Is(err, errSentinel) || !strings.Contains(err.Error(), staleIndexMarker) {
		t.Errorf("withIndexHealthErr on a failing index = %v: want the notice added and errors.Is preserved", err)
	}

	heal()
	store.Enqueue("demo.go")
	waitForHealth(t, store, "recovered", func(h topology.Health) bool { return !h.Failing && h.State == "idle" })
	for _, tc := range cases {
		if text, _ := answer(t, tc.tool, tc.args); strings.Contains(text, staleIndexMarker) {
			t.Errorf("%s: notice outlived recovery:\n%s", tc.name, text)
		}
	}
}
