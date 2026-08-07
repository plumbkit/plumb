package tools

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/collab"
	"github.com/plumbkit/plumb/internal/textfmt"
)

// git_intent_warn.go — repo-level peer-intent warnings on git state verbs.
// share_intent's claims are path-glob based, so a peer whose intent covers the
// whole repository ("rebasing ops main") is invisible to another session about
// to run `git switch`/`rebase`/`reset`. Before a repo-state verb runs, live
// peer intents covering the repository are surfaced in the response as an
// advisory warning naming the peer and the claim. Surfacing only — it never
// blocks the op and never demands confirm (the ref-movement guard in
// git_ref_guard.go already owns confirmed friction); it reuses the collab
// intent store and glob matching, adding no storage of its own.

const (
	// peerIntentQueryTimeout bounds the collab.db read so a slow disk never
	// stalls a git op for the sake of an advisory warning. The check runs
	// while the per-repo serialisation lock is held, so it must stay cheap.
	peerIntentQueryTimeout = 200 * time.Millisecond
	// maxPeerIntentWarnings caps how many matching claims the warning quotes;
	// the remainder collapses into a count line.
	maxPeerIntentWarnings = 3
	// maxPeerIntentBodyRunes bounds each quoted intent body — bodies are
	// agent-authored free text, unbounded at write time.
	maxPeerIntentBodyRunes = 160
)

// repoStateVerb reports whether a git op changes repository state visible to
// other sessions — moving HEAD, rewriting refs, or discarding working-tree
// content — the ops a peer's in-progress work (a rebase, a checkout, a reset)
// can collide with. Every destructive-tier op qualifies; among write-tier ops
// only the HEAD movers do (commit, switch, checkout -b/-B). Index-only writes
// (add, restore --staged, stash push) and purely additive ones (branch/tag
// create, mv) are excluded: they cannot disturb a peer's in-flight repo
// operation, so warning there would be noise. Read and network tiers never
// warn.
func repoStateVerb(sub string, tier gitTier) bool {
	if tier == tierDestructive {
		return true
	}
	return tier == tierWrite && (sub == "commit" || sub == "switch" || sub == "checkout")
}

// peerIntentWarnFn returns the repo-intent check for this call, or nil — the
// zero-cost path — when the call is not a repo-state verb, the warning is not
// wired, [collab] intents is off, or the connection has no session identity
// (without one every intent would look like a peer's).
func (t *Git) peerIntentWarnFn(sub string, tier gitTier) func(context.Context, string) string {
	if !repoStateVerb(sub, tier) || t.intentsOn == nil || t.collabStore == nil || t.sessID == "" {
		return nil
	}
	if !t.intentsOn() {
		return nil
	}
	return t.peerRepoIntentWarning
}

// peerRepoIntentWarning renders the advisory warning for live peer intents
// covering the repository rooted at repoRoot. Best-effort: any query failure
// yields no warning — this is surfacing, never a gate.
func (t *Git) peerRepoIntentWarning(ctx context.Context, repoRoot string) string {
	store := t.collabStore()
	if store == nil {
		return ""
	}
	ws := ""
	if t.deps.WorkspaceFn != nil {
		ws = t.deps.WorkspaceFn()
	}
	qctx, cancel := context.WithTimeout(ctx, peerIntentQueryTimeout)
	defer cancel()
	now := time.Now()
	intents, err := store.LiveIntents(qctx, now)
	if err != nil {
		return ""
	}
	return formatRepoIntentWarning(intents, ws, repoRoot, t.sessID, now)
}

// formatRepoIntentWarning renders the warning block for the matching claims.
// The session's own intent and expired rows (defensive — LiveIntents already
// filters them) never warn.
func formatRepoIntentWarning(intents []collab.Row, ws, repoRoot, selfID string, now time.Time) string {
	var matched []collab.Row
	for _, r := range intents {
		if r.AuthorID == selfID || !r.ExpiresAt.After(now) {
			continue
		}
		if intentCoversRepo(r.PathGlobs, ws, repoRoot) {
			matched = append(matched, r)
		}
	}
	if len(matched) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("# plumb-warning: peer intent claims cover this repository (advisory, unverified — not a lock):\n")
	for i, r := range matched {
		if i == maxPeerIntentWarnings {
			fmt.Fprintf(&sb, "#   … and %d more peer intent claim(s)\n", len(matched)-maxPeerIntentWarnings)
			break
		}
		fmt.Fprintf(&sb, "#   peer %s claimed: %q (expires in %s)\n",
			r.AuthorSession, textfmt.Ellipsis(r.Body, maxPeerIntentBodyRunes), humaniseTTL(r.ExpiresAt.Sub(now)))
	}
	return sb.String()
}

// intentCoversRepo reports whether a peer intent's claim reaches the
// repository rooted at repoRoot. An UNSCOPED intent (no path globs) is a
// whole-workspace broadcast — "rebasing ops main" — and always covers it. A
// scoped intent covers the repo when the repo IS the workspace root (every
// workspace-relative glob names paths inside it) or when a glob matches the
// repo root's workspace-relative path (e.g. "plumb/**" for a nested repo).
// Globs are workspace-relative, so a repo outside the workspace can only be
// covered by an unscoped broadcast.
func intentCoversRepo(globs []string, ws, repoRoot string) bool {
	if len(globs) == 0 {
		return true
	}
	// Canonicalise both sides: repoRoot comes from `git rev-parse
	// --show-toplevel`, which resolves symlinks the pinned workspace path may
	// keep (macOS /var vs /private/var), so a naive Rel would escape with "..".
	if resolved, err := canonicalPathForBoundary(repoRoot); err == nil {
		repoRoot = resolved
	}
	if resolved, err := canonicalPathForBoundary(ws); err == nil {
		ws = resolved
	}
	rel, err := filepath.Rel(ws, repoRoot)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	if rel == "." {
		return true
	}
	return collab.MatchPath(globs, rel)
}
