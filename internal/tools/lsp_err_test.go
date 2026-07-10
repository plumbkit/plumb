package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/tools"
)

// A by-name caller supplies no coordinates, so a server rejection of the position
// plumb resolved for it must not be explained with a hint about line/character
// arguments it never passed. rename_symbol got this in #174; the three nav tools
// share the same by-name resolution and now share the same error flavour.

// rejectingMock answers DocumentSymbols but fails every positional query, as a
// server with a stale index does when handed a position from its own symbol tree.
type rejectingMock struct {
	mockLSP
	err error
}

func (m *rejectingMock) References(context.Context, protocol.ReferenceParams) ([]protocol.Location, error) {
	return nil, m.err
}

func (m *rejectingMock) Definition(context.Context, protocol.DefinitionParams) ([]protocol.Location, error) {
	return nil, m.err
}

func (m *rejectingMock) PrepareCallHierarchy(context.Context, protocol.PrepareCallHierarchyParams) ([]protocol.CallHierarchyItem, error) {
	return nil, m.err
}

func rejectingFixture(t *testing.T) (*rejectingMock, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "foo.go")
	if err := os.WriteFile(path, []byte("package p\n\nfunc Foo() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := &rejectingMock{err: errors.New("no identifier found")}
	m.docSymbols = []protocol.DocumentSymbol{{
		Name:           "Foo",
		Range:          protocol.Range{Start: protocol.Position{Line: 2, Character: 0}, End: protocol.Position{Line: 2, Character: 13}},
		SelectionRange: protocol.Range{Start: protocol.Position{Line: 2, Character: 5}, End: protocol.Position{Line: 2, Character: 8}},
	}}
	return m, "file://" + path
}

// assertNameFlavouredErr checks the error explains a stale symbol tree and never
// points the caller at coordinates it did not pass.
func assertNameFlavouredErr(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected the server rejection to surface as an error")
	}
	if strings.Contains(err.Error(), "0-based") {
		t.Errorf("a symbol_name caller must not be pointed at line/character it never passed: %v", err)
	}
	if !strings.Contains(err.Error(), `symbol "Foo"`) || !strings.Contains(err.Error(), "index is stale") {
		t.Errorf("expected a stale-symbol-tree hint naming the symbol, got: %v", err)
	}
}

func TestFindReferences_ByName_ServerRejectionHintDoesNotMentionCoordinates(t *testing.T) {
	m, uri := rejectingFixture(t)
	args, _ := json.Marshal(map[string]any{"uri": uri, "symbol_name": "Foo"})
	_, err := tools.NewFindReferences(m, nil, 0, 0).Execute(context.Background(), args)
	assertNameFlavouredErr(t, err)
}

func TestGetDefinition_ByName_ServerRejectionHintDoesNotMentionCoordinates(t *testing.T) {
	m, uri := rejectingFixture(t)
	args, _ := json.Marshal(map[string]any{"uri": uri, "symbol_name": "Foo"})
	_, err := tools.NewGetDefinition(m, nil, 0, 0).Execute(context.Background(), args)
	assertNameFlavouredErr(t, err)
}

func TestCallHierarchy_ByName_ServerRejectionHintDoesNotMentionCoordinates(t *testing.T) {
	m, uri := rejectingFixture(t)
	args, _ := json.Marshal(map[string]any{"uri": uri, "symbol_name": "Foo"})
	_, err := tools.NewCallHierarchy(m, 0).Execute(context.Background(), args)
	assertNameFlavouredErr(t, err)
}

// A raw-position caller keeps the coordinate hint: it did choose the coordinates.
// The position miss first snaps to the enclosing symbol; when the retry also
// fails, the surfaced error is the positional one.
func TestFindReferences_ByPosition_KeepsCoordinateHint(t *testing.T) {
	m, uri := rejectingFixture(t)
	m.err = errors.New("boom") // not a position miss, so no snap
	args, _ := json.Marshal(map[string]any{"uri": uri, "line": 2, "character": 5})
	_, err := tools.NewFindReferences(m, nil, 0, 0).Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected the server error to surface")
	}
	if !strings.Contains(err.Error(), "0-based") {
		t.Errorf("a raw-position caller keeps the coordinate hint, got: %v", err)
	}
}
