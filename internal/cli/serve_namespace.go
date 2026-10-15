package cli

import (
	"encoding/json"
	"errors"

	"github.com/plumbkit/plumb/internal/mcp"
)

// The serve side of PLAN-520's namespaces: `plumb serve --namespace <token>`
// (or PLUMB_NAMESPACE) carries the token in every initialize it sends, and a
// replay the daemon refuses ends the proxy rather than retrying forever — a
// revoked namespace does not come back, and the proxy must never quietly carry
// on in the default namespace.

// errNamespaceRefused wraps the daemon's refusal of this proxy's namespace.
var errNamespaceRefused = errors.New("the daemon refused this connection's namespace")

// withNamespace adds the namespace token to the initialize _meta; an empty
// token leaves the meta unchanged.
func withNamespace(meta map[string]json.RawMessage, token string) map[string]json.RawMessage {
	if token == "" {
		return meta
	}
	if meta == nil {
		meta = map[string]json.RawMessage{}
	}
	if raw, err := json.Marshal(token); err == nil {
		meta[mcp.MetaNamespaceTokenKey] = raw
	}
	return meta
}

// namespaceRefusal reports whether frame is the daemon's namespace refusal, and
// its message.
func namespaceRefusal(frame []byte) (string, bool) {
	var resp struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(frame, &resp) != nil || resp.Error == nil || resp.Error.Code != mcp.CodeNamespaceRefused {
		return "", false
	}
	return resp.Error.Message, true
}

// serveNamespaceToken is the namespace a serve joins: --namespace, else
// PLUMB_NAMESPACE, else none.
func serveNamespaceToken(flag string, getenv func(string) string) string {
	if flag != "" {
		return flag
	}
	return getenv("PLUMB_NAMESPACE")
}

// legacyPumpNamespaceGuard refuses a namespace on the legacy byte pump: it
// cannot carry the token, and running in the default namespace instead would be
// exactly the fallback a namespace forbids.
func legacyPumpNamespaceGuard(namespace string) error {
	if namespace == "" {
		return nil
	}
	return errors.New("plumb serve: --namespace needs the resilient proxy; drop --no-reconnect")
}
