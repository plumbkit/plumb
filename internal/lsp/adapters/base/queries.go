package base

import (
	"context"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// The forwarded half of lsp.Client: every method here is a labelled call with
// no server-specific behaviour. Each label is the string plumb surfaces to
// agents on failure and is pinned by conformance.RunErrorContract.

// ── Queries ──────────────────────────────────────────────────────────────────

// DocumentSymbols returns all symbols in the document.
func (a *Adapter) DocumentSymbols(ctx context.Context, params protocol.DocumentSymbolParams) ([]protocol.DocumentSymbol, error) {
	return Call[[]protocol.DocumentSymbol](ctx, a, "documentSymbol", protocol.MethodDocumentSymbols, params)
}

// WorkspaceSymbols searches for symbols matching the query.
func (a *Adapter) WorkspaceSymbols(ctx context.Context, params protocol.WorkspaceSymbolParams) ([]protocol.SymbolInformation, error) {
	return Call[[]protocol.SymbolInformation](ctx, a, "workspaceSymbol", protocol.MethodWorkspaceSymbols, params)
}

// Definition returns the definition location(s) for the symbol at pos.
func (a *Adapter) Definition(ctx context.Context, params protocol.DefinitionParams) ([]protocol.Location, error) {
	return Call[[]protocol.Location](ctx, a, "definition", protocol.MethodDefinition, params)
}

// References returns all references to the symbol at pos.
func (a *Adapter) References(ctx context.Context, params protocol.ReferenceParams) ([]protocol.Location, error) {
	return Call[[]protocol.Location](ctx, a, "references", protocol.MethodReferences, params)
}

// Hover returns hover information at pos.
func (a *Adapter) Hover(ctx context.Context, params protocol.HoverParams) (*protocol.Hover, error) {
	return CallPtr[protocol.Hover](ctx, a, "hover", protocol.MethodHover, params)
}

// ── Edits ────────────────────────────────────────────────────────────────────

// PrepareRename checks whether rename is valid at pos.
func (a *Adapter) PrepareRename(ctx context.Context, params protocol.PrepareRenameParams) (*protocol.PrepareRenameResult, error) {
	return CallPtr[protocol.PrepareRenameResult](ctx, a, "prepareRename", protocol.MethodPrepareRename, params)
}

// Rename performs a workspace-wide rename.
func (a *Adapter) Rename(ctx context.Context, params protocol.RenameParams) (*protocol.WorkspaceEdit, error) {
	return CallPtr[protocol.WorkspaceEdit](ctx, a, "rename", protocol.MethodRename, params)
}

// ── Call hierarchy ───────────────────────────────────────────────────────────

// PrepareCallHierarchy resolves the call-hierarchy item at pos.
func (a *Adapter) PrepareCallHierarchy(ctx context.Context, params protocol.PrepareCallHierarchyParams) ([]protocol.CallHierarchyItem, error) {
	return Call[[]protocol.CallHierarchyItem](ctx, a, "prepareCallHierarchy", protocol.MethodPrepareCallHierarchy, params)
}

// IncomingCalls returns the callers of item.
func (a *Adapter) IncomingCalls(ctx context.Context, params protocol.CallHierarchyIncomingCallsParams) ([]protocol.CallHierarchyIncomingCall, error) {
	return Call[[]protocol.CallHierarchyIncomingCall](ctx, a, "callHierarchy/incomingCalls", protocol.MethodCallHierarchyIncoming, params)
}

// OutgoingCalls returns the callees of item.
func (a *Adapter) OutgoingCalls(ctx context.Context, params protocol.CallHierarchyOutgoingCallsParams) ([]protocol.CallHierarchyOutgoingCall, error) {
	return Call[[]protocol.CallHierarchyOutgoingCall](ctx, a, "callHierarchy/outgoingCalls", protocol.MethodCallHierarchyOutgoing, params)
}

// ── Type hierarchy ───────────────────────────────────────────────────────────

// PrepareTypeHierarchy resolves the type-hierarchy item at pos.
func (a *Adapter) PrepareTypeHierarchy(ctx context.Context, params protocol.PrepareTypeHierarchyParams) ([]protocol.TypeHierarchyItem, error) {
	return Call[[]protocol.TypeHierarchyItem](ctx, a, "prepareTypeHierarchy", protocol.MethodPrepareTypeHierarchy, params)
}

// Supertypes returns the supertypes of item.
func (a *Adapter) Supertypes(ctx context.Context, params protocol.TypeHierarchySupertypesParams) ([]protocol.TypeHierarchyItem, error) {
	return Call[[]protocol.TypeHierarchyItem](ctx, a, "typeHierarchy/supertypes", protocol.MethodTypeHierarchySuper, params)
}

// Subtypes returns the subtypes of item.
func (a *Adapter) Subtypes(ctx context.Context, params protocol.TypeHierarchySubtypesParams) ([]protocol.TypeHierarchyItem, error) {
	return Call[[]protocol.TypeHierarchyItem](ctx, a, "typeHierarchy/subtypes", protocol.MethodTypeHierarchySub, params)
}
