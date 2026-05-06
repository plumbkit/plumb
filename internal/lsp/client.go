// Package lsp defines the LSPClient interface and the process supervisor.
package lsp

import (
	"context"
	"encoding/json"

	"github.com/golimpio/plumb/internal/lsp/protocol"
)

// NotificationHandler is invoked for each server-initiated notification.
// It must not block; spawn a goroutine if processing takes time.
type NotificationHandler func(method string, params json.RawMessage)

// LSPClient defines the operations Plumb uses from any language server.
//
// All methods take a context.Context and return an error.  Implementations
// must document their concurrency contract in their type's doc comment.
type LSPClient interface {
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

	// ── Capabilities ────────────────────────────────────────────────────────

	// Capabilities returns the negotiated server capabilities.
	// Returns nil if Initialize has not completed successfully.
	Capabilities() *protocol.ServerCapabilities

	// ── Notifications ───────────────────────────────────────────────────────

	// Subscribe registers handler to receive all server-initiated notifications.
	// The returned function unsubscribes the handler.
	Subscribe(handler NotificationHandler) (unsubscribe func())
}
