package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/tools"
	"github.com/plumbkit/plumb/internal/topology"
)

// symbol_fallback_healthy_test.go guards the rule behind issue #571: the
// tree-sitter fallback is for a language server that DID NOT ANSWER. A server
// that answered — even with "that symbol is not in this file" — is not
// unavailable, and re-asking another tree behind its back both picks a symbol
// the server never named and blames the server in the banner.
//
// Every tool that resolves through resolveSymbolOrFallback is driven through the
// same four situations, in both directions, so a guard that refuses everything
// (the fallback never runs) fails alongside one that lets everything through
// (it always runs):
//
//	healthy server, symbol present        resolves, no banner
//	healthy server, symbol absent         refused, nothing written, no fallback
//	unavailable server                    fallback lands the edit, says so
//	cold server answering an empty tree   fallback lands the edit, says "warming"

// coldProbe is a warm-up probe reporting a fixed state.
func coldProbe(warming bool) tools.LSPWarmupFn {
	return func(string) (bool, time.Duration) { return warming, 4 * time.Second }
}

// executor is the part of a tool these tests drive.
type executor interface {
	Execute(context.Context, json.RawMessage) (string, error)
}

// resolverRun is what one tool did on one fixture.
type resolverRun struct {
	out   string
	err   error
	wrote func() bool // whether any file the tool was pointed at changed
}

// resolverCaller is a tool that resolves its target through
// resolveSymbolOrFallback, over a fixture whose Go source holds the symbol
// target. holding is the document-symbol tree a server that knows the file
// returns; lacking is one from a server that answered without the symbol.
type resolverCaller struct {
	name    string
	target  string
	holding []protocol.DocumentSymbol
	lacking []protocol.DocumentSymbol
	run     func(t *testing.T, client *mockLSP, warm tools.LSPWarmupFn, namePath string) resolverRun
}

// editCaller builds a resolverCaller for a single-file symbol-edit tool over
// fallbackFixture's demo.go (Alpha, then Beta). exec runs the tool against uri.
func editCaller(name, content string, exec func(client *mockLSP, store *topology.Store, warm tools.LSPWarmupFn, args map[string]any) (string, error)) resolverCaller {
	return resolverCaller{
		name:    name,
		target:  "Beta",
		holding: alphaBetaSymbols(),
		lacking: []protocol.DocumentSymbol{symbolAt("Alpha", 2, 4, 1)},
		run: func(t *testing.T, client *mockLSP, warm tools.LSPWarmupFn, namePath string) resolverRun {
			t.Helper()
			store, fpath, uri := fallbackFixture(t)
			before := readFileText(t, fpath)
			out, err := exec(client, store, warm, map[string]any{
				"uri": uri, "name_path": namePath, "content": content, "dry_run": false,
			})
			return resolverRun{out: out, err: err, wrote: func() bool { return readFileText(t, fpath) != before }}
		},
	}
}

func resolverCallers(t *testing.T) []resolverCaller {
	t.Helper()
	run := func(t *testing.T, tool executor, args map[string]any) (string, error) {
		t.Helper()
		return tool.Execute(context.Background(), slowFallbackArgs(t, args))
	}
	topo := func(s *topology.Store) func() *topology.Store { return func() *topology.Store { return s } }
	return []resolverCaller{
		editCaller("insert_before_symbol", "// added above Beta\n", func(c *mockLSP, s *topology.Store, w tools.LSPWarmupFn, a map[string]any) (string, error) {
			return run(t, tools.NewInsertBeforeSymbol(c, time.Second).WithTopologyFallback(topo(s)).WithLSPWarmup(w), a)
		}),
		editCaller("insert_after_symbol", "\n\nfunc Gamma() int { return 3 }", func(c *mockLSP, s *topology.Store, w tools.LSPWarmupFn, a map[string]any) (string, error) {
			return run(t, tools.NewInsertAfterSymbol(c, time.Second).WithTopologyFallback(topo(s)).WithLSPWarmup(w), a)
		}),
		editCaller("replace_symbol_body", "func Beta() int {\n\treturn 99\n}", func(c *mockLSP, s *topology.Store, w tools.LSPWarmupFn, a map[string]any) (string, error) {
			return run(t, tools.NewReplaceSymbolBody(c, time.Second).WithTopologyFallback(topo(s)).WithLSPWarmup(w), a)
		}),
		{
			name:    "move_symbol",
			target:  "Foo",
			holding: fooBarSymbols(),
			lacking: []protocol.DocumentSymbol{symbolAt("Bar", 5, 5, 27)},
			run: func(t *testing.T, client *mockLSP, warm tools.LSPWarmupFn, namePath string) resolverRun {
				t.Helper()
				dir := t.TempDir()
				srcPath, srcURI := writeInDir(t, dir, "src.go", moveSrc)
				dstPath, dstURI := writeInDir(t, dir, "dst.go", moveDstBefore)
				store := openTopologyStore(t, dir)
				out, err := run(t, tools.NewMoveSymbol(client, time.Second).WithTopologyFallback(topo(store)).WithLSPWarmup(warm), map[string]any{
					"source_uri": srcURI, "name_path": namePath, "destination_uri": dstURI, "dry_run": false,
				})
				return resolverRun{out: out, err: err, wrote: func() bool {
					return readFileText(t, srcPath) != moveSrc || readFileText(t, dstPath) != moveDstBefore
				}}
			},
		},
	}
}

func TestSymbolTools_HealthyServerResolvesWithoutABanner(t *testing.T) {
	for _, tc := range resolverCallers(t) {
		t.Run(tc.name, func(t *testing.T) {
			r := tc.run(t, &mockLSP{docSymbols: tc.holding}, nil, tc.target)
			if r.err != nil {
				t.Fatalf("a server that knows the symbol must resolve it: %v", r.err)
			}
			if !r.wrote() {
				t.Errorf("the edit never reached disk:\n%s", r.out)
			}
			if strings.Contains(r.out, "topology fallback") || strings.Contains(r.out, "LSP unavailable") {
				t.Errorf("a healthy server answered; the banner blames it anyway:\n%s", r.out)
			}
		})
	}
}

// TestSymbolTools_HealthyServerWithoutTheSymbolDoesNotFallBack is #571's first
// clause: the server answered, and its answer was "not in this file". The index
// still has the name, and used to be asked anyway.
func TestSymbolTools_HealthyServerWithoutTheSymbolDoesNotFallBack(t *testing.T) {
	for _, tc := range resolverCallers(t) {
		t.Run(tc.name, func(t *testing.T) {
			r := tc.run(t, &mockLSP{docSymbols: tc.lacking}, nil, tc.target)
			if r.err == nil {
				t.Fatalf("the server answered without %q; the fallback resolved it anyway:\n%s", tc.target, r.out)
			}
			if !strings.Contains(r.err.Error(), tc.target) || !strings.Contains(r.err.Error(), "not found") {
				t.Errorf("the refusal should say %q was not found: %v", tc.target, r.err)
			}
			if strings.Contains(r.err.Error(), "tree-sitter fallback finds no symbol") {
				t.Errorf("the tree-sitter fallback was consulted for a server that answered: %v", r.err)
			}
			if r.wrote() {
				t.Error("a refused edit must leave the files untouched")
			}
		})
	}
}

func TestSymbolTools_UnavailableServerStillFallsBack(t *testing.T) {
	for _, tc := range resolverCallers(t) {
		t.Run(tc.name, func(t *testing.T) {
			r := tc.run(t, &mockLSP{err: errors.New("lsp unavailable")}, nil, tc.target)
			if r.err != nil {
				t.Fatalf("a server that cannot answer must degrade to the tree-sitter fallback: %v", r.err)
			}
			if !r.wrote() {
				t.Errorf("the fallback resolved the symbol but nothing reached disk:\n%s", r.out)
			}
			if !strings.Contains(r.out, "topology fallback — LSP unavailable") {
				t.Errorf("the fallback must be announced, naming the server as unavailable:\n%s", r.out)
			}
		})
	}
}

// TestSymbolTools_ColdServerEmptyAnswerFallsBack: a cold server does not only
// fail — sourcekit-lsp and jdtls answer with an empty tree before indexing
// completes, and "no symbols" from one that is still warming is not an answer
// about the file. The same empty tree from a ready server is one.
func TestSymbolTools_ColdServerEmptyAnswerFallsBack(t *testing.T) {
	for _, tc := range resolverCallers(t) {
		t.Run(tc.name+"/warming", func(t *testing.T) {
			r := tc.run(t, &mockLSP{}, coldProbe(true), tc.target)
			if r.err != nil {
				t.Fatalf("a warming server's empty answer must degrade to the fallback: %v", r.err)
			}
			if !r.wrote() {
				t.Errorf("nothing reached disk:\n%s", r.out)
			}
			if !strings.Contains(r.out, "LSP still warming") {
				t.Errorf("the banner should say the server is still warming:\n%s", r.out)
			}
		})
		t.Run(tc.name+"/ready", func(t *testing.T) {
			r := tc.run(t, &mockLSP{}, coldProbe(false), tc.target)
			if r.err == nil {
				t.Fatalf("a ready server answered with no symbols; the fallback resolved one anyway:\n%s", r.out)
			}
			if r.wrote() {
				t.Error("a refused edit must leave the files untouched")
			}
		})
	}
}

// nestedBetaSymbols is the tree of a server that nests Beta inside a class and
// has no top-level Beta, while the Go file the index parses still declares one.
func nestedBetaSymbols() []protocol.DocumentSymbol {
	holder := protocol.DocumentSymbol{
		Name: "Holder", Kind: protocol.SKClass,
		Range:          protocol.Range{Start: protocol.Position{Line: 2}, End: protocol.Position{Line: 8, Character: 1}},
		SelectionRange: protocol.Range{Start: protocol.Position{Line: 2, Character: 5}, End: protocol.Position{Line: 2, Character: 11}},
		Children:       []protocol.DocumentSymbol{symbolAt("Beta", 6, 8, 1)},
	}
	return []protocol.DocumentSymbol{holder}
}

// TestSymbolTools_BareNestedNameIsRefusedWithThePath: a plain name addresses a
// top-level symbol. A server that nests the match answered "not at the top
// level", which is not licence to ask the index and edit whatever it finds; the
// refusal names the path that does resolve, and that path works.
func TestSymbolTools_BareNestedNameIsRefusedWithThePath(t *testing.T) {
	for _, tc := range resolverCallers(t) {
		if tc.name == "move_symbol" {
			continue // a nested symbol is move_symbol's own refusal: move_symbol_nested_test.go
		}
		t.Run(tc.name, func(t *testing.T) {
			r := tc.run(t, &mockLSP{docSymbols: nestedBetaSymbols()}, nil, "Beta")
			if r.err == nil {
				t.Fatalf("Beta is not a top-level symbol of the server's tree; the fallback edited the index's Beta:\n%s", r.out)
			}
			if !strings.Contains(r.err.Error(), `"Holder/Beta"`) {
				t.Errorf("the refusal should offer the name_path that resolves: %v", r.err)
			}
			if r.wrote() {
				t.Error("a refused edit must leave the file untouched")
			}

			r = tc.run(t, &mockLSP{docSymbols: nestedBetaSymbols()}, nil, "Holder/Beta")
			if r.err != nil {
				t.Fatalf("the path the refusal offered must resolve: %v", r.err)
			}
			if !r.wrote() || strings.Contains(r.out, "topology fallback") {
				t.Errorf("Holder/Beta should resolve through the server, no banner (wrote=%v):\n%s", r.wrote(), r.out)
			}
		})
	}
}
