package rust

import (
	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/adapters/base"
	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// Adapter implements lsp.Client for rust-analyzer.
//
// rust-analyzer expects a rootUri pointing at the Cargo workspace root (the
// directory containing Cargo.toml) and reads its configuration from
// rust-analyzer.toml / the workspace Cargo manifest. It registers file watchers
// dynamically via client/registerCapability, which the embedded base answers so
// DidChangeWatchedFiles events are filtered to the registered globs.
//
// Every lsp.Client method is inherited from base.Adapter, which labels each
// error "rust-analyzer <label>: <cause>"; rust-analyzer needs nothing
// server-specific beyond its InitializeParams.
//
// Concurrency: all exported methods are safe for concurrent use.
type Adapter struct{ *base.Adapter }

// Compile-time contract check: a mis-signed method fails here, in this package,
// rather than as a confusing error wherever the adapter is used as an lsp.Client.
var _ lsp.Client = (*Adapter)(nil)

// New creates an Adapter wired to conn. The caller must call Initialize before
// any query method.
func New(conn jsonrpc.Caller) *Adapter {
	return &Adapter{Adapter: base.New(conn, "rust-analyzer")}
}

// DefaultInitParams returns InitializeParams suitable for rust-analyzer.
// rootURI must be a file:// URI pointing to the Cargo workspace root.
// rust-analyzer needs no initialization options for plumb's use — it reads its
// configuration from the workspace — so none are sent.
func DefaultInitParams(rootURI string) protocol.InitializeParams {
	return protocol.InitializeParams{
		ProcessID:    protocol.ProcessID(),
		ClientInfo:   &protocol.ClientInfo{Name: "plumb", Version: "dev"},
		RootURI:      rootURI,
		Capabilities: protocol.DefaultClientCapabilities(),
	}
}
