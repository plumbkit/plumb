package lsp

import (
	"encoding/json"

	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/lsp/watcher"
)

// HandleServerRequest answers a server-initiated request on behalf of an
// adapter. Every adapter handles the same two methods identically:
// client/registerCapability records watcher glob patterns on w so
// DidChangeWatchedFiles can filter events, and client/unregisterCapability
// removes them. Both return (nil, nil) — an empty success response.
//
// Any other method is answered with a *jsonrpc.MethodNotFoundError carrying the
// method name, which the JSON-RPC layer maps to error code -32601. Methods that
// need more than this shared registration handling — e.g.
// workspace/diagnostic/refresh — are intercepted one layer above the adapter by
// the pool's connection-handler wrapper (internal/cli/pool_diagnostics.go's
// wrapServerRequest), so this helper stays uniform across all nine adapters and
// needs no per-adapter extension point.
func HandleServerRequest(w *watcher.Filter, method string, params json.RawMessage) (any, error) {
	switch method {
	case protocol.MethodRegisterCapability:
		w.Register(params)
		return nil, nil
	case protocol.MethodUnregisterCapability:
		w.Unregister(params)
		return nil, nil
	}
	return nil, &jsonrpc.MethodNotFoundError{Method: method}
}
