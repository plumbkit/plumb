package tools_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/tools"
)

// goplsMethodFixture is the file and document-symbol tree gopls reports for it:
// Go methods are TOP-LEVEL symbols named "(*Recv).Method", never children of
// the receiver type, so a plain method name equals no symbol name exactly.
const goplsMethodSource = `package p

type WriteTracker struct{}

func (w *WriteTracker) WroteMtime() int {
	return 1
}

func (w *WriteTracker) Close() {}

func (r ReadTracker) Close() {}
`

func goplsMethodSymbols() []protocol.DocumentSymbol {
	sym := func(name string, kind protocol.SymbolKind, line, char, end uint32) protocol.DocumentSymbol {
		return protocol.DocumentSymbol{
			Name:           name,
			Kind:           kind,
			Range:          protocol.Range{Start: protocol.Position{Line: line}, End: protocol.Position{Line: end}},
			SelectionRange: protocol.Range{Start: protocol.Position{Line: line, Character: char}},
		}
	}
	return []protocol.DocumentSymbol{
		sym("WriteTracker", protocol.SKStruct, 2, 5, 2),
		sym("(*WriteTracker).WroteMtime", protocol.SKMethod, 4, 23, 6),
		sym("(*WriteTracker).Close", protocol.SKMethod, 8, 23, 8),
		sym("(ReadTracker).Close", protocol.SKMethod, 10, 21, 10),
	}
}

func writeGoplsMethodFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "p.go")
	if err := os.WriteFile(path, []byte(goplsMethodSource), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestPlainGoMethodName_ResolvesInAllThreeTools is issue #546 item 1: a plain
// method name ("WroteMtime") resolves in find_references and get_definition
// exactly as it does in read_symbol, and all three land on the same symbol —
// not "No symbol named …" from two of them.
func TestPlainGoMethodName_ResolvesInAllThreeTools(t *testing.T) {
	path := writeGoplsMethodFile(t)
	uri := "file://" + path
	want := protocol.Position{Line: 4, Character: 23}

	refs := &mockLSP{docSymbols: goplsMethodSymbols(), locations: []protocol.Location{{URI: uri, Range: protocol.Range{Start: want}}}}
	out, err := tools.NewFindReferences(refs, nil, time.Minute, 0).
		Execute(context.Background(), rawArgs(t, map[string]any{"uri": uri, "symbol_name": "WroteMtime"}))
	if err != nil {
		t.Fatalf("find_references: %v", err)
	}
	if strings.Contains(out, "No symbol named") || refs.lastRefPos != want {
		t.Fatalf("find_references did not resolve the plain method name (queried %+v, want %+v):\n%s", refs.lastRefPos, want, out)
	}

	def := &mockLSP{docSymbols: goplsMethodSymbols(), locations: []protocol.Location{{URI: uri, Range: protocol.Range{Start: want}}}}
	out, err = tools.NewGetDefinition(def, nil, time.Minute, 0).
		Execute(context.Background(), rawArgs(t, map[string]any{"uri": uri, "symbol_name": "WroteMtime"}))
	if err != nil {
		t.Fatalf("get_definition: %v", err)
	}
	if strings.Contains(out, "No symbol named") || def.lastDefPos != want {
		t.Fatalf("get_definition did not resolve the plain method name (queried %+v, want %+v):\n%s", def.lastDefPos, want, out)
	}

	rs := &mockLSP{docSymbols: goplsMethodSymbols()}
	out, err = tools.NewReadSymbol(rs, nil, time.Minute, 0, tools.NewReadTracker()).
		Execute(context.Background(), rawArgs(t, map[string]any{"path": path, "name": "WroteMtime"}))
	if err != nil {
		t.Fatalf("read_symbol: %v", err)
	}
	if !strings.Contains(out, "return 1") {
		t.Fatalf("read_symbol did not resolve the plain method name through the language server:\n%s", out)
	}
}

// TestPlainGoMethodName_AmbiguousListsEveryMatch: a plain name two receivers
// share is never resolved to one of them silently. find_references and
// get_definition follow read_symbol's convention and answer for every match,
// each labelled with its receiver.
func TestPlainGoMethodName_AmbiguousListsEveryMatch(t *testing.T) {
	path := writeGoplsMethodFile(t)
	uri := "file://" + path
	args := rawArgs(t, map[string]any{"uri": uri, "symbol_name": "Close"})
	locs := []protocol.Location{{URI: uri, Range: protocol.Range{Start: protocol.Position{Line: 8}}}}

	refs, err := tools.NewFindReferences(&mockLSP{docSymbols: goplsMethodSymbols(), locations: locs}, nil, time.Minute, 0).
		Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("find_references: %v", err)
	}
	def, err := tools.NewGetDefinition(&mockLSP{docSymbols: goplsMethodSymbols(), locations: locs}, nil, time.Minute, 0).
		Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("get_definition: %v", err)
	}
	for tool, out := range map[string]string{"find_references": refs, "get_definition": def} {
		for _, want := range []string{"2 ", "(*WriteTracker).Close", "(ReadTracker).Close"} {
			if !strings.Contains(out, want) {
				t.Errorf("%s: ambiguous plain name must list every match (missing %q):\n%s", tool, want, out)
			}
		}
	}
}

// TestLSPCrossFileCallers_AmbiguousPlainNameAnswersNothing: topology_impact
// passes a Go method's bare name, which several receivers in one file can
// share. The resolver must not report one receiver's callers as another's.
func TestLSPCrossFileCallers_AmbiguousPlainNameAnswersNothing(t *testing.T) {
	mock := &mockLSP{
		docSymbols: goplsMethodSymbols(),
		locations:  []protocol.Location{loc("file:///ws/other.go", 3)},
	}
	fn := tools.NewLSPCrossFileCallers(mock, nil, time.Minute, 0, func() string { return "/ws" })
	if sites := fn(context.Background(), "p.go", "Close"); sites != nil {
		t.Errorf("an ambiguous name must answer nothing rather than guess a receiver, got %+v", sites)
	}
	if sites := fn(context.Background(), "p.go", "WroteMtime"); len(sites) != 1 {
		t.Errorf("a unique plain method name must resolve its cross-file callers, got %+v", sites)
	}
}

func rawArgs(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
