package cli

// routing_proxy_activation.go — which language servers have actually served this
// connection. The routing proxy picks a server per file; this records the
// secondaries it reached, so the session record, daemon_info's lsp row, and
// session_start's guidance can describe what is really serving the caller rather
// than only the primary acquired at attach time.

import (
	"sort"
	"strings"
)

// setActivateHook wires the callback fired when a secondary language server
// first serves a request under the connection's workspace. Pass nil to clear it
// (done on a workspace re-pin so a switched connection starts with a clean
// adapter set).
func (r *routingProxy) setActivateHook(fn func(language string)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onActivate = fn
}

// routedLanguages returns the sorted, deduplicated set of non-primary languages
// that have actually served this connection. Empty means nothing has routed —
// which is honest, not a fallback: a language server nothing has asked for is
// not "attached" to this session in any sense the caller can use.
func (r *routingProxy) routedLanguages() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.activated) == 0 {
		return nil
	}
	out := make([]string, 0, len(r.activated))
	for lang := range r.activated {
		out = append(out, lang)
	}
	sort.Strings(out)
	return out
}

// noteActivated reports a secondary language server coming live for a file
// inside the connection's pinned workspace, so the session record and the
// daemon_info lsp row can surface every active LSP (not just the primary). It
// fires for any language other than the primary whose file resolves to the
// connection's workspace root or the primary root — or a directory beneath
// either — since a secondary's own root marker (e.g. index.html for HTML) makes
// Detect carve out a sub-root (site/), so a strict root== check would miss it.
// It does NOT fire for a genuinely different project reached by cross-workspace
// routing.
//
// wsRoot is checked as well as primaryRoot because a connection may have NO
// primary at all: a LanguageNone root whose files are served purely by per-file
// routing never acquires one, and gating on primaryRoot alone (which is "") made
// every such session report an empty adapter list and "lsp: none attached" while
// a routed server answered its queries. setDiscovered sets wsRoot on every attach
// path, including the ones that acquire nothing.
func (r *routingProxy) noteActivated(root, language string) {
	r.mu.Lock()
	cb := r.onActivate
	primaryLang := r.primaryLang
	within := withinRoot(root, r.wsRoot) || withinRoot(root, r.primaryRoot)
	if language == primaryLang || !within {
		r.mu.Unlock()
		return
	}
	if r.activated == nil {
		r.activated = make(map[string]struct{}, 1)
	}
	r.activated[language] = struct{}{}
	r.mu.Unlock()
	if cb != nil {
		cb(language)
	}
}

// withinRoot reports whether path is root itself or a descendant directory of it.
func withinRoot(path, root string) bool {
	if root == "" {
		return false
	}
	return path == root || strings.HasPrefix(path, root+"/")
}
