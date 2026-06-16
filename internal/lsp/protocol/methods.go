// Package protocol defines LSP 3.17 types and method name constants.
package protocol

// Method name constants for the LSP methods Plumb uses.
const (
	MethodInitialize            = "initialize"
	MethodInitialized           = "initialized"
	MethodShutdown              = "shutdown"
	MethodExit                  = "exit"
	MethodDidOpen               = "textDocument/didOpen"
	MethodDidChange             = "textDocument/didChange"
	MethodDidClose              = "textDocument/didClose"
	MethodDidChangeWatchedFiles = "workspace/didChangeWatchedFiles"
	MethodRegisterCapability    = "client/registerCapability"
	MethodUnregisterCapability  = "client/unregisterCapability"
	MethodDocumentSymbols       = "textDocument/documentSymbol"
	MethodWorkspaceSymbols      = "workspace/symbol"
	MethodDefinition            = "textDocument/definition"
	MethodReferences            = "textDocument/references"
	MethodHover                 = "textDocument/hover"
	MethodPrepareRename         = "textDocument/prepareRename"
	MethodRename                = "textDocument/rename"
	MethodPublishDiagnostics    = "textDocument/publishDiagnostics"
	MethodDiagnostic            = "textDocument/diagnostic"

	// Call hierarchy
	MethodPrepareCallHierarchy  = "textDocument/prepareCallHierarchy"
	MethodCallHierarchyIncoming = "callHierarchy/incomingCalls"
	MethodCallHierarchyOutgoing = "callHierarchy/outgoingCalls"

	// Type hierarchy
	MethodPrepareTypeHierarchy = "textDocument/prepareTypeHierarchy"
	MethodTypeHierarchySuper   = "typeHierarchy/supertypes"
	MethodTypeHierarchySub     = "typeHierarchy/subtypes"
)
