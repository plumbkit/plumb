package pyright

import (
	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/adapters/base"
	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// pyrightInitOptions holds pyright-specific initialization options.
// See https://github.com/microsoft/pyright/blob/main/docs/configuration.md
type pyrightInitOptions struct {
	PythonVersion    string `json:"pythonVersion,omitempty"`
	TypeCheckingMode string `json:"typeCheckingMode,omitempty"`
}

// Adapter implements lsp.Client for pyright-langserver.
//
// Pyright uses a slightly different workspace model from gopls: it expects a
// rootUri pointing to the project root and reads its configuration from
// pyrightconfig.json or pyproject.toml if present. It may register file
// watchers via client/registerCapability, which the embedded base answers so
// DidChangeWatchedFiles events are filtered to the registered globs. Pyright
// requires full-document sync (SyncFull) by default, so callers should send the
// complete document text in each DidChange event.
//
// Every lsp.Client method is inherited from base.Adapter, which labels each
// error "pyright <label>: <cause>"; this package adds only pyright's
// initialisation options.
//
// Concurrency: all exported methods are safe for concurrent use.
type Adapter struct{ *base.Adapter }

// Compile-time contract check: a mis-signed method fails here, in this package,
// rather than as a confusing error wherever the adapter is used as an lsp.Client.
var _ lsp.Client = (*Adapter)(nil)

// New creates an Adapter wired to conn.  The caller must call Initialize before
// any query method.
func New(conn jsonrpc.Caller) *Adapter {
	return &Adapter{Adapter: base.New(conn, "pyright")}
}

// DefaultInitParams returns InitializeParams suitable for pyright.
// rootURI must be a file:// URI pointing to the Python project root.
func DefaultInitParams(rootURI string) protocol.InitializeParams {
	return protocol.InitializeParams{
		ProcessID:    protocol.ProcessID(),
		ClientInfo:   &protocol.ClientInfo{Name: "plumb", Version: "dev"},
		RootURI:      rootURI,
		Capabilities: protocol.DefaultClientCapabilities(),
		InitializationOptions: pyrightInitOptions{
			PythonVersion:    "3.12",
			TypeCheckingMode: "basic",
		},
	}
}
