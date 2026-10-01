package tools_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/tools"
	"github.com/plumbkit/plumb/internal/topology"
)

// move_symbol_generic_test.go is PR #559's blocking review finding: for a
// generic receiver, move_symbol moved the WRONG method. The file below is the
// reviewer's repro — A's Run first, the generic S's Run second. The refusal for
// a bare "Run" offered "S[T]/Run"; passing it reported success and moved
// (a A) Run, because the topology index could not express S[T] as a receiver and
// topologyNodeByPath then fell back, silently, to the first node named Run.

const (
	aRunLine = "func (a A) Run() int { return 1 }"
	sRunLine = "func (s *S[T]) Run() int { return 2 }"

	genericMoveSrc = "package demo\n\n" +
		"type A struct{}\n\n" +
		"type S[T any] struct{}\n\n" +
		aRunLine + "\n\n" +
		sRunLine + "\n\n" +
		"func Free() {}\n"

	moveDstBefore = "package demo\n\nfunc Keep() {}\n"
)

// genericMoveSymbols is the tree gopls returns for genericMoveSrc: methods flat,
// the generic receiver spelled with its type parameters.
func genericMoveSymbols() []protocol.DocumentSymbol {
	return []protocol.DocumentSymbol{
		symbolAt("A", 2, 2, 15),
		symbolAt("S", 4, 4, 22),
		symbolAt("(A).Run", 6, 6, uint32(len(aRunLine))),
		symbolAt("(*S[T]).Run", 8, 8, uint32(len(sRunLine))),
		symbolAt("Free", 10, 10, 14),
	}
}

// moveServer is one way the symbol is resolved: the language server's own
// answer, the tree-sitter index alone, or both.
type moveServer struct {
	name     string
	lsp      func() *mockLSP
	topology bool
	// failed: the server errors rather than answering, so the index is the only
	// tree that can. A miss then needs saying aloud, because the server's own
	// error says nothing about the path.
	failed bool
}

var moveServers = []moveServer{
	{"healthy server, no index", func() *mockLSP { return &mockLSP{docSymbols: genericMoveSymbols()} }, false, false},
	{"healthy server and index", func() *mockLSP { return &mockLSP{docSymbols: genericMoveSymbols()} }, true, false},
	{"absent server, index fallback", func() *mockLSP { return &mockLSP{err: errors.New("lsp unavailable")} }, true, true},
}

// runGenericMove moves namePath out of genericMoveSrc under server. It returns
// both files as they are afterwards, and the tool's error.
func runGenericMove(t *testing.T, server moveServer, namePath string) (src, dst string, err error) {
	t.Helper()
	dir := t.TempDir()
	srcPath, srcURI := writeInDir(t, dir, "src.go", genericMoveSrc)
	dstPath, dstURI := writeInDir(t, dir, "dst.go", moveDstBefore)
	tool := tools.NewMoveSymbol(server.lsp(), time.Second)
	if server.topology {
		store := openTopologyStore(t, dir)
		tool = tool.WithTopologyFallback(func() *topology.Store { return store })
	}
	_, err = tool.Execute(context.Background(), moveArgs(t, map[string]any{
		"source_uri": srcURI, "name_path": namePath, "destination_uri": dstURI, "dry_run": false,
	}))
	return readFileText(t, srcPath), readFileText(t, dstPath), err
}

// TestMoveSymbol_GenericReceiverMovesTheRightMethod: each name_path the refusal
// can offer — and the spelling an agent would try from the source, S[T]/Run —
// moves the method of THAT receiver, however the symbol is resolved, and leaves
// the other where it was.
func TestMoveSymbol_GenericReceiverMovesTheRightMethod(t *testing.T) {
	for _, server := range moveServers {
		for _, tc := range []struct {
			namePath    string
			moved, kept string
		}{
			{"A/Run", aRunLine, sRunLine},
			{"S/Run", sRunLine, aRunLine},
			{"S[T]/Run", sRunLine, aRunLine},
		} {
			t.Run(server.name+"/"+tc.namePath, func(t *testing.T) {
				src, dst, err := runGenericMove(t, server, tc.namePath)
				if err != nil {
					t.Fatalf("move %s: %v", tc.namePath, err)
				}
				if strings.Contains(src, tc.moved) || !strings.Contains(src, tc.kept) {
					t.Errorf("source: want %q gone and %q kept:\n%s", tc.moved, tc.kept, src)
				}
				if !strings.Contains(dst, tc.moved) || strings.Contains(dst, tc.kept) {
					t.Errorf("destination: want %q only:\n%s", tc.moved, dst)
				}
			})
		}
	}
}

// TestMoveSymbol_PathWithNoMatchingParentIsRefused is the silent wrong answer
// itself: a name_path whose parent matches nothing must be refused with both
// files untouched, never resolved to some other method named Run.
func TestMoveSymbol_PathWithNoMatchingParentIsRefused(t *testing.T) {
	for _, server := range moveServers {
		for _, namePath := range []string{"Nope/Run", "B[T]/Run", "A/Free"} {
			t.Run(server.name+"/"+namePath, func(t *testing.T) {
				src, dst, err := runGenericMove(t, server, namePath)
				if err == nil {
					t.Fatalf("%s names no declaration; it moved one anyway:\nsource:\n%s\ndestination:\n%s", namePath, src, dst)
				}
				if !strings.Contains(err.Error(), namePath) {
					t.Errorf("the refusal should name the path it could not resolve: %v", err)
				}
				// A server that answered "not found" needs no second opinion quoted
				// back; one that failed to answer does, or the agent is left with
				// "language server unavailable" about a path no retry can resolve.
				if said := strings.Contains(err.Error(), "tree-sitter fallback finds no symbol"); said != server.failed {
					t.Errorf("fallback miss noted = %v, want %v: %v", said, server.failed, err)
				}
				if src != genericMoveSrc || dst != moveDstBefore {
					t.Errorf("a refused move must leave both files untouched:\nsource:\n%s\ndestination:\n%s", src, dst)
				}
			})
		}
	}
}

// TestMoveSymbol_AmbiguousRunOffersPathsThatResolve: the refusal for a bare
// "Run" names a path for each method, with the receiver as the index and the
// server name it — S, not S[T] — and each of those paths is then proven to move
// exactly the method it was offered for (the test above).
func TestMoveSymbol_AmbiguousRunOffersPathsThatResolve(t *testing.T) {
	for _, server := range moveServers[:2] {
		t.Run(server.name, func(t *testing.T) {
			src, dst, err := runGenericMove(t, server, "Run")
			if err == nil || !strings.Contains(err.Error(), "ambiguous") {
				t.Fatalf("two methods named Run must be refused as ambiguous, got %v", err)
			}
			for _, want := range []string{`"A/Run"`, `"S/Run"`} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal should offer %s: %v", want, err)
				}
			}
			if strings.Contains(err.Error(), "S[T]/Run") {
				t.Errorf("the refusal offers a path with type parameters, which no resolver follows: %v", err)
			}
			if src != genericMoveSrc || dst != moveDstBefore {
				t.Errorf("a refused move must leave both files untouched:\nsource:\n%s\ndestination:\n%s", src, dst)
			}
		})
	}
}
