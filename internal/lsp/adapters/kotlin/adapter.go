package kotlin

import (
	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/adapters/base"
	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// Adapter implements lsp.Client for kotlin-language-server.
//
// kotlin-language-server expects a rootUri pointing at the project root (a
// Gradle/Maven module) and resolves the classpath from the build files. It may
// register file watchers dynamically via client/registerCapability, which the
// embedded base answers so DidChangeWatchedFiles events are filtered to the
// registered globs.
//
// Every lsp.Client method is inherited from base.Adapter, which labels each
// error "kotlin-language-server <label>: <cause>"; kotlin-language-server needs
// nothing server-specific beyond its InitializeParams.
//
// Concurrency: all exported methods are safe for concurrent use.
type Adapter struct{ *base.Adapter }

// Compile-time contract check: a mis-signed method fails here, in this package,
// rather than as a confusing error wherever the adapter is used as an lsp.Client.
var _ lsp.Client = (*Adapter)(nil)

// New creates an Adapter wired to conn. The caller must call Initialize before
// any query method.
func New(conn jsonrpc.Caller) *Adapter {
	return &Adapter{Adapter: base.New(conn, "kotlin-language-server")}
}

// DefaultInitParams returns InitializeParams suitable for
// kotlin-language-server. rootURI must be a file:// URI pointing to the project
// root. No initialization options are required for plumb's use.
func DefaultInitParams(rootURI string) protocol.InitializeParams {
	return protocol.InitializeParams{
		ProcessID:    protocol.ProcessID(),
		ClientInfo:   &protocol.ClientInfo{Name: "plumb", Version: "dev"},
		RootURI:      rootURI,
		Capabilities: protocol.DefaultClientCapabilities(),
	}
}
