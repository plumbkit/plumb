package topology_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/topology"
	golang "github.com/plumbkit/plumb/internal/topology/extractors/golang"
)

// TestResolver_EndToEnd_AChainedOrShadowedCallIsNotABareCall runs real source
// through the Go extractor, the index and the call resolver. It is the shape the
// review found on plumb's own tree: `newSessionStartTool(s).Execute()` linked to a
// package function named Execute, because the chained receiver was stored as a bare
// call. A local function value of the same name must not link either; a genuine
// bare call of Execute must.
func TestResolver_EndToEnd_AChainedOrShadowedCallIsNotABareCall(t *testing.T) {
	ws := t.TempDir()
	for name, src := range map[string]string{
		"p/a.go": "package p\n\nfunc Execute() {}\n",
		"p/b.go": "package p\n\ntype T struct{}\n\nfunc (T) Execute() {}\n\nfunc newT() T { return T{} }\n",
		"p/c.go": "package p\n\nfunc Caller() {\n\tnewT().Execute()\n\tExecute()\n}\n\n" +
			"func Shadow() {\n\tExecute := func() {}\n\tExecute()\n}\n",
	} {
		path := filepath.Join(ws, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	store, err := topology.Open(ws, config.TopologyConfig{MaxFileSizeBytes: 512 * 1024}, []topology.Extractor{golang.New()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	var c topology.CallGraphStatus
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		st := store.Status()
		if st.IndexerState == "idle" && st.IndexedFiles == 3 && st.CallGraph.CallSites > 0 {
			c = st.CallGraph
			break
		}
	}
	if c.CallSites != 4 {
		t.Fatalf("call sites = %d, want 4 (newT, .Execute, Execute in Caller; Execute in Shadow): %+v", c.CallSites, c)
	}
	// Caller's newT() and Execute() are genuine bare calls into sibling files.
	if c.Resolved != 2 || c.ResolvedSamePackage != 2 {
		t.Errorf("resolved = %d (same-package %d), want 2 and 2 — a third would be newT().Execute() read as Execute(): %+v",
			c.Resolved, c.ResolvedSamePackage, c)
	}
	if c.UnresolvedReceiver != 1 {
		t.Errorf("unresolved receiver = %d, want 1 (newT().Execute()): %+v", c.UnresolvedReceiver, c)
	}
	if c.BareUnmatched != 1 {
		t.Errorf("bare unmatched = %d, want 1 (Shadow's local Execute): %+v", c.BareUnmatched, c)
	}
}
