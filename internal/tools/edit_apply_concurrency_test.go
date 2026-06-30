package tools

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// TestApplyTextEditsToFile_ConcurrentNoLostUpdate runs many concurrent
// symbol-edit applications against the same file. Each prepends one "X"; under
// the per-path write lock the read-modify-writes serialise, so all K inserts
// survive. Without the lock they lost-update each other (fewer than K), and the
// old fixed "<path>.tmp" name would also collide. Regression for toolslsp-1/2.
func TestApplyTextEditsToFile_ConcurrentNoLostUpdate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("base"), 0o644); err != nil {
		t.Fatal(err)
	}

	const k = 8
	at0 := protocol.Range{
		Start: protocol.Position{Line: 0, Character: 0},
		End:   protocol.Position{Line: 0, Character: 0},
	}
	var wg sync.WaitGroup
	errs := make([]error, k)
	for i := 0; i < k; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = applyTextEditsToFile(path, []protocol.TextEdit{{Range: at0, NewText: "X"}})
		}(i)
	}
	wg.Wait()

	for i, e := range errs {
		if e != nil {
			t.Fatalf("apply %d failed: %v", i, e)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(data), "X"); got != k {
		t.Errorf("lost update: %d 'X' prefixes, want %d (content=%q)", got, k, data)
	}
	if !strings.HasSuffix(string(data), "base") {
		t.Errorf("original content lost: %q", data)
	}
	// The old code wrote to a fixed "<path>.tmp"; safeWrite uses a unique temp, so
	// no deterministic sidecar must remain.
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("a fixed <path>.tmp sidecar was left behind: stat err = %v", err)
	}
}
