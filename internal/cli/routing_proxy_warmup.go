package cli

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/plumbkit/plumb/internal/paths"
)

// Warm-up surfacing for the routing proxy: the not-ready error formatting and
// the resolution-only WarmupStatus query a tool or session_start uses to fail
// fast with an elapsed-time advisory instead of blocking on a cold handshake.
// Split from routing_proxy.go to keep that file under the size cap.

// warmingErr formats the error returned when a routed language server's handshake
// has not yet completed. It folds in elapsed warm-up time and points the caller
// at the tools that answer immediately, so an agent retries (or switches to
// topology) rather than reading a bare "not yet ready" as a hard failure. root
// is appended for the per-file routing case; pass "" for the primary.
func warmingErr(elapsed time.Duration, root string) error {
	loc := ""
	if root != "" {
		loc = " for " + root
	}
	if elapsed <= 0 {
		return fmt.Errorf("LSP server not yet ready%s — it is still starting up; retry shortly "+
			"(topology_search / find_symbol / file_outline answer now)", loc)
	}
	return fmt.Errorf("LSP server still warming%s (%s elapsed) — retry shortly "+
		"(topology_search / find_symbol / file_outline answer now)", loc, roundWarmElapsed(elapsed))
}

// roundWarmElapsed rounds a warm-up duration to a human-friendly precision:
// 100 ms under a second, whole seconds beyond.
func roundWarmElapsed(d time.Duration) time.Duration {
	if d < time.Second {
		return d.Round(100 * time.Millisecond)
	}
	return d.Round(time.Second)
}

// WarmupStatus reports whether the language server that would serve uri (or the
// connection's primary, when uri is empty) is still warming up, and for how
// long. Resolution-only — it never starts a server — so a tool or session_start
// can fail fast with an elapsed-time advisory rather than block on a cold
// handshake. Returns (false, 0) when the target is ready or cannot be resolved.
func (r *routingProxy) WarmupStatus(uri string) (warming bool, elapsed time.Duration) {
	root, language := r.warmupTarget(uri)
	if root == "" || language == "" || language == LanguageNone {
		return false, 0
	}
	return r.pool.warmupFor(root, language)
}

// warmupTarget resolves the (root, language) WarmupStatus inspects for uri: the
// connection primary when uri is empty, else the URI's detected root and
// per-file language (mirroring route()). Falls back to the primary when URI
// resolution fails.
func (r *routingProxy) warmupTarget(uri string) (root, language string) {
	if uri == "" {
		r.mu.RLock()
		defer r.mu.RUnlock()
		return r.primaryRoot, r.primaryLang
	}
	path := paths.URIToPath(uri)
	root, language, err := r.pool.Detect(filepath.Dir(path))
	if err != nil {
		r.mu.RLock()
		defer r.mu.RUnlock()
		return r.primaryRoot, r.primaryLang
	}
	if fileLang := r.pool.fileLanguage(path); fileLang != "" {
		language = fileLang
	}
	return root, language
}
