package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/topology"
	goext "github.com/plumbkit/plumb/internal/topology/extractors/golang"
	ts "github.com/plumbkit/plumb/internal/topology/extractors/treesitter"
)

// errNoLanguageServer is the hard error unavailableLSP answers every call with:
// exactly the failure each LSP-backed surface's topology fallback exists to
// replace.
var errNoLanguageServer = errors.New("language server unavailable")

// unavailableLSP is an lsp.Client that answers nothing, which puts every tool
// wired to it on its topology fallback path. The embedded nil interface supplies
// the rest of the interface at compile time and panics if one is called — the
// same shape as ctxRecordingClient (mutationtest_safety_test.go) and
// notifyHookClient (write_record_race_test.go), so a call the covered paths do
// not make fails loudly rather than quietly changing what is under test.
//
// Only the methods those paths reach are implemented:
//
//	WorkspaceSymbols     workspace_symbols, workspace-wide query mode
//	DocumentSymbols      read_symbol, and get_definition's by-name resolution
//	PrepareCallHierarchy call_hierarchy, queried by position
//	References           call_hierarchy's LSP callers, inside its own fallback
type unavailableLSP struct {
	lsp.Client
}

func (c *unavailableLSP) WorkspaceSymbols(context.Context, protocol.WorkspaceSymbolParams) ([]protocol.SymbolInformation, error) {
	return nil, errNoLanguageServer
}

func (c *unavailableLSP) DocumentSymbols(context.Context, protocol.DocumentSymbolParams) ([]protocol.DocumentSymbol, error) {
	return nil, errNoLanguageServer
}

func (c *unavailableLSP) PrepareCallHierarchy(context.Context, protocol.PrepareCallHierarchyParams) ([]protocol.CallHierarchyItem, error) {
	return nil, errNoLanguageServer
}

func (c *unavailableLSP) References(context.Context, protocol.ReferenceParams) ([]protocol.Location, error) {
	return nil, errNoLanguageServer
}

// emptyLSP answers a workspace query successfully with nothing — the lazy server
// (zls and the other on-demand indexers) that has not analysed the matching
// files yet. It is the one shape that puts workspace_symbols on its tree-sitter
// FILL path rather than its fallback: an error falls back, and an answer
// carrying hits wins outright, so only empty-and-no-error reaches
// formatTopologyFill. Same embedding contract as unavailableLSP.
type emptyLSP struct {
	lsp.Client
}

func (c *emptyLSP) WorkspaceSymbols(context.Context, protocol.WorkspaceSymbolParams) ([]protocol.SymbolInformation, error) {
	return nil, nil
}

// fallbackBannerLine returns the first line of out carrying marker, the text
// that announces a topology fallback.
func fallbackBannerLine(out, marker string) (string, bool) {
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, marker) {
			return line, true
		}
	}
	return "", false
}

// TestTopologyFallbacks_LabelAFailingIndex is the PLAN-497 regression for the
// LSP tools' topology fallbacks: while the index's most recent cycle has failed,
// the banner each one leads with must carry the stale-index clause (PLAN-490) —
// these are the answers an agent is most likely to act on, since a fallback that
// says "no callers" from a failing index is an absence claim the index cannot
// support. When a cycle succeeds again the clause must go, or it stops meaning
// anything.
//
// The language server is stubbed out entirely (unavailableLSP), so every surface
// takes its fallback in all three phases and the only thing that varies is the
// index's health. That is deliberate: it isolates the labelling rule from the
// server's behaviour, which is the variable the other tests cover.
//
// Covered here: workspace_symbols (query mode, and the uri-scoped IN-FILE
// banner), workspace_symbols' tree-sitter FILL banner, get_definition (by name),
// read_symbol, call_hierarchy (by position — the site PLAN-490's reviewer picked
// first), file_outline's index fallback, and minimal_diff_review (whose clause
// goes in the report header and whose caller-count finding is withheld while the
// index is failing). Not covered, and why: get_definition's by-position path and
// call_hierarchy's by-name path surface the language server's error instead of
// falling back, so neither has a banner to label; each tool is exercised on the
// path that carries one.
func TestTopologyFallbacks_LabelAFailingIndex(t *testing.T) {
	// minDiffReview needs a git working tree, and a bare t.TempDir() is only
	// outside one by accident (CI puts TMPDIR inside this checkout), so the
	// fixture IS the repo helper the review tests already use. One workspace,
	// one store, one three-phase shape for every surface.
	ws, _ := setupReviewRepo(t)
	writeFixture := func(name, src string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(ws, name), []byte(src), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	writeFixture("go.mod", "module example.com/demo\n\ngo 1.22\n")
	writeFixture("demo.go", "package demo\n\nfunc Alpha() { Beta() }\n\nfunc Beta() {}\n")
	writeFixture("demo_test.go", "package demo\n\nimport \"testing\"\n\nfunc TestAlpha(t *testing.T) { Alpha() }\n")
	// A tree-sitter-backed language for the FILL case (Go via gopls is excluded
	// from the fill by design), and the caller + single-use helper shape the
	// review's caller-count finding is built on.
	writeFixture("demo.py", "def handle_request():\n    pass\n")
	writeFixture("review.go", "package demo\n\nfunc CallHelper() int {\n\tx := helperOnce()\n\treturn x + 1\n}\n\nfunc helperOnce() int {\n\treturn 42\n}\n")

	store, err := topology.Open(ws, config.TopologyConfig{MaxFileSizeBytes: 512 * 1024},
		[]topology.Extractor{goext.New(), ts.NewPython()})
	if err != nil {
		t.Fatalf("topology.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	waitForHealth(t, store, "synced", func(h topology.Health) bool {
		return h.State == "idle" && !h.LastSync.IsZero()
	})
	storeFn := func() *topology.Store { return store }
	wsFn := func(context.Context) string { return ws }

	const (
		ttl     = time.Minute
		timeout = 5 * time.Second
	)
	demoPath := filepath.Join(ws, "demo.go")
	demoURI := toFileURI(demoPath)
	client := &unavailableLSP{}

	type executor interface {
		Execute(context.Context, json.RawMessage) (string, error)
	}
	type fallbackCase struct {
		name string
		tool executor
		args string
		// banner is a substring of the line the tool announces its fallback on.
		// Every phase asserts it is present: a surface that quietly stopped
		// falling back would otherwise pass for the wrong reason, since a missing
		// banner trivially carries no clause.
		banner string
	}
	cases := []fallbackCase{
		{
			name:   "workspace_symbols",
			tool:   NewWorkspaceSymbols(client, nil, ttl, timeout, wsFn).WithTopologyFallback(storeFn),
			args:   `{"query":"Alpha"}`,
			banner: "[topology fallback",
		},
		{
			// The uri-scoped IN-FILE banner: topologyFallbackInFile's own note,
			// a separate call site from the workspace-wide one above.
			name:   "workspace_symbols (in-file)",
			tool:   NewWorkspaceSymbols(client, nil, ttl, timeout, wsFn).WithTopologyFallback(storeFn),
			args:   fmt.Sprintf(`{"query":"Alpha","uri":%q}`, demoURI),
			banner: "[topology fallback",
		},
		{
			// The FILL banner: the server answered empty and the index supplies
			// the answer, so this is not a fallback at all — and a failing index
			// makes its hits exactly as untrustworthy.
			name:   "workspace_symbols (tree-sitter fill)",
			tool:   NewWorkspaceSymbols(&emptyLSP{}, nil, ttl, timeout, wsFn).WithTopologyFallback(storeFn),
			args:   `{"query":"handle_request"}`,
			banner: "[topology fill",
		},
		{
			name:   "get_definition",
			tool:   NewGetDefinition(client, nil, ttl, timeout).WithWorkspace(wsFn).WithTopologyFallback(storeFn),
			args:   fmt.Sprintf(`{"uri":%q,"symbol_name":"Alpha"}`, demoURI),
			banner: "[topology fallback",
		},
		{
			name:   "read_symbol",
			tool:   NewReadSymbol(client, nil, ttl, timeout, nil).WithWorkspace(wsFn).WithTopologyFallback(storeFn),
			args:   fmt.Sprintf(`{"path":%q,"name":"Alpha"}`, demoPath),
			banner: "[topology fallback",
		},
		{
			name:   "call_hierarchy",
			tool:   NewCallHierarchy(client, timeout).WithWorkspace(wsFn).WithTopologyFallback(storeFn),
			args:   fmt.Sprintf(`{"uri":%q,"line":2,"character":5,"direction":"both"}`, demoPath),
			banner: "(reconstructed",
		},
		{
			// file_outline's header IS its banner: "source=topology" is how the
			// answer says which index it came from, and the clause rides on it.
			name:   "file_outline",
			tool:   NewFileOutline(client, nil, ttl, timeout).WithWorkspace(wsFn).WithTopologyFallback(storeFn),
			args:   fmt.Sprintf(`{"uri":%q}`, demoURI),
			banner: "source=topology",
		},
	}

	// check runs one surface in one phase. The clause is asserted ON the banner
	// line, not merely somewhere in the answer: a clause parked elsewhere would
	// not tell the reader what the banner is telling them.
	check := func(t *testing.T, tc fallbackCase, wantClause bool, phase string) {
		t.Helper()
		out, err := tc.tool.Execute(context.Background(), json.RawMessage(tc.args))
		if err != nil {
			t.Fatalf("%s (%s): the topology fallback must answer, not error: %v", tc.name, phase, err)
		}
		line, ok := fallbackBannerLine(out, tc.banner)
		if !ok {
			t.Fatalf("%s (%s): the answer carries no fallback banner (%q):\n%s", tc.name, phase, tc.banner, out)
		}
		if !wantClause {
			if strings.Contains(out, staleIndexMarker) {
				t.Errorf("%s (%s): a healthy index still carries the stale clause:\n%s", tc.name, phase, out)
			}
			return
		}
		if !strings.Contains(line, staleIndexMarker) {
			t.Errorf("%s (%s): the fallback banner lacks the stale clause:\n%s", tc.name, phase, line)
		}
	}

	// minimal_diff_review carries two PLAN-490 behaviours that must flip
	// TOGETHER, so it is checked on two axes rather than through the banner table:
	// the clause belongs in the report HEADER (formatReview — there is no fallback
	// banner line in a review), and callerCountAt withholds its counts while the
	// index is failing. A clause with the count still present would be a caveat
	// nobody needs; a withheld count with no clause would be an unexplained
	// silence. A single-use finding is the only surface the caller count reaches,
	// so its presence is the count's presence.
	review := NewMinimalDiffReview(storeFn).WithWorkspace(wsFn)
	checkReview := func(phase string, wantClause, wantCount bool) {
		t.Helper()
		out, err := review.Execute(context.Background(), json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("minimal_diff_review (%s): the review must answer, not error: %v", phase, err)
		}
		header, _, ok := strings.Cut(out, "\nfindings")
		if !ok {
			t.Fatalf("minimal_diff_review (%s): the report carries no findings section:\n%s", phase, out)
		}
		if wantClause {
			if !strings.Contains(header, staleIndexMarker) {
				t.Errorf("minimal_diff_review (%s): the report header lacks the stale clause:\n%s", phase, out)
			}
		} else if strings.Contains(out, staleIndexMarker) {
			t.Errorf("minimal_diff_review (%s): a healthy index still carries the stale clause:\n%s", phase, out)
		}
		// The limits section always names single-use-abstraction, so the count's
		// presence is the FINDING line ("[INFO] single-use-abstraction …"), not
		// the bare kind name.
		counted := strings.Contains(out, "] single-use-abstraction")
		if wantCount && (!counted || !strings.Contains(out, "helperOnce")) {
			t.Errorf("minimal_diff_review (%s): the caller-count finding for helperOnce is missing:\n%s", phase, out)
		}
		if !wantCount && counted {
			t.Errorf("minimal_diff_review (%s): a caller count from a failing index must be withheld:\n%s", phase, out)
		}
	}

	// Positive control: nothing is wrong with the index, so the fallbacks answer
	// bare.
	for _, tc := range cases {
		check(t, tc, false, "healthy")
	}
	checkReview("healthy", false, true)

	heal := injectPersistFault(t, ws)
	writeFixture("demo.go", "package demo\n\nfunc Alpha() { Beta() }\n\nfunc Beta() {}\n\nfunc Gamma() {}\n")
	store.Enqueue("demo.go")
	waitForHealth(t, store, "failing", func(h topology.Health) bool { return h.Failing })

	for _, tc := range cases {
		check(t, tc, true, "failing index")
	}
	checkReview("failing index", true, false)

	heal()
	store.Enqueue("demo.go")
	waitForHealth(t, store, "recovered", func(h topology.Health) bool { return !h.Failing && h.State == "idle" })
	for _, tc := range cases {
		check(t, tc, false, "recovered")
	}
	checkReview("recovered", false, true)
}
