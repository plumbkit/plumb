package protocol

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// ─── Primitives ──────────────────────────────────────────────────────────────

// Position is a zero-based line and character offset.
type Position struct {
	Line      uint32 `json:"line"`
	Character uint32 `json:"character"`
}

// Range is a start/end position pair.
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// Location is a URI + Range.
type Location struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
}

// ─── Text document ───────────────────────────────────────────────────────────

// TextDocumentIdentifier names a text document by URI.
type TextDocumentIdentifier struct {
	URI string `json:"uri"`
}

// VersionedTextDocumentIdentifier includes a version number.
type VersionedTextDocumentIdentifier struct {
	URI     string `json:"uri"`
	Version int32  `json:"version"`
}

// TextDocumentItem is used when opening a document.
type TextDocumentItem struct {
	URI        string `json:"uri"`
	LanguageID string `json:"languageId"`
	Version    int32  `json:"version"`
	Text       string `json:"text"`
}

// TextDocumentContentChangeEvent describes a change to an open document.
// When Range is nil the entire document is replaced by Text.
type TextDocumentContentChangeEvent struct {
	Range *Range `json:"range,omitempty"`
	Text  string `json:"text"`
}

// TextDocumentPositionParams addresses a position in a document.
type TextDocumentPositionParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
	Position     Position               `json:"position"`
}

// ─── Symbols ─────────────────────────────────────────────────────────────────

// SymbolKind enumerates LSP symbol kinds.
type SymbolKind int

const (
	SKFile          SymbolKind = 1
	SKModule        SymbolKind = 2
	SKNamespace     SymbolKind = 3
	SKPackage       SymbolKind = 4
	SKClass         SymbolKind = 5
	SKMethod        SymbolKind = 6
	SKProperty      SymbolKind = 7
	SKField         SymbolKind = 8
	SKConstructor   SymbolKind = 9
	SKEnum          SymbolKind = 10
	SKInterface     SymbolKind = 11
	SKFunction      SymbolKind = 12
	SKVariable      SymbolKind = 13
	SKConstant      SymbolKind = 14
	SKString        SymbolKind = 15
	SKNumber        SymbolKind = 16
	SKBoolean       SymbolKind = 17
	SKArray         SymbolKind = 18
	SKObject        SymbolKind = 19
	SKKey           SymbolKind = 20
	SKNull          SymbolKind = 21
	SKEnumMember    SymbolKind = 22
	SKStruct        SymbolKind = 23
	SKEvent         SymbolKind = 24
	SKOperator      SymbolKind = 25
	SKTypeParameter SymbolKind = 26
)

// DocumentSymbol is a symbol in a text document (hierarchical).
type DocumentSymbol struct {
	Name           string           `json:"name"`
	Detail         string           `json:"detail,omitempty"`
	Kind           SymbolKind       `json:"kind"`
	Range          Range            `json:"range"`
	SelectionRange Range            `json:"selectionRange"`
	Children       []DocumentSymbol `json:"children,omitempty"`
}

// SymbolInformation is a flat symbol for workspace symbol results.
type SymbolInformation struct {
	Name          string     `json:"name"`
	Kind          SymbolKind `json:"kind"`
	Location      Location   `json:"location"`
	ContainerName string     `json:"containerName,omitempty"`
}

// ─── Hover ───────────────────────────────────────────────────────────────────

// MarkupContent holds text in either plaintext or markdown.
type MarkupContent struct {
	Kind  string `json:"kind"` // "plaintext" | "markdown"
	Value string `json:"value"`
}

// Hover is the response to a hover request.
type Hover struct {
	Contents MarkupContent `json:"contents"`
	Range    *Range        `json:"range,omitempty"`
}

// ─── Edits ───────────────────────────────────────────────────────────────────

// TextEdit replaces a range in a document.
type TextEdit struct {
	Range   Range  `json:"range"`
	NewText string `json:"newText"`
}

// TextDocumentEdit is a set of edits for a specific document version.
type TextDocumentEdit struct {
	TextDocument VersionedTextDocumentIdentifier `json:"textDocument"`
	Edits        []TextEdit                      `json:"edits"`
}

// WorkspaceEdit is the result of a rename or code action.
type WorkspaceEdit struct {
	Changes         map[string][]TextEdit `json:"changes,omitempty"`
	DocumentChanges []TextDocumentEdit    `json:"documentChanges,omitempty"`
}

// PrepareRenameResult is the response to a prepareRename request.
type PrepareRenameResult struct {
	Range       Range  `json:"range"`
	Placeholder string `json:"placeholder"`
}

// ─── Diagnostics ─────────────────────────────────────────────────────────────

// DiagnosticSeverity enumerates diagnostic severity levels.
type DiagnosticSeverity int

const (
	SevError       DiagnosticSeverity = 1
	SevWarning     DiagnosticSeverity = 2
	SevInformation DiagnosticSeverity = 3
	SevHint        DiagnosticSeverity = 4
)

// Diagnostic reports a problem in a document.
type Diagnostic struct {
	Range    Range              `json:"range"`
	Severity DiagnosticSeverity `json:"severity,omitempty"`
	Code     any                `json:"code,omitempty"`
	Source   string             `json:"source,omitempty"`
	Message  string             `json:"message"`
}

// PublishDiagnosticsParams is the payload for textDocument/publishDiagnostics.
type PublishDiagnosticsParams struct {
	URI         string       `json:"uri"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

// ─── Capabilities ────────────────────────────────────────────────────────────

// BoolOrOptions represents an LSP capability field that may be a boolean true
// or a more detailed options object.  We parse it as "enabled" (true when either
// true or a non-null object) and preserve the raw JSON for callers that need
// the options detail.
type BoolOrOptions struct {
	Enabled bool
	Raw     json.RawMessage
}

func (b *BoolOrOptions) UnmarshalJSON(data []byte) error {
	if string(data) == "true" {
		b.Enabled = true
		return nil
	}
	if string(data) == "false" || string(data) == "null" {
		return nil
	}
	b.Enabled = true
	b.Raw = data
	return nil
}

func (b BoolOrOptions) MarshalJSON() ([]byte, error) {
	if !b.Enabled {
		return []byte("false"), nil
	}
	if len(b.Raw) > 0 {
		return b.Raw, nil
	}
	return []byte("true"), nil
}

// TextDocumentSyncKind describes how documents are synced.
type TextDocumentSyncKind int

const (
	SyncNone        TextDocumentSyncKind = 0
	SyncFull        TextDocumentSyncKind = 1
	SyncIncremental TextDocumentSyncKind = 2
)

// TextDocumentSyncOptions describes document sync capabilities.
type TextDocumentSyncOptions struct {
	OpenClose bool                 `json:"openClose,omitempty"`
	Change    TextDocumentSyncKind `json:"change"`
}

// RenameOptions describes rename capabilities.
type RenameOptions struct {
	PrepareProvider bool `json:"prepareProvider,omitempty"`
}

// ServerCapabilities lists what an LSP server supports.
type ServerCapabilities struct {
	TextDocumentSync        *TextDocumentSyncOptions `json:"textDocumentSync,omitempty"`
	HoverProvider           *BoolOrOptions           `json:"hoverProvider,omitempty"`
	DefinitionProvider      *BoolOrOptions           `json:"definitionProvider,omitempty"`
	ReferencesProvider      *BoolOrOptions           `json:"referencesProvider,omitempty"`
	DocumentSymbolProvider  *BoolOrOptions           `json:"documentSymbolProvider,omitempty"`
	WorkspaceSymbolProvider *BoolOrOptions           `json:"workspaceSymbolProvider,omitempty"`
	RenameProvider          json.RawMessage          `json:"renameProvider,omitempty"`
}

// RenameEnabled reports whether the server supports rename.
func (c *ServerCapabilities) RenameEnabled() bool {
	if len(c.RenameProvider) == 0 {
		return false
	}
	var b BoolOrOptions
	_ = json.Unmarshal(c.RenameProvider, &b)
	return b.Enabled
}

// PrepareRenameEnabled reports whether the server supports prepareRename.
func (c *ServerCapabilities) PrepareRenameEnabled() bool {
	if len(c.RenameProvider) == 0 {
		return false
	}
	var opts RenameOptions
	if err := json.Unmarshal(c.RenameProvider, &opts); err != nil {
		return false
	}
	return opts.PrepareProvider
}

// ─── Client capabilities ──────────────────────────────────────────────────────

// DefaultClientCapabilities returns the capabilities Plumb advertises.
//
// workspace.didChangeWatchedFiles.dynamicRegistration is true: this tells
// the server it may use client/registerCapability to register the file
// globs it wants to watch (gopls registers patterns like **/*.go, **/go.mod
// once this is declared). The adapter's request handler responds OK to
// these registrations — we don't track them, but accepting is essential so
// gopls actually consumes our workspace/didChangeWatchedFiles notifications.
func DefaultClientCapabilities() ClientCapabilities {
	return ClientCapabilities{
		TextDocument: TextDocumentClientCapabilities{
			Synchronization: &TextDocumentSyncClientCapabilities{DynamicRegistration: false},
			DocumentSymbol:  &DocumentSymbolClientCapabilities{HierarchicalDocumentSymbolSupport: true},
		},
		Workspace: WorkspaceClientCapabilities{
			Symbol:                &WorkspaceSymbolClientCapabilities{},
			DidChangeWatchedFiles: &DidChangeWatchedFilesClientCapabilities{DynamicRegistration: true},
		},
	}
}

// ClientCapabilities describes what the client supports.
type ClientCapabilities struct {
	TextDocument TextDocumentClientCapabilities `json:"textDocument"`
	Workspace    WorkspaceClientCapabilities    `json:"workspace"`
}

// TextDocumentClientCapabilities describes client text-document capabilities.
type TextDocumentClientCapabilities struct {
	Synchronization *TextDocumentSyncClientCapabilities `json:"synchronization,omitempty"`
	DocumentSymbol  *DocumentSymbolClientCapabilities   `json:"documentSymbol,omitempty"`
}

// TextDocumentSyncClientCapabilities describes sync client capabilities.
type TextDocumentSyncClientCapabilities struct {
	DynamicRegistration bool `json:"dynamicRegistration,omitempty"`
}

// DocumentSymbolClientCapabilities describes document symbol client capabilities.
type DocumentSymbolClientCapabilities struct {
	HierarchicalDocumentSymbolSupport bool `json:"hierarchicalDocumentSymbolSupport,omitempty"`
}

// WorkspaceClientCapabilities describes workspace-level client capabilities.
type WorkspaceClientCapabilities struct {
	Symbol                *WorkspaceSymbolClientCapabilities       `json:"symbol,omitempty"`
	DidChangeWatchedFiles *DidChangeWatchedFilesClientCapabilities `json:"didChangeWatchedFiles,omitempty"`
}

// WorkspaceSymbolClientCapabilities describes workspace symbol client capabilities.
type WorkspaceSymbolClientCapabilities struct{}

// DidChangeWatchedFilesClientCapabilities lets the client declare it can
// receive (and the server can dynamically register) watched-file events.
// Required for gopls to register file watchers via client/registerCapability.
type DidChangeWatchedFilesClientCapabilities struct {
	DynamicRegistration bool `json:"dynamicRegistration,omitempty"`
	// RelativePatternSupport: omitted — gopls falls back to absolute globs.
}

// ─── Initialize ───────────────────────────────────────────────────────────────

// ClientInfo identifies the MCP client (plumb) to the language server.
type ClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// InitializeParams is sent with the initialize request.
type InitializeParams struct {
	ProcessID             *int32             `json:"processId"`
	ClientInfo            *ClientInfo        `json:"clientInfo,omitempty"`
	RootURI               string             `json:"rootUri"`
	Capabilities          ClientCapabilities `json:"capabilities"`
	InitializationOptions any                `json:"initializationOptions,omitempty"`
}

// ServerInfo identifies the language server.
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// InitializeResult is the response from the server.
type InitializeResult struct {
	Capabilities ServerCapabilities `json:"capabilities"`
	ServerInfo   *ServerInfo        `json:"serverInfo,omitempty"`
}

// ProcessID returns the current process ID as an *int32 for InitializeParams.
func ProcessID() *int32 {
	pid := int32(os.Getpid())
	return &pid
}

// FileURI converts an absolute path to a file:// URI.
// Safe on Windows: backslashes are converted to forward slashes and a drive
// letter (e.g. C:\path) is prefixed with an extra slash → file:///C:/path.
func FileURI(path string) string {
	path = filepath.ToSlash(path)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return "file://" + path
}

// ─── Request / notification params ───────────────────────────────────────────

// DidOpenTextDocumentParams is the payload for textDocument/didOpen.
type DidOpenTextDocumentParams struct {
	TextDocument TextDocumentItem `json:"textDocument"`
}

// DidChangeTextDocumentParams is the payload for textDocument/didChange.
type DidChangeTextDocumentParams struct {
	TextDocument   VersionedTextDocumentIdentifier  `json:"textDocument"`
	ContentChanges []TextDocumentContentChangeEvent `json:"contentChanges"`
}

// DidCloseTextDocumentParams is the payload for textDocument/didClose.
type DidCloseTextDocumentParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
}

// FileChangeType enumerates the kinds of filesystem events reported via
// workspace/didChangeWatchedFiles. Values match the LSP spec.
type FileChangeType int

const (
	FileCreated FileChangeType = 1
	FileChanged FileChangeType = 2
	FileDeleted FileChangeType = 3
)

// FileEvent describes one filesystem change.
type FileEvent struct {
	URI  string         `json:"uri"`
	Type FileChangeType `json:"type"`
}

// DidChangeWatchedFilesParams is the payload for workspace/didChangeWatchedFiles.
// This is the LSP-correct primitive for telling the server about external
// (non-client-owned) file changes — use it whenever a tool writes to disk
// behind the server's back. Prefer this over the didOpen/didChange/didClose
// dance, which is for editor-managed buffers and forces the server to treat
// the client as the source of truth.
type DidChangeWatchedFilesParams struct {
	Changes []FileEvent `json:"changes"`
}

// DocumentSymbolParams is the payload for textDocument/documentSymbol.
type DocumentSymbolParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
}

// WorkspaceSymbolParams is the payload for workspace/symbol.
type WorkspaceSymbolParams struct {
	Query string `json:"query"`
}

// DefinitionParams is the payload for textDocument/definition.
type DefinitionParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
	Position     Position               `json:"position"`
}

// ReferenceContext controls whether the declaration is included in results.
type ReferenceContext struct {
	IncludeDeclaration bool `json:"includeDeclaration"`
}

// ReferenceParams is the payload for textDocument/references.
type ReferenceParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
	Position     Position               `json:"position"`
	Context      ReferenceContext       `json:"context"`
}

// HoverParams is the payload for textDocument/hover.
type HoverParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
	Position     Position               `json:"position"`
}

// PrepareRenameParams is the payload for textDocument/prepareRename.
type PrepareRenameParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
	Position     Position               `json:"position"`
}

// RenameParams is the payload for textDocument/rename.
type RenameParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
	Position     Position               `json:"position"`
	NewName      string                 `json:"newName"`
}

// ── Call hierarchy ────────────────────────────────────────────────────────────

// CallHierarchyItem represents a call-hierarchy node (function/method).
type CallHierarchyItem struct {
	Name           string     `json:"name"`
	Kind           SymbolKind `json:"kind"`
	URI            string     `json:"uri"`
	Range          Range      `json:"range"`
	SelectionRange Range      `json:"selectionRange"`
	Detail         string     `json:"detail,omitempty"`
}

// PrepareCallHierarchyParams is the payload for textDocument/prepareCallHierarchy.
type PrepareCallHierarchyParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
	Position     Position               `json:"position"`
}

// CallHierarchyIncomingCallsParams is the payload for callHierarchy/incomingCalls.
type CallHierarchyIncomingCallsParams struct {
	Item CallHierarchyItem `json:"item"`
}

// CallHierarchyIncomingCall is one caller in the incoming-call graph.
type CallHierarchyIncomingCall struct {
	From       CallHierarchyItem `json:"from"`
	FromRanges []Range           `json:"fromRanges"`
}

// CallHierarchyOutgoingCallsParams is the payload for callHierarchy/outgoingCalls.
type CallHierarchyOutgoingCallsParams struct {
	Item CallHierarchyItem `json:"item"`
}

// CallHierarchyOutgoingCall is one callee in the outgoing-call graph.
type CallHierarchyOutgoingCall struct {
	To         CallHierarchyItem `json:"to"`
	FromRanges []Range           `json:"fromRanges"`
}

// ── Type hierarchy ────────────────────────────────────────────────────────────

// TypeHierarchyItem represents a type-hierarchy node (class/interface/struct).
type TypeHierarchyItem struct {
	Name           string     `json:"name"`
	Kind           SymbolKind `json:"kind"`
	URI            string     `json:"uri"`
	Range          Range      `json:"range"`
	SelectionRange Range      `json:"selectionRange"`
	Detail         string     `json:"detail,omitempty"`
}

// PrepareTypeHierarchyParams is the payload for textDocument/prepareTypeHierarchy.
type PrepareTypeHierarchyParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
	Position     Position               `json:"position"`
}

// TypeHierarchySupertypesParams is the payload for typeHierarchy/supertypes.
type TypeHierarchySupertypesParams struct {
	Item TypeHierarchyItem `json:"item"`
}

// TypeHierarchySubtypesParams is the payload for typeHierarchy/subtypes.
type TypeHierarchySubtypesParams struct {
	Item TypeHierarchyItem `json:"item"`
}
