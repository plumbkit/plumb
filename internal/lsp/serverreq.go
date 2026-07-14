package lsp

import (
	"encoding/json"

	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/lsp/watcher"
)

// ServerRequestExtension lets an adapter handle server-initiated requests
// beyond the shared capability-registration pair. It is consulted only for
// methods HandleServerRequest does not handle itself. Return handled=true to
// claim the method — result and err are then sent back to the server as-is.
// Return handled=false to decline; HandleServerRequest then answers with a
// *jsonrpc.MethodNotFoundError.
type ServerRequestExtension func(method string, params json.RawMessage) (result any, handled bool, err error)

// HandleServerRequest answers a server-initiated request on behalf of an
// adapter. Every adapter handles the same two methods identically:
// client/registerCapability records watcher glob patterns on w so
// DidChangeWatchedFiles can filter events, and client/unregisterCapability
// removes them. Both return (nil, nil) — an empty success response.
//
// extra, when non-nil, extends the handled method set per adapter (e.g.
// workspace/diagnostic/refresh) without touching this shared logic; it is
// never consulted for the registration methods above. Any method neither
// handled here nor claimed by extra is answered with a
// *jsonrpc.MethodNotFoundError carrying the method name, which the JSON-RPC
// layer maps to error code -32601.
func HandleServerRequest(w *watcher.Filter, method string, params json.RawMessage, extra ServerRequestExtension) (any, error) {
	switch method {
	case protocol.MethodRegisterCapability:
		w.Register(params)
		return nil, nil
	case protocol.MethodUnregisterCapability:
		w.Unregister(params)
		return nil, nil
	}
	if extra != nil {
		if result, handled, err := extra(method, params); handled {
			return result, err
		}
	}
	return nil, &jsonrpc.MethodNotFoundError{Method: method}
}
