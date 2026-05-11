package tools_test

import (
	"context"
	"encoding/json"

	"github.com/golimpio/plumb/internal/lsp/protocol"
)

// mockLSP implements lsp.LSPClient for tool unit tests.
// Set the relevant field before each test; err applies to every method.
type mockLSP struct {
	wsSymbols  []protocol.SymbolInformation
	docSymbols []protocol.DocumentSymbol
	locations  []protocol.Location
	hover      *protocol.Hover
	caps       *protocol.ServerCapabilities
	err        error
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
func (m *mockLSP) WorkspaceSymbols(_ context.Context, _ protocol.WorkspaceSymbolParams) ([]protocol.SymbolInformation, error) {
	return m.wsSymbols, m.err
}
func (m *mockLSP) DocumentSymbols(_ context.Context, _ protocol.DocumentSymbolParams) ([]protocol.DocumentSymbol, error) {
	return m.docSymbols, m.err
}
func (m *mockLSP) Definition(_ context.Context, _ protocol.DefinitionParams) ([]protocol.Location, error) {
	return m.locations, m.err
}
func (m *mockLSP) References(_ context.Context, _ protocol.ReferenceParams) ([]protocol.Location, error) {
	return m.locations, m.err
}
func (m *mockLSP) Hover(_ context.Context, _ protocol.HoverParams) (*protocol.Hover, error) {
	return m.hover, m.err
}
func (m *mockLSP) PrepareRename(_ context.Context, _ protocol.PrepareRenameParams) (*protocol.PrepareRenameResult, error) {
	return nil, m.err
}
func (m *mockLSP) Rename(_ context.Context, _ protocol.RenameParams) (*protocol.WorkspaceEdit, error) {
	return nil, m.err
}
func (m *mockLSP) PrepareCallHierarchy(_ context.Context, _ protocol.PrepareCallHierarchyParams) ([]protocol.CallHierarchyItem, error) {
	return nil, m.err
}
func (m *mockLSP) IncomingCalls(_ context.Context, _ protocol.CallHierarchyIncomingCallsParams) ([]protocol.CallHierarchyIncomingCall, error) {
	return nil, m.err
}
func (m *mockLSP) OutgoingCalls(_ context.Context, _ protocol.CallHierarchyOutgoingCallsParams) ([]protocol.CallHierarchyOutgoingCall, error) {
	return nil, m.err
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
func (m *mockLSP) Capabilities() *protocol.ServerCapabilities { return m.caps }
func (m *mockLSP) Subscribe(_ func(string, json.RawMessage)) func() { return func() {} }
