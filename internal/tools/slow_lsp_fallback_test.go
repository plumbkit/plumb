package tools_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/tools"
	"github.com/plumbkit/plumb/internal/topology"
	goext "github.com/plumbkit/plumb/internal/topology/extractors/golang"
)

// slow_lsp_fallback_test.go is the PLAN-403 guard set: the same defect PLAN-390
// fixed in read_symbol, in the tools it was deliberately deferred from.
//
// Every case here drives a language server that completes nothing — the limit
// case of "cold and still indexing", and the one that made the fallback look
// present while being dead. Two properties are asserted together, because
// either alone is satisfied by broken code:
//
//   - the tool ANSWERS FROM TREE-SITTER, and for the write tools the answer
//     reaches DISK. A fallback that is reached but cannot run (topology's
//     safeExtract refuses to start a parse on an expired context) is
//     byte-identical to a working one in a test that only checks for an error;
//   - it does so STRICTLY INSIDE the tool's own budget, so a caller whose
//     patience equals that budget still sees the fallback rather than a
//     transport timeout.
//
// Each is run twice: once from a bare parent context, and once from a caller
// that has already imposed the same deadline — the case withLSPDeadline passed
// straight through, handing the server every last nanosecond.

// slowFallbackBudget is small enough to keep the suite quick, and large enough
// that parsing a ten-line Go file is never the reason a case fails.
const slowFallbackBudget = 2 * time.Second

// slowLSP is a server that accepts a request and simply never answers.
func slowLSP() *mockLSP { return &mockLSP{block: true} }

func slowFallbackArgs(t *testing.T, m map[string]any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return raw
}

// openTopologyStore opens a Go-extractor topology store over ws. The store's
// ExtractFile re-parses on demand, so the symbol-edit fallbacks need no index
// wait; the outline/search fallbacks read the index and use newIndexedStore.
func openTopologyStore(t *testing.T, ws string) *topology.Store {
	t.Helper()
	s, err := topology.Open(ws, config.TopologyConfig{MaxFileSizeBytes: 512 * 1024},
		[]topology.Extractor{goext.New()})
	if err != nil {
		t.Fatalf("topology.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// alphaBetaSymbols is the document-symbol tree a HEALTHY server would return for
// fallbackFixture's demo.go, used for the warm-path direction.
func alphaBetaSymbols() []protocol.DocumentSymbol {
	return []protocol.DocumentSymbol{
		symbolAt("Alpha", 2, 4, 1),
		symbolAt("Beta", 6, 8, 1),
	}
}

// diskCheck is the on-disk assertion that proves a fallback actually RAN rather
// than merely being reached: plain data, so the case table needs no closure
// carrying a *testing.T around.
type diskCheck struct {
	path     string
	mustHave string
	mustLack string
}

// fallbackToolCase is one tool under one server, plus what its answer must have
// left on disk.
type fallbackToolCase struct {
	name string
	// disclosesTimeout says the tool's fallback banner must name the missed
	// attempt budget ("did not answer within …") rather than call a healthy but
	// slow server unavailable. False for file_outline alone: it labels the
	// answer source=topology and makes no claim about WHY the server did not
	// own it, so it has no "LSP unavailable" wording to correct (review §2).
	disclosesTimeout bool
	setup            func(t *testing.T, client *mockLSP) (exec func(context.Context) (string, error), checks []diskCheck)
}

// writeToolCases covers every tool PLAN-403 names whose fallback also WRITES.
// The write is the reason these were deferred from PLAN-390, so the verifier
// always reads the file back.
func writeToolCases() []fallbackToolCase {
	return []fallbackToolCase{
		{
			name: "insert_before_symbol",
			setup: func(t *testing.T, client *mockLSP) (func(context.Context) (string, error), []diskCheck) {
				t.Helper()
				store, fpath, uri := fallbackFixture(t)
				tool := tools.NewInsertBeforeSymbol(client, slowFallbackBudget).
					WithTopologyFallback(func() *topology.Store { return store })
				args := slowFallbackArgs(t, map[string]any{
					"uri": uri, "name_path": "Beta",
					"content": "// added above Beta\n", "dry_run": false,
				})
				return func(ctx context.Context) (string, error) { return tool.Execute(ctx, args) },
					[]diskCheck{{path: fpath, mustHave: "// added above Beta\nfunc Beta() int {"}}
			},
		},
		{
			name: "insert_after_symbol",
			setup: func(t *testing.T, client *mockLSP) (func(context.Context) (string, error), []diskCheck) {
				t.Helper()
				store, fpath, uri := fallbackFixture(t)
				tool := tools.NewInsertAfterSymbol(client, slowFallbackBudget).
					WithTopologyFallback(func() *topology.Store { return store })
				args := slowFallbackArgs(t, map[string]any{
					"uri": uri, "name_path": "Alpha",
					"content": "\n\nfunc Gamma() int { return 3 }", "dry_run": false,
				})
				return func(ctx context.Context) (string, error) { return tool.Execute(ctx, args) },
					[]diskCheck{{path: fpath, mustHave: "func Gamma() int { return 3 }"}}
			},
		},
		{
			name: "replace_symbol_body",
			setup: func(t *testing.T, client *mockLSP) (func(context.Context) (string, error), []diskCheck) {
				t.Helper()
				store, fpath, uri := fallbackFixture(t)
				tool := tools.NewReplaceSymbolBody(client, slowFallbackBudget).
					WithTopologyFallback(func() *topology.Store { return store })
				args := slowFallbackArgs(t, map[string]any{
					"uri": uri, "name_path": "Alpha",
					"content": "func Alpha() int {\n\treturn 99\n}", "dry_run": false,
				})
				return func(ctx context.Context) (string, error) { return tool.Execute(ctx, args) },
					[]diskCheck{{path: fpath, mustHave: "return 99", mustLack: "return 1\n"}}
			},
		},
		{
			name: "move_symbol",
			setup: func(t *testing.T, client *mockLSP) (func(context.Context) (string, error), []diskCheck) {
				t.Helper()
				dir := t.TempDir()
				srcPath, srcURI := writeInDir(t, dir, "src.go", moveSrc)
				dstPath, dstURI := writeInDir(t, dir, "dst.go", "package demo\n\nfunc Keep() {}\n")
				store := openTopologyStore(t, dir)
				tool := tools.NewMoveSymbol(client, slowFallbackBudget).
					WithTopologyFallback(func() *topology.Store { return store })
				args := slowFallbackArgs(t, map[string]any{
					"source_uri": srcURI, "name_path": "Foo",
					"destination_uri": dstURI, "dry_run": false,
				})
				return func(ctx context.Context) (string, error) { return tool.Execute(ctx, args) },
					[]diskCheck{
						{path: dstPath, mustHave: "func Foo() int { return 1 }"},
						{path: srcPath, mustLack: "func Foo() int { return 1 }"},
					}
			},
		},
	}
}

// readToolCases covers the two read tools PLAN-403 names for the no-headroom
// half: their fallbacks already ran on the live parent context, but the server
// attempt was allowed to consume the caller's entire budget first.
func readToolCases() []fallbackToolCase {
	return []fallbackToolCase{
		{
			name: "file_outline",
			setup: func(t *testing.T, client *mockLSP) (func(context.Context) (string, error), []diskCheck) {
				t.Helper()
				store, uri := newIndexedStore(t)
				tool := tools.NewFileOutline(client, nil, 0, slowFallbackBudget).
					WithTopologyFallback(func() *topology.Store { return store })
				args := slowFallbackArgs(t, map[string]any{"uri": uri})
				return func(ctx context.Context) (string, error) { return tool.Execute(ctx, args) }, nil
			},
		},
		{
			name:             "workspace_symbols",
			disclosesTimeout: true,
			setup: func(t *testing.T, client *mockLSP) (func(context.Context) (string, error), []diskCheck) {
				t.Helper()
				store, _ := newIndexedStore(t)
				tool := tools.NewWorkspaceSymbols(client, nil, 0, slowFallbackBudget, nil).
					WithTopologyFallback(func() *topology.Store { return store })
				// "Handle", not "HandleRequest": the response header echoes the
				// query back, so asking for the exact name would let the assertion
				// below pass on the echo alone, whatever the Map returned.
				args := slowFallbackArgs(t, map[string]any{"query": "Handle"})
				return func(ctx context.Context) (string, error) { return tool.Execute(ctx, args) }, nil
			},
		},
		{
			name:             "workspace_symbols/in_file",
			disclosesTimeout: true,
			setup: func(t *testing.T, client *mockLSP) (func(context.Context) (string, error), []diskCheck) {
				t.Helper()
				store, uri := newIndexedStore(t)
				tool := tools.NewWorkspaceSymbols(client, nil, 0, slowFallbackBudget, nil).
					WithTopologyFallback(func() *topology.Store { return store })
				args := slowFallbackArgs(t, map[string]any{"query": "Handle", "uri": uri})
				return func(ctx context.Context) (string, error) { return tool.Execute(ctx, args) }, nil
			},
		},
	}
}

// parentContexts are the two callers a tool must behave identically for. The
// second is the load-bearing one: withLSPDeadline handed an already-bounded
// context straight through, so the server spent the caller's whole budget and
// the fallback — however live its context — had no time left to answer in.
func parentContexts(t *testing.T) []struct {
	name string
	ctx  context.Context
} {
	t.Helper()
	bounded, cancel := context.WithTimeout(context.Background(), slowFallbackBudget)
	t.Cleanup(cancel)
	return []struct {
		name string
		ctx  context.Context
	}{
		{"unbounded caller", context.Background()},
		{"caller deadline equals the tool budget", bounded},
	}
}

// TestSymbolWriteTools_SlowLSPWritesViaFallback is the half that matters to an
// agent rather than only to CI: a symbol-edit whose server never answers must
// still land on disk, from tree-sitter, inside the tool's budget.
func TestSymbolWriteTools_SlowLSPWritesViaFallback(t *testing.T) {
	for _, tc := range writeToolCases() {
		for _, parent := range parentContexts(t) {
			t.Run(tc.name+"/"+parent.name, func(t *testing.T) {
				exec, checks := tc.setup(t, slowLSP())

				start := time.Now()
				out, err := exec(parent.ctx)
				elapsed := time.Since(start)

				if err != nil {
					t.Fatalf("a slow language server must degrade to the tree-sitter fallback; "+
						"got an error after %v: %v", elapsed, err)
				}
				if elapsed >= slowFallbackBudget {
					t.Errorf("answered after %v — at or past the whole %v budget. A caller whose "+
						"patience equals the budget never sees the fallback, which is the PLAN-390 "+
						"inversion; the server attempt must leave headroom for the parse.",
						elapsed, slowFallbackBudget)
				}
				if !strings.Contains(out, "topology fallback") {
					t.Errorf("response does not name the tree-sitter fallback:\n%s", out)
				}
				// The disclosure PLAN-403 owes the agent: the server was up and
				// merely slow, so "LSP unavailable" would be a lie and the range
				// is line-granular either way.
				if !strings.Contains(out, "did not answer within") {
					t.Errorf("a timed-out server must be reported as such, not as unavailable:\n%s", out)
				}
				// The assertion a reached-but-dead fallback cannot satisfy.
				for _, c := range checks {
					assertDisk(t, c)
				}
			})
		}
	}
}

// TestSymbolReadTools_SlowLSPAnswersInsideBudget is the no-headroom half for the
// two read tools: their fallbacks were already handed a live context, but only
// ever reached it after the server had spent the caller's entire budget.
func TestSymbolReadTools_SlowLSPAnswersInsideBudget(t *testing.T) {
	for _, tc := range readToolCases() {
		for _, parent := range parentContexts(t) {
			t.Run(tc.name+"/"+parent.name, func(t *testing.T) {
				exec, _ := tc.setup(t, slowLSP())

				start := time.Now()
				out, err := exec(parent.ctx)
				elapsed := time.Since(start)

				if err != nil {
					t.Fatalf("a slow language server must degrade to the Map; got an error after %v: %v", elapsed, err)
				}
				if elapsed >= slowFallbackBudget {
					t.Errorf("answered after %v — at or past the whole %v budget, so a caller whose "+
						"patience equals it sees a timeout instead of the Map answer",
						elapsed, slowFallbackBudget)
				}
				if !strings.Contains(out, "HandleRequest") {
					t.Errorf("expected the Map's answer naming HandleRequest:\n%s", out)
				}
				// The same disclosure the write tools owe: the server was up and
				// merely slower than its (now shortened) attempt budget, so
				// "LSP unavailable" argues the agent into the wrong conclusion.
				if tc.disclosesTimeout && !strings.Contains(out, "did not answer within") {
					t.Errorf("a timed-out server must be reported as such, not as unavailable:\n%s", out)
				}
				if tc.disclosesTimeout && strings.Contains(out, "LSP unavailable") {
					t.Errorf("a server that answered nothing inside its attempt budget is slow, not absent:\n%s", out)
				}
			})
		}
	}
}

// TestSymbolTools_WarmLSPUnchanged is the other direction, and the one an
// over-eager fix breaks: when the server answers promptly it must WIN. No
// fallback banner, no tree-sitter range, no added latency — the warm path is
// what every agent hits, and shortening the attempt budget must not cost it
// anything.
func TestSymbolTools_WarmLSPUnchanged(t *testing.T) {
	// Provenance, not just absence of a banner: the server is given a range no
	// tree-sitter parse of demo.go could produce (Alpha stops one line short of
	// its closing brace), so seeing it echoed proves the SERVER's answer was
	// used. A dry run, because the range is only printed in the preview.
	t.Run("symbol edits/server range wins", func(t *testing.T) {
		store, _, uri := fallbackFixture(t)
		serverOnly := []protocol.DocumentSymbol{symbolAt("Alpha", 2, 3, 8), symbolAt("Beta", 6, 8, 1)}
		tool := tools.NewReplaceSymbolBody(&mockLSP{docSymbols: serverOnly}, slowFallbackBudget).
			WithTopologyFallback(func() *topology.Store { return store })
		out, err := tool.Execute(context.Background(), slowFallbackArgs(t, map[string]any{
			"uri": uri, "name_path": "Alpha", "content": "x",
		}))
		if err != nil {
			t.Fatalf("warm server: %v", err)
		}
		if strings.Contains(out, "topology fallback") {
			t.Errorf("an answering server must resolve the symbol itself, with no fallback banner:\n%s", out)
		}
		if !strings.Contains(out, "Range: line 2 char 0 → line 3 char 8") {
			t.Errorf("expected the server's own range, not a fresh tree-sitter one:\n%s", out)
		}
	})

	t.Run("symbol edits/apply", func(t *testing.T) {
		store, fpath, uri := fallbackFixture(t)
		tool := tools.NewReplaceSymbolBody(&mockLSP{docSymbols: alphaBetaSymbols()}, slowFallbackBudget).
			WithTopologyFallback(func() *topology.Store { return store })
		args := slowFallbackArgs(t, map[string]any{
			"uri": uri, "name_path": "Alpha",
			"content": "func Alpha() int {\n\treturn 99\n}", "dry_run": false,
		})

		start := time.Now()
		out, err := tool.Execute(context.Background(), args)
		elapsed := time.Since(start)

		if err != nil {
			t.Fatalf("warm server: %v", err)
		}
		if strings.Contains(out, "topology fallback") {
			t.Errorf("an answering server must resolve the symbol itself, with no fallback banner:\n%s", out)
		}
		if elapsed > slowFallbackBudget/4 {
			t.Errorf("warm path took %v; bounding the attempt must add no latency when the "+
				"server answers", elapsed)
		}
		requireFileHas(t, fpath, "return 99")
	})

	t.Run("file_outline", func(t *testing.T) {
		store, uri := newIndexedStore(t)
		tool := tools.NewFileOutline(&mockLSP{docSymbols: alphaBetaSymbols()}, nil, 0, slowFallbackBudget).
			WithTopologyFallback(func() *topology.Store { return store })
		out := runOutline(t, tool, uri, nil)
		if !strings.Contains(out, "source=lsp") {
			t.Errorf("an answering server must own the outline; got:\n%s", out)
		}
	})

	t.Run("workspace_symbols", func(t *testing.T) {
		store, _ := newIndexedStore(t)
		client := &mockLSP{wsSymbols: []protocol.SymbolInformation{{
			Name: "HandleRequest", Kind: protocol.SKFunction,
			Location: protocol.Location{URI: "file:///demo.go"},
		}}}
		tool := tools.NewWorkspaceSymbols(client, nil, 0, slowFallbackBudget, nil).
			WithTopologyFallback(func() *topology.Store { return store })
		out, err := tool.Execute(context.Background(), slowFallbackArgs(t, map[string]any{"query": "HandleRequest"}))
		if err != nil {
			t.Fatalf("warm server: %v", err)
		}
		if strings.Contains(out, "topology fallback") {
			t.Errorf("an answering server must own the result, with no fallback banner:\n%s", out)
		}
	})
}

// TestSymbolWriteTools_NoTopologyStillBounded is the honesty case for the
// deferred-risk half: with no Map to fall back to there is nothing to degrade
// to, so the tool must still give up inside its budget rather than hang, and
// must not have written anything.
func TestSymbolWriteTools_NoTopologyStillBounded(t *testing.T) {
	_, fpath, uri := fallbackFixture(t)
	before, err := os.ReadFile(fpath)
	if err != nil {
		t.Fatal(err)
	}
	tool := tools.NewReplaceSymbolBody(slowLSP(), slowFallbackBudget)
	args := slowFallbackArgs(t, map[string]any{
		"uri": uri, "name_path": "Alpha",
		"content": "func Alpha() int { return 0 }", "dry_run": false,
	})

	start := time.Now()
	_, execErr := tool.Execute(context.Background(), args)
	elapsed := time.Since(start)

	if execErr == nil {
		t.Fatal("expected a timeout error with no topology store wired")
	}
	if elapsed >= slowFallbackBudget {
		t.Errorf("gave up after %v, at or past the whole %v budget", elapsed, slowFallbackBudget)
	}
	after, err := os.ReadFile(fpath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("a failed resolve must write nothing:\n%s", after)
	}
}

func assertDisk(t *testing.T, c diskCheck) {
	t.Helper()
	got, err := os.ReadFile(filepath.Clean(c.path))
	if err != nil {
		t.Fatalf("reading %s: %v", c.path, err)
	}
	if c.mustHave != "" && !strings.Contains(string(got), c.mustHave) {
		t.Errorf("the fallback did not reach disk — %s is missing %q:\n%s", c.path, c.mustHave, got)
	}
	if c.mustLack != "" && strings.Contains(string(got), c.mustLack) {
		t.Errorf("%s still contains %q:\n%s", c.path, c.mustLack, got)
	}
}

func requireFileHas(t *testing.T, path, want string) {
	t.Helper()
	assertDisk(t, diskCheck{path: path, mustHave: want})
}
