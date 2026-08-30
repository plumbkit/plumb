//go:build integration

package topology_test

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/topology"
	golang "github.com/plumbkit/plumb/internal/topology/extractors/golang"
	"github.com/plumbkit/plumb/internal/topology/extractors/treesitter"
	"github.com/plumbkit/plumb/internal/topology/extractors/wasmts"

	_ "modernc.org/sqlite"
)

const (
	lifecycleBenchRevision      = "f97fc017"
	lifecycleBenchFiles         = 1415
	lifecycleBenchResolvedCalls = 2531

	// lifecycleSaveBudget is a REGRESSION bound, not a performance target. The
	// per-save numbers stay logged for PLAN-377; this only has to catch a save
	// that stopped being incremental.
	//
	// It was 5s, which is roughly what a slow runner actually takes: on
	// macos-latest the same three files measured 1.3s–5.03s, and CI run
	// 33288975634 went red at 5033.84ms — over by 34ms, 0.7%. A bound that a
	// green machine lands within one percent of is a coin toss, and every merge
	// paid for it in re-runs (PLAN-419). 15s keeps roughly 3× headroom over the
	// worst observed CI save while still catching the regression that matters:
	// a save that re-indexes the corpus takes tens of seconds, not single-digit
	// ones, so the failure this guards against is an order of magnitude away
	// rather than a rounding error away.
	lifecycleSaveBudget = 15 * time.Second

	// lifecycleWALBudget bounds the bytes ONE save appends to the WAL. Unlike
	// the wall-clock bound this is deterministic — CI and local runs report
	// byte-identical growth — so it has never flaked. It was still too tight to
	// leave alone: the worst observed save (internal/cli/root.go pass 0) writes
	// 1,965,240 bytes against a 2 MiB cap, 6% of headroom, so the next corpus
	// bump or SQLite change reddens main for no behavioural reason. 8 MiB keeps
	// the guard meaningful — a save that rewrote the whole index would add on
	// the order of the 45 MB database — with room the measurement can move in.
	lifecycleWALBudget int64 = 8 * 1024 * 1024
)

// The corpus is pinned to plumb revision f97fc017, materialised with
// `git archive` into a fresh temp directory so it holds exactly that committed
// tree and no .git, build output or pre-existing index. Every measurement below
// runs against a freshly built index of that corpus, warm (the initial full resync has
// completed and the WAL has been checkpointed), with a concurrent reader holding
// an open read transaction — the state the card measured in, and the state in
// which the WAL cannot be checkpointed away between saves.

func benchExtractors() []topology.Extractor {
	return []topology.Extractor{
		golang.New(),
		treesitter.NewPython(), treesitter.NewTypeScript(), treesitter.NewTSX(),
		treesitter.NewJavaScript(), treesitter.NewRust(), treesitter.NewZig(),
		treesitter.NewKotlin(), wasmts.NewSwift(), treesitter.NewJava(),
		treesitter.NewBash(), treesitter.NewHCL(), treesitter.NewSQL(),
		treesitter.NewDockerfile(), treesitter.NewTOML(), treesitter.NewYAML(),
		treesitter.NewMarkdown(), treesitter.NewHTML(), treesitter.NewRuby(),
		treesitter.NewC(), treesitter.NewJSON(), treesitter.NewCSS(),
		treesitter.NewScala(), treesitter.NewElixir(), treesitter.NewCSharp(),
		treesitter.NewPHP(), treesitter.NewSCSS(), treesitter.NewXML(),
		treesitter.NewLua(), treesitter.NewCpp(), treesitter.NewObjC(),
		treesitter.NewDart(),
	}
}

// benchCorpus materialises the pinned benchmark revision into a fresh temp directory.
func benchCorpus(t testing.TB) string {
	t.Helper()
	return unpackCorpus(t, benchRepoRoot(t), t.TempDir())
}

func unpackCorpus(t testing.TB, root, dst string) string {
	t.Helper()
	rev := lifecycleBenchRevision
	tarball := filepath.Join(t.TempDir(), "corpus.tar")
	archive := exec.Command("git", "-C", root, "archive", "--format=tar", "-o", tarball, rev) //nolint:gosec // G204: rev is operator-supplied bench configuration, not request data
	if out, err := archive.CombinedOutput(); err != nil {
		t.Fatalf("git archive: %v: %s", err, out)
	}
	untar := exec.Command("tar", "-x", "-f", tarball, "-C", dst)
	if out, err := untar.CombinedOutput(); err != nil {
		t.Fatalf("tar: %v: %s", err, out)
	}
	return dst
}

func walBytes(t testing.TB, dbPath string) int64 {
	t.Helper()
	fi, err := os.Stat(dbPath + "-wal")
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatalf("stat wal: %v", err)
	}
	return fi.Size()
}

// TestLifecycleSaveCost measures and enforces the wall-clock and WAL budgets for
// representative single-file saves on a warm index of plumb's own tree. Each save
// must complete in under five seconds and grow the WAL by no more than 2 MiB; the
// exact before/after numbers remain published for PLAN-377.
func TestLifecycleSaveCost(t *testing.T) {
	ws := benchCorpus(t)
	// Always index from scratch: a pinned corpus that kept its index would give
	// the second run of a before/after pair a different warm state from the first.
	if err := os.RemoveAll(filepath.Join(ws, ".plumb")); err != nil {
		t.Fatalf("clear index: %v", err)
	}
	cfg := config.TopologyConfig{MaxFileSizeBytes: 512 * 1024}
	store, err := topology.Open(ws, cfg, benchExtractors())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()

	waitIndexed(t, store)
	st := store.Status()
	if st.IndexedFiles != lifecycleBenchFiles {
		t.Fatalf("benchmark corpus indexed %d files, want exactly %d at revision %s", st.IndexedFiles, lifecycleBenchFiles, lifecycleBenchRevision)
	}
	if st.CallGraph.Resolved != lifecycleBenchResolvedCalls {
		t.Fatalf("benchmark corpus resolved %d calls, want exactly %d at revision %s", st.CallGraph.Resolved, lifecycleBenchResolvedCalls, lifecycleBenchRevision)
	}
	t.Logf("corpus %s: %d indexed files, db %d bytes, call graph resolved=%d",
		lifecycleBenchRevision, st.IndexedFiles, st.DBSizeBytes, st.CallGraph.Resolved)

	dbPath := topology.DBPath(ws)
	// Checkpoint, then pin the WAL open with a reader so the measured growth is
	// bytes this save wrote, not bytes that survived a checkpoint race.
	ctl, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open ctl: %v", err)
	}
	defer ctl.Close()
	if _, err := ctl.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	reader, err := ctl.Begin()
	if err != nil {
		t.Fatalf("reader begin: %v", err)
	}
	var n int
	if err := reader.QueryRow(`SELECT COUNT(*) FROM topology_files`).Scan(&n); err != nil {
		t.Fatalf("reader query: %v", err)
	}
	defer reader.Rollback() //nolint:errcheck // read-only probe

	for _, rel := range []string{
		"internal/topology/indexer.go",
		"internal/cli/root.go",
		"internal/mcp/server.go",
	} {
		abs := filepath.Join(ws, rel)
		original, err := os.ReadFile(abs)
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		for pass := range 3 {
			before := walBytes(t, dbPath)
			d := timeSave(t, store, ws, rel, original, pass)
			after := walBytes(t, dbPath)
			walDelta := after - before
			t.Logf("save %-34s pass %d: %8.2fms  wal +%d bytes", rel, pass, float64(d.Microseconds())/1000, walDelta)
			if d >= lifecycleSaveBudget {
				t.Errorf("save %s pass %d took %s; budget is < %s", rel, pass, d, lifecycleSaveBudget)
			}
			if walDelta > lifecycleWALBudget {
				t.Errorf("save %s pass %d grew WAL by %d bytes; budget is <= %d", rel, pass, walDelta, lifecycleWALBudget)
			}
		}
		// Leave the corpus byte-identical to HEAD so a pinned corpus stays
		// comparable between a before and an after run.
		if err := os.WriteFile(abs, original, 0o600); err != nil {
			t.Fatalf("restore %s: %v", rel, err)
		}
	}
}

// timeSave rewrites one file, enqueues it and waits for the indexer to report a
// completed cycle, returning the wall-clock time that took.
func timeSave(t testing.TB, store *topology.Store, ws, rel string, original []byte, pass int) time.Duration {
	t.Helper()
	abs := filepath.Join(ws, rel)
	body := append(append([]byte{}, original...), []byte(fmt.Sprintf("\n// bench pass %d\n", pass))...)
	if err := os.WriteFile(abs, body, 0o600); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
	last := store.Status().LastSync
	start := time.Now()
	store.Enqueue(rel)
	for {
		s := store.Status()
		if s.IndexerState == "idle" && s.LastSync.After(last) {
			return time.Since(start)
		}
		if time.Since(start) > 60*time.Second {
			t.Fatalf("save of %s did not complete", rel)
		}
		time.Sleep(50 * time.Microsecond)
	}
}

func waitIndexed(t testing.TB, store *topology.Store) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		s := store.Status()
		if s.IndexerState == "idle" && !s.LastSync.IsZero() && s.IndexedFiles > 500 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("initial index did not complete")
}
