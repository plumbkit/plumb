package cli

import (
	"os"
	"strings"
)

// The Claude Code and Codex hooks (`plumb hooks run-claude`, run-codex) speak
// to the daemon's control socket for a session: its mailbox at Stop, its
// context hints, the /clear handover. A session that runs in a namespace
// (PLAN-520) has its serves started with PLUMB_NAMESPACE, and the hooks Claude
// Code runs for it inherit the same environment — so a hook forwards that token
// and the daemon answers only from that namespace's connections. A hook without
// a token speaks for the default namespace; an unknown, released or expired
// token is answered by nobody, never by the default namespace.

// hookNamespaceToken is the namespace token the hook process runs under: the
// PLUMB_NAMESPACE its session's serves carry, or "" for the default namespace.
func hookNamespaceToken() string { return strings.TrimSpace(os.Getenv("PLUMB_NAMESPACE")) }

// namespaceForToken resolves a hook request's token to the namespace id it is
// scoped to: "" for no token (the default namespace); ok is false for a token
// the registry refuses, which then matches no connection at all.
func (r *connRegistry) namespaceForToken(token string) (string, bool) {
	if token == "" {
		return "", true
	}
	ns, err := r.namespaces.resolve(token)
	if err != nil {
		return "", false
	}
	return ns.ID, true
}

// registryCtrlHandlers are the control-socket handlers the connection
// registry answers: the project reload, namespaces, and the hooks' requests —
// the /clear marker (kept beside the connections that consume it), the mailbox
// probe, and the resolver that scopes each hook request to its namespace.
func registryCtrlHandlers(r *connRegistry) ctrlHandlers {
	return ctrlHandlers{
		reloadProject:       r.reloadProject,
		namespace:           r.namespaces.handleCtrl,
		conversationCleared: r.conversationClears().mark,
		mailbox:             r.mailboxProbe,
		resolveNamespace:    r.namespaceForToken,
	}
}

// hookRequestNamespace is the namespace a hook request is answered from: the
// handlers' resolver's answer, or — with no resolver wired — the default
// namespace for a request without a token and nobody for one with a token
// (never a silent fallback to the default namespace).
func hookRequestNamespace(h ctrlHandlers, token string) (string, bool) {
	if h.resolveNamespace == nil {
		return "", token == ""
	}
	return h.resolveNamespace(token)
}

// handleNamespace is a registered connection's namespace ("" for the default,
// and for a handle built without one in a test).
func handleNamespace(h connHandle) string {
	if h.namespace == nil {
		return ""
	}
	return h.namespace()
}

// namespacedID qualifies a conversation id by its namespace wherever the daemon
// keys per-conversation state a hook reaches — a /clear marker, so a hook in
// one namespace cannot hand over a conversation in another, and the context
// hints' allowance, so two namespaces reusing an id do not share a budget. The
// default namespace keeps the bare id, so its existing keys are unchanged. A
// /clear id never holds ':' (handleClearCommand drops one that does), so a
// default-namespace id cannot spell another namespace's marker.
func namespacedID(namespace, id string) string {
	if namespace == "" {
		return id
	}
	return namespace + ":" + id
}
