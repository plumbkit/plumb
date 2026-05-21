//go:build integration

package topology_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/golimpio/plumb/internal/config"
	"github.com/golimpio/plumb/internal/lsp/adapters/gopls"
	"github.com/golimpio/plumb/internal/lsp/jsonrpc"
	"github.com/golimpio/plumb/internal/lsp/protocol"
	"github.com/golimpio/plumb/internal/topology"
	goext "github.com/golimpio/plumb/internal/topology/extractors/golang"
)

func benchRepoRoot(t testing.TB) string {
	t.Helper()
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find repo root")
		}
		dir = parent
	}
}

func requireGoplsBench(t testing.TB) string {
	t.Helper()
	p, err := exec.LookPath("gopls")
	if err != nil {
		t.Skip("gopls not found on PATH")
	}
	return p
}

// TestDoD6_TopologyFasterThanLSP asserts that topology search is ≥5× faster
// than gopls workspace/symbol on a cold start. Both are measured against the
// plumb repo itself.
//
// Run: go test -tags=integration -run TestDoD6 ./internal/topology/...
func TestDoD6_TopologyFasterThanLSP(t *testing.T) {
	goplsPath := requireGoplsBench(t)
	workspace := benchRepoRoot(t)

	lspDur, lspN := measureLSPCold(t, goplsPath, workspace)
	t.Logf("gopls cold start + workspace/symbol: %s (%d symbols)", lspDur, lspN)

	topoDur, topoN := measureTopologyCold(t, workspace)
	t.Logf("topology open + resync + search:      %s (%d results)", topoDur, topoN)

	ratio := float64(lspDur) / float64(topoDur)
	t.Logf("DoD-6 speedup ratio: %.1f× (requirement: ≥5×)", ratio)
	if ratio < 5.0 {
		t.Errorf("DoD-6 FAILED: topology must be ≥5× faster than LSP cold start; got %.1f×", ratio)
	}
}

func measureLSPCold(t *testing.T, goplsPath, workspace string) (time.Duration, int) {
	t.Helper()
	start := time.Now()

	cmd := exec.Command(goplsPath, "serve")
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal("start gopls:", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	conn := jsonrpc.NewConn(stdout, stdin)
	t.Cleanup(func() { _ = conn.Close() })
	ad := gopls.New(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	if _, err := ad.Initialize(ctx, gopls.DefaultInitParams(protocol.FileURI(workspace))); err != nil {
		t.Fatal("initialize:", err)
	}
	if err := ad.Initialized(ctx); err != nil {
		t.Fatal("initialized:", err)
	}
	syms, err := ad.WorkspaceSymbols(ctx, protocol.WorkspaceSymbolParams{Query: "workspacePool"})
	if err != nil {
		t.Fatal("workspace symbols:", err)
	}
	return time.Since(start), len(syms)
}

func measureTopologyCold(t *testing.T, workspace string) (time.Duration, int) {
	t.Helper()
	tmpWS := t.TempDir()
	if err := copyGoFilesTo(workspace, tmpWS); err != nil {
		t.Fatal("copy workspace:", err)
	}

	cfg := config.TopologyConfig{MaxFileSizeBytes: 512 * 1024}
	start := time.Now()
	store, err := topology.Open(tmpWS, cfg, []topology.Extractor{goext.New()})
	if err != nil {
		t.Fatal("open store:", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	deadline := time.Now().Add(180 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		if store.Status().IndexerState == "idle" {
			break
		}
	}
	if store.Status().IndexerState != "idle" {
		t.Fatal("topology indexer did not reach idle within 3 min")
	}

	results, err := store.Search(context.Background(), "workspacePool", topology.SearchOpts{Limit: 10})
	if err != nil {
		t.Fatal("search:", err)
	}
	return time.Since(start), len(results)
}

// TestDoD7_ConcurrentSearchNoBusy fires 100 concurrent Search() goroutines
// while the indexer is actively writing and asserts zero SQLITE_BUSY errors.
// Run with -race to verify no data races.
//
// Run: go test -tags=integration -race -run TestDoD7 ./internal/topology/...
func TestDoD7_ConcurrentSearchNoBusy(t *testing.T) {
	workspace := benchRepoRoot(t)
	tmpWS := t.TempDir()
	if err := copyGoFilesTo(workspace, tmpWS); err != nil {
		t.Fatal("copy workspace:", err)
	}

	cfg := config.TopologyConfig{MaxFileSizeBytes: 512 * 1024}
	store, err := topology.Open(tmpWS, cfg, []topology.Extractor{goext.New()})
	if err != nil {
		t.Fatal("open store:", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Fire 100 concurrent searches immediately — the indexer is still writing.
	ctx := context.Background()
	var wg sync.WaitGroup
	errCh := make(chan error, 100)
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, searchErr := store.Search(ctx, "workspacePool", topology.SearchOpts{Limit: 5}); searchErr != nil {
				select {
				case errCh <- fmt.Errorf("search: %w", searchErr):
				default:
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for e := range errCh {
		t.Errorf("DoD-7: %v", e)
	}
}

// BenchmarkTopologySearch measures topology search throughput on a warmed index.
//
// Run: go test -tags=integration -bench=BenchmarkTopologySearch ./internal/topology/...
func BenchmarkTopologySearch(b *testing.B) {
	workspace := benchRepoRoot(b)
	tmpWS := b.TempDir()
	if err := copyGoFilesTo(workspace, tmpWS); err != nil {
		b.Fatal("copy workspace:", err)
	}

	cfg := config.TopologyConfig{MaxFileSizeBytes: 512 * 1024}
	store, err := topology.Open(tmpWS, cfg, []topology.Extractor{goext.New()})
	if err != nil {
		b.Fatal("open store:", err)
	}
	b.Cleanup(func() { _ = store.Close() })

	deadline := time.Now().Add(180 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		if store.Status().IndexerState == "idle" {
			break
		}
	}
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = store.Search(ctx, "workspacePool", topology.SearchOpts{Limit: 10})
	}
}

// copyGoFilesTo copies all .go source files from src to dst, preserving the
// relative directory structure. Skips hidden dirs, vendor, node_modules, testdata, and .plumb.
func copyGoFilesTo(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if info.IsDir() {
			switch info.Name() {
			case "vendor", "node_modules", "testdata", "dist", "build", "__pycache__":
				return filepath.SkipDir
			}
			if len(info.Name()) > 1 && info.Name()[0] == '.' {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		rel, _ := filepath.Rel(src, path)
		dstPath := filepath.Join(dst, rel)
		if err := os.MkdirAll(filepath.Dir(dstPath), 0o700); err != nil {
			return err
		}
		data, err := os.ReadFile(path) //nolint:gosec
		if err != nil {
			return err
		}
		return os.WriteFile(dstPath, data, 0o644)
	})
}
