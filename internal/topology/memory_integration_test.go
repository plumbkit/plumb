package topology_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	tsg "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"

	"github.com/golimpio/plumb/internal/config"
	"github.com/golimpio/plumb/internal/topology"
	goext "github.com/golimpio/plumb/internal/topology/extractors/golang"
	ts "github.com/golimpio/plumb/internal/topology/extractors/treesitter"
)

// allExtractors mirrors cli.buildExtractors: every supported structural
// extractor, constructed up front (as on every workspace attach).
func allExtractors() []topology.Extractor {
	return []topology.Extractor{
		goext.New(),
		ts.NewPython(), ts.NewJavaScript(), ts.NewRust(), ts.NewZig(), ts.NewKotlin(),
		ts.NewSwift(), ts.NewJava(), ts.NewBash(), ts.NewHCL(), ts.NewSQL(),
		ts.NewDockerfile(), ts.NewTOML(), ts.NewYAML(), ts.NewMarkdown(), ts.NewHTML(),
	}
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// TestResyncMemoryDiscipline is an end-to-end guard for the two memory fixes,
// driven through the real topology indexer rather than a single extractor:
//
//  1. Lazy grammar loading — a workspace that contains only a few languages must
//     decode only those grammars, not all ~15 supported ones (the eager path
//     carried ~307 MB of unused grammar tables).
//  2. Parse-arena recycling — indexing many files of one language must not
//     allocate a fresh tree-sitter arena per file (the eager path churned
//     ~1.6 GB through nodeArena during a cold resync).
//
// The grammar cache and arena profile are process-global; the package runs no
// parallel tests and this test opens a single store (one indexer goroutine), so
// the counters reflect only this resync.
func TestResyncMemoryDiscipline(t *testing.T) {
	ws := t.TempDir()

	// A workspace of Go + Python + TOML + Markdown only. The other ~11
	// tree-sitter languages (Rust, Swift, Kotlin, Java, Zig, JS, HTML, SQL, …)
	// are deliberately absent.
	const pyFiles = 30
	for i := range pyFiles {
		writeFile(t, ws, fmt.Sprintf("mod_%d.py", i),
			"def f():\n    return g()\n\n\ndef g():\n    pass\n")
	}
	writeFile(t, ws, "main.go", "package main\n\nfunc main() {}\n")
	writeFile(t, ws, "config.toml", "[server]\nhost = \"localhost\"\nport = 8080\n")
	writeFile(t, ws, "README.md", "# Title\n\n## Section\n\nbody text\n")

	// Start from a clean global state, then watch what the resync actually does.
	grammars.PurgeEmbeddedLanguageCache()
	tsg.DrainArenaPools()
	tsg.EnableArenaProfile(true)
	defer tsg.EnableArenaProfile(false)
	tsg.ResetArenaProfile()

	cfg := config.TopologyConfig{MaxFileSizeBytes: 512 * 1024}
	store, err := topology.Open(ws, cfg, allExtractors())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// The indexer starts in "idle" and transitions to "running" once it picks up
	// the initial resync, so "idle" alone is racy at start. Wait until it is idle
	// AND at least one grammar has decoded (proof the resync actually parsed).
	deadline := time.Now().Add(60 * time.Second)
	var loaded int
	for time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		loaded, _ = grammars.EmbeddedLanguageCacheStats()
		if store.Status().IndexerState == "idle" && loaded > 0 {
			break
		}
	}
	if store.Status().IndexerState != "idle" || loaded == 0 {
		t.Fatalf("indexer did not finish parsing within timeout (state=%s, grammars=%d)",
			store.Status().IndexerState, loaded)
	}

	// (1) Lazy grammars: only the present tree-sitter languages decoded.
	if loaded >= 15 {
		t.Fatalf("all %d grammars decoded — lazy grammar loading is not working; a "+
			"Go/Python/TOML/Markdown workspace must not load Rust/Swift/Kotlin/… grammars", loaded)
	}
	// python + toml + markdown are present; allow a small margin but it must stay
	// far below the full supported set.
	if loaded > 5 {
		t.Fatalf("decoded %d grammars for a 3-tree-sitter-language workspace; expected ~3 — grammars are not loading lazily per language", loaded)
	}

	// (2) Arena recycling: 30 Python files parsed, but arenas are reused, so the
	// number of newly-allocated arenas stays a small constant, not ~per-file.
	p := tsg.ArenaProfileSnapshot()
	acquires := p.FullAcquire + p.IncrementalAcquire
	news := p.FullNew + p.IncrementalNew
	if acquires < pyFiles {
		t.Skipf("arena profile recorded only %d acquires for %d+ parses; profiled path may differ on this gotreesitter version", acquires, pyFiles)
	}
	if news >= uint64(pyFiles) {
		t.Fatalf("parse arenas not recycled: %d newly-allocated arenas across %d+ parses (want a small constant) — tree.Release() is not returning arenas to the pool", news, pyFiles)
	}
}
