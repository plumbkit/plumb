package tools_test

import (
	"context"
	"encoding/json"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// mockLSP implements lsp.Client for tool unit tests.
// Set the relevant field before each test; err applies to every method.
type mockLSP struct {
	wsSymbols    []protocol.SymbolInformation
	docSymbols   []protocol.DocumentSymbol
	locations    []protocol.Location
	hover        *protocol.Hover
	caps         *protocol.ServerCapabilities
	err          error
	renameResult *protocol.WorkspaceEdit // returned by Rename when non-nil
	block        bool                    // when true, query methods wait for ctx cancellation

	// Call-hierarchy responses (nil by default → same as an empty server).
	chItems    []protocol.CallHierarchyItem
	chIncoming []protocol.CallHierarchyIncomingCall
	chOutgoing []protocol.CallHierarchyOutgoingCall

	// lastDefPos / lastRefPos record the Position of the most recent
	// Definition / References call, so a test can assert the tool queried the
	// identifier (DocumentSymbol SelectionRange) rather than the declaration
	// start (the keyword). See TestGetDefinition_ByName_UsesSelectionRange.
	lastDefPos protocol.Position
	lastRefPos protocol.Position
	lastRefURI string // URI of the most recent References call (asserts path absolutisation)
}

func (m *mockLSP) Initialize(_ context.Context, _ protocol.InitializeParams) (*protocol.InitializeResult, error) {
	return &protocol.InitializeResult{}, m.err
}
func (m *mockLSP) Initialized(_ context.Context) error { return m.err }
func (m *mockLSP) Shutdown(_ context.Context) error    { return m.err }
func (m *mockLSP) Exit(_ context.Context) error        { return m.err }
func (m *mockLSP) DidOpen(_ context.Context, _ protocol.DidOpenTextDocumentParams) error {
	return m.err
}

func (m *mockLSP) DidChange(_ context.Context, _ protocol.DidChangeTextDocumentParams) error {
	return m.err
}

func (m *mockLSP) DidClose(_ context.Context, _ protocol.DidCloseTextDocumentParams) error {
	return m.err
}

func (m *mockLSP) DidChangeWatchedFiles(_ context.Context, _ protocol.DidChangeWatchedFilesParams) error {
	return m.err
}

func (m *mockLSP) WorkspaceSymbols(ctx context.Context, _ protocol.WorkspaceSymbolParams) ([]protocol.SymbolInformation, error) {
	if m.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return m.wsSymbols, m.err
}

func (m *mockLSP) DocumentSymbols(ctx context.Context, _ protocol.DocumentSymbolParams) ([]protocol.DocumentSymbol, error) {
	if m.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return m.docSymbols, m.err
}

func (m *mockLSP) Definition(_ context.Context, p protocol.DefinitionParams) ([]protocol.Location, error) {
	m.lastDefPos = p.Position
	return m.locations, m.err
}

func (m *mockLSP) References(_ context.Context, p protocol.ReferenceParams) ([]protocol.Location, error) {
	m.lastRefPos = p.Position
	m.lastRefURI = p.TextDocument.URI
	return m.locations, m.err
}

func (m *mockLSP) Hover(_ context.Context, _ protocol.HoverParams) (*protocol.Hover, error) {
	return m.hover, m.err
}

func (m *mockLSP) PrepareRename(_ context.Context, _ protocol.PrepareRenameParams) (*protocol.PrepareRenameResult, error) {
	return nil, m.err
}

func (m *mockLSP) Rename(_ context.Context, _ protocol.RenameParams) (*protocol.WorkspaceEdit, error) {
	return m.renameResult, m.err
}

func (m *mockLSP) PrepareCallHierarchy(_ context.Context, _ protocol.PrepareCallHierarchyParams) ([]protocol.CallHierarchyItem, error) {
	return m.chItems, m.err
}

func (m *mockLSP) IncomingCalls(_ context.Context, _ protocol.CallHierarchyIncomingCallsParams) ([]protocol.CallHierarchyIncomingCall, error) {
	return m.chIncoming, m.err
}

func (m *mockLSP) OutgoingCalls(_ context.Context, _ protocol.CallHierarchyOutgoingCallsParams) ([]protocol.CallHierarchyOutgoingCall, error) {
	return m.chOutgoing, m.err
}

func (m *mockLSP) PrepareTypeHierarchy(_ context.Context, _ protocol.PrepareTypeHierarchyParams) ([]protocol.TypeHierarchyItem, error) {
	return nil, m.err
}

func (m *mockLSP) Supertypes(_ context.Context, _ protocol.TypeHierarchySupertypesParams) ([]protocol.TypeHierarchyItem, error) {
	return nil, m.err
}

func (m *mockLSP) Subtypes(_ context.Context, _ protocol.TypeHierarchySubtypesParams) ([]protocol.TypeHierarchyItem, error) {
	return nil, m.err
}
func (m *mockLSP) Capabilities() *protocol.ServerCapabilities       { return m.caps }
func (m *mockLSP) Subscribe(_ func(string, json.RawMessage)) func() { return func() {} }
