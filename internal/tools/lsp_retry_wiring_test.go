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
)

// notReadyOnceMock rejects the FIRST call to each per-document method with a
// sourcekit-lsp-style "No language service for" / "Failed to find snapshot
// for" error, then answers normally — reproducing the build-graph-indexing
// race the 2026-08 error autopsy found behind get_definition (83% server-state),
// find_references (36%), and rename_symbol (50%) failures against sourcekit-lsp.
type notReadyOnceMock struct {
	*mockLSP
	defCalls, refCalls, renCalls int
}

func (m *notReadyOnceMock) Definition(ctx context.Context, p protocol.DefinitionParams) ([]protocol.Location, error) {
	m.defCalls++
	if m.defCalls == 1 {
		return nil, errors.New("sourcekit-lsp documentSymbol: jsonrpc error -32001: No language service for 'file:///p/x.swift' found")
	}
	return m.mockLSP.Definition(ctx, p)
}

func (m *notReadyOnceMock) References(ctx context.Context, p protocol.ReferenceParams) ([]protocol.Location, error) {
	m.refCalls++
	if m.refCalls == 1 {
		return nil, errors.New("sourcekit-lsp: jsonrpc error -32001: No language service for 'file:///p/x.swift' found")
	}
	return m.mockLSP.References(ctx, p)
}

func (m *notReadyOnceMock) Rename(ctx context.Context, p protocol.RenameParams) (*protocol.WorkspaceEdit, error) {
	m.renCalls++
	if m.renCalls == 1 {
		return nil, errors.New("sourcekit-lsp rename: jsonrpc error -32001: Failed to find snapshot for 'file:///p/x.swift'")
	}
	return m.mockLSP.Rename(ctx, p)
}

// TestGetDefinition_RetriesOnceOnServerNotReady is the PLAN-363 item-5 repro
// for get_definition: a "No language service for" rejection is retried once,
// transparently, instead of failing the call.
func TestGetDefinition_RetriesOnceOnServerNotReady(t *testing.T) {
	m := &notReadyOnceMock{mockLSP: &mockLSP{
		locations: []protocol.Location{{URI: "file:///p/x.swift", Range: protocol.Range{Start: protocol.Position{Line: 5, Character: 5}}}},
	}}
	tool := tools.NewGetDefinition(m, nil, time.Minute, 0)
	args, _ := json.Marshal(map[string]any{"uri": "file:///p/x.swift", "line": 5, "character": 5})

	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("expected the retry to recover, got error: %v", err)
	}
	if !strings.Contains(out, "Definition at") {
		t.Errorf("expected a definition after the retry, got:\n%s", out)
	}
	if m.defCalls != 2 {
		t.Errorf("expected exactly 2 Definition calls (one retry), got %d", m.defCalls)
	}
}

// TestFindReferences_RetriesOnceOnServerNotReady is the PLAN-363 item-5 repro
// for find_references.
func TestFindReferences_RetriesOnceOnServerNotReady(t *testing.T) {
	m := &notReadyOnceMock{mockLSP: &mockLSP{
		locations: []protocol.Location{{URI: "file:///p/x.swift", Range: protocol.Range{Start: protocol.Position{Line: 5, Character: 5}}}},
	}}
	tool := tools.NewFindReferences(m, nil, time.Minute, 0)
	args, _ := json.Marshal(map[string]any{"uri": "file:///p/x.swift", "line": 5, "character": 5})

	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("expected the retry to recover, got error: %v", err)
	}
	if !strings.Contains(out, "reference(s)") {
		t.Errorf("expected references after the retry, got:\n%s", out)
	}
	if m.refCalls != 2 {
		t.Errorf("expected exactly 2 References calls (one retry), got %d", m.refCalls)
	}
}

// TestRenameSymbol_RetriesOnceOnServerNotReady is the PLAN-363 item-5 repro
// for rename_symbol ("Failed to find snapshot for", sourcekit-lsp's rename
// flavour of the same build-graph race).
func TestRenameSymbol_RetriesOnceOnServerNotReady(t *testing.T) {
	m := &notReadyOnceMock{mockLSP: &mockLSP{
		renameResult: &protocol.WorkspaceEdit{Changes: map[string][]protocol.TextEdit{
			"file:///p/x.swift": {{Range: protocol.Range{Start: protocol.Position{Line: 0, Character: 0}, End: protocol.Position{Line: 0, Character: 3}}, NewText: "Bar"}},
		}},
	}}
	tool := tools.NewRenameSymbol(m, time.Minute)
	args, _ := json.Marshal(map[string]any{"uri": "file:///p/x.swift", "line": 0, "character": 0, "new_name": "Bar", "dry_run": true})

	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("expected the retry to recover, got error: %v", err)
	}
	if !strings.Contains(out, "Renamed to") {
		t.Errorf("expected a rename preview after the retry, got:\n%s", out)
	}
	if m.renCalls != 2 {
		t.Errorf("expected exactly 2 Rename calls (one retry), got %d", m.renCalls)
	}
}
