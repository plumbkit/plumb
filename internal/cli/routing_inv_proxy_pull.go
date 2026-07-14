package cli

import (
	"path/filepath"

	"github.com/plumbkit/plumb/internal/cache"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/paths"
)

// routing_inv_proxy_pull.go — the pull-diagnostics state surface of the
// session's routing invalidator proxy. The diagnostics tool and the post-write
// pull path record textDocument/diagnostic results and read result IDs through
// these methods; each routes to the Invalidator of the workspace entry owning
// the URI, mirroring Diagnostics()'s resolution, so pull state always lands in
// the same cache that serves the merged (push+pull) read view.

// owningInv resolves the Invalidator owning uri, or nil when the URI belongs to
// a workspace that has never been acquired. Empty uri (and any resolution
// failure) falls back to the primary, mirroring Diagnostics().
func (r *routingInvProxy) owningInv(uri string) *cache.Invalidator {
	r.mu.RLock()
	primaryRoot := r.primaryRoot
	primaryLang := r.primaryLang
	primary := r.primary
	r.mu.RUnlock()

	if uri == "" {
		return primary
	}
	path := paths.URIToPath(uri)
	root, language, err := r.pool.Detect(filepath.Dir(path))
	targetLang := r.routeLang(path, language)
	if err != nil || (root == primaryRoot && targetLang == primaryLang) {
		return primary
	}
	if e := r.pool.lookup(root, targetLang); e != nil {
		return e.inv
	}
	return nil
}

// PullResultID returns the previousResultId to send on the next
// textDocument/diagnostic request for uri, routed to the owning workspace's
// cache. ok is false when no result ID is known (or the URI is out of bounds).
func (r *routingInvProxy) PullResultID(uri string) (string, bool) {
	if err := r.checkURI(uri); err != nil {
		return "", false
	}
	inv := r.owningInv(uri)
	if inv == nil {
		return "", false
	}
	return inv.PullResultID(uri)
}

// RecordPullUnchanged records an "unchanged" pull report against the owning
// workspace's cache. Returns false — mutating nothing — when the result ID is
// unknown there (the caller must re-pull without a previousResultId) or the
// URI cannot be routed.
func (r *routingInvProxy) RecordPullUnchanged(uri, resultID string) bool {
	if err := r.checkURI(uri); err != nil {
		return false
	}
	inv := r.owningInv(uri)
	if inv == nil {
		return false
	}
	return inv.RecordPullUnchanged(uri, resultID)
}

// RecordPullResult ingests a full/unchanged pull report (and its related
// documents) into the owning workspace's cache. Related documents come from
// the same language server as the primary URI, so one routing decision covers
// the whole report. A no-op when the URI cannot be routed.
func (r *routingInvProxy) RecordPullResult(uri string, report protocol.DocumentDiagnosticReport) {
	if err := r.checkURI(uri); err != nil {
		return
	}
	if inv := r.owningInv(uri); inv != nil {
		inv.RecordPullResult(uri, report)
	}
}

// AllPullResultIDs returns the primary workspace's recorded (URI, result ID)
// pairs, sorted by URI — the previousResultIds payload for a workspace pull.
// Primary-scoped by design: workspace/diagnostic is a workspace-wide request,
// and the no-URI tool path (like every URI-less query) targets the primary. A
// workspace pull routed to a secondary server simply sends no prior IDs and
// receives full reports.
func (r *routingInvProxy) AllPullResultIDs() []protocol.PreviousResultID {
	r.mu.RLock()
	primary := r.primary
	r.mu.RUnlock()
	if primary == nil {
		return []protocol.PreviousResultID{}
	}
	return primary.AllPullResultIDs()
}
