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
// Covered here: workspace_symbols (query mode), get_definition (by name),
// read_symbol, call_hierarchy (by position — the site PLAN-490's reviewer picked
// first). Not covered, and why: get_definition's by-position path and
// call_hierarchy's by-name path surface the language server's error instead of
// falling back, so neither has a banner to label; each tool is exercised on the
// path that carries one.
func TestTopologyFallbacks_LabelAFailingIndex(t *testing.T) {
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

	// Positive control: nothing is wrong with the index, so the fallbacks answer
	// bare.
	for _, tc := range cases {
		check(t, tc, false, "healthy")
	}

	heal := injectPersistFault(t, ws)
	writeFixture("demo.go", "package demo\n\nfunc Alpha() { Beta() }\n\nfunc Beta() {}\n\nfunc Gamma() {}\n")
	store.Enqueue("demo.go")
	waitForHealth(t, store, "failing", func(h topology.Health) bool { return h.Failing })

	for _, tc := range cases {
		check(t, tc, true, "failing index")
	}

	heal()
	store.Enqueue("demo.go")
	waitForHealth(t, store, "recovered", func(h topology.Health) bool { return !h.Failing && h.State == "idle" })
	for _, tc := range cases {
		check(t, tc, false, "recovered")
	}
}
