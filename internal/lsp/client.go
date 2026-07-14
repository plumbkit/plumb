// Package lsp defines the Client interface and the process supervisor.
package lsp

import (
	"context"
	"encoding/json"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// NotificationHandler is invoked for each server-initiated notification.
// It must not block; spawn a goroutine if processing takes time.
type NotificationHandler func(method string, params json.RawMessage)

// Client defines the operations Plumb uses from any language server.
//
// All methods take a context.Context and return an error.  Implementations
// must document their concurrency contract in their type's doc comment.
type Client interface {
	// ── Lifecycle ───────────────────────────────────────────────────────────

	// Initialize sends the initialize request.  Must be called first.
	Initialize(ctx context.Context, params protocol.InitializeParams) (*protocol.InitializeResult, error)

	// Initialized sends the initialized notification (no response).
	// Must be called after Initialize succeeds.
	Initialized(ctx context.Context) error

	// Shutdown requests a clean shutdown.  Call Exit afterward.
	Shutdown(ctx context.Context) error

	// Exit sends the exit notification.  The server process should exit.
	Exit(ctx context.Context) error

	// ── Document lifecycle ──────────────────────────────────────────────────

	// DidOpen notifies the server that a document has been opened.
	DidOpen(ctx context.Context, params protocol.DidOpenTextDocumentParams) error

	// DidChange notifies the server of document changes.
	DidChange(ctx context.Context, params protocol.DidChangeTextDocumentParams) error

	// DidClose notifies the server that a document has been closed.
	DidClose(ctx context.Context, params protocol.DidCloseTextDocumentParams) error

	// DidChangeWatchedFiles notifies the server that one or more files on disk
	// changed outside the client's open-document set. This is the correct
	// primitive for "plumb just wrote this file" — it refreshes the server's
	// view without claiming buffer ownership.
	DidChangeWatchedFiles(ctx context.Context, params protocol.DidChangeWatchedFilesParams) error

	// ── Queries ─────────────────────────────────────────────────────────────

	// DocumentSymbols returns all symbols in the given document.
	DocumentSymbols(ctx context.Context, params protocol.DocumentSymbolParams) ([]protocol.DocumentSymbol, error)

	// WorkspaceSymbols searches for symbols matching query across the workspace.
	WorkspaceSymbols(ctx context.Context, params protocol.WorkspaceSymbolParams) ([]protocol.SymbolInformation, error)

	// Definition returns the location(s) of the symbol under the cursor.
	Definition(ctx context.Context, params protocol.DefinitionParams) ([]protocol.Location, error)

	// References returns all references to the symbol under the cursor.
	References(ctx context.Context, params protocol.ReferenceParams) ([]protocol.Location, error)

	// Hover returns hover information at the given position.
	Hover(ctx context.Context, params protocol.HoverParams) (*protocol.Hover, error)

	// ── Edits ───────────────────────────────────────────────────────────────

	// PrepareRename checks whether a rename is valid at the given position.
	// Returns nil if rename is not supported at that position.
	PrepareRename(ctx context.Context, params protocol.PrepareRenameParams) (*protocol.PrepareRenameResult, error)

	// Rename performs a workspace-wide rename.
	Rename(ctx context.Context, params protocol.RenameParams) (*protocol.WorkspaceEdit, error)

	// ── Call hierarchy ───────────────────────────────────────────────────────

	// PrepareCallHierarchy resolves the call-hierarchy item at pos.
	PrepareCallHierarchy(ctx context.Context, params protocol.PrepareCallHierarchyParams) ([]protocol.CallHierarchyItem, error)

	// IncomingCalls returns the callers of item.
	IncomingCalls(ctx context.Context, params protocol.CallHierarchyIncomingCallsParams) ([]protocol.CallHierarchyIncomingCall, error)

	// OutgoingCalls returns the callees of item.
	OutgoingCalls(ctx context.Context, params protocol.CallHierarchyOutgoingCallsParams) ([]protocol.CallHierarchyOutgoingCall, error)

	// ── Type hierarchy ───────────────────────────────────────────────────────

	// PrepareTypeHierarchy resolves the type-hierarchy item at pos.
	PrepareTypeHierarchy(ctx context.Context, params protocol.PrepareTypeHierarchyParams) ([]protocol.TypeHierarchyItem, error)

	// Supertypes returns the supertypes of item.
	Supertypes(ctx context.Context, params protocol.TypeHierarchySupertypesParams) ([]protocol.TypeHierarchyItem, error)

	// Subtypes returns the subtypes of item.
	Subtypes(ctx context.Context, params protocol.TypeHierarchySubtypesParams) ([]protocol.TypeHierarchyItem, error)

	// ── Capabilities ────────────────────────────────────────────────────────

	// Capabilities returns the negotiated server capabilities.
	// Returns nil if Initialize has not completed successfully.
	Capabilities() *protocol.ServerCapabilities

	// ── Notifications ───────────────────────────────────────────────────────

	// Subscribe registers handler to receive all server-initiated notifications.
	// The returned function unsubscribes the handler.
	Subscribe(handler func(string, json.RawMessage)) func()
}

// PullInitializer is an optional adapter capability. An adapter implements it to
// customise its InitializeParams for the LSP 3.17 pull-diagnostics model when the
// connection's resolved diagnostics mode is "pull". The pool type-asserts each
// adapter to this interface and, when present, calls EnablePullDiagnostics before
// sending initialize; an adapter with no pull-specific initialization options
// simply does not implement it, and the pool applies the generic client-capability
// swap (protocol.ClientCapabilitiesFor(true)) on its own.
type PullInitializer interface {
	// EnablePullDiagnostics mutates params in place to opt this server into the
	// pull model — at minimum advertising the pull client capability, plus any
	// server-specific initialization option (e.g. gopls's "pullDiagnostics").
	EnablePullDiagnostics(params *protocol.InitializeParams)
}
