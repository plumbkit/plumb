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
	if !repoStateVerb(sub, tier) || t.intentsOn == nil || t.collabStore == nil || t.sessID == nil || t.sessID() == "" {
		return nil
	}
	if !t.intentsOn() {
		return nil
	}
	return func(ctx context.Context, repoRoot string) string {
		return t.peerRepoIntentWarning(ctx, repoRoot, tier)
	}
}

// intentWarningBudget returns t.hintBudgetBytes(), or 0 (unbounded, matching
// textfmt.ClampBytes' convention) when the connection never wired it — a test
// double that only sets intentsOn/collabStore, say.
func (t *Git) intentWarningBudget() int {
	if t.hintBudgetBytes == nil {
		return 0
	}
	return t.hintBudgetBytes()
}

// peerRepoIntentWarning renders the advisory warning for live peer intents
// covering the repository rooted at repoRoot, tier-aware (see
// intentCoversRepo). Best-effort: any query failure yields no warning — this
// is surfacing, never a gate.
func (t *Git) peerRepoIntentWarning(ctx context.Context, repoRoot string, tier gitTier) string {
	store := t.collabStore()
	if store == nil {
		return ""
	}
	ws := ""
	if t.deps.WorkspaceFn != nil {
		ws = t.deps.WorkspaceFn(ctx)
	}
	qctx, cancel := context.WithTimeout(ctx, peerIntentQueryTimeout)
	defer cancel()
	now := time.Now()
	intents, err := store.LiveIntents(qctx, now)
	if err != nil {
		return ""
	}
	sessID := ""
	if t.sessID != nil {
		sessID = t.sessID()
	}
	return formatRepoIntentWarning(intents, ws, repoRoot, sessID, now, tier, t.intentWarningBudget())
}

// formatRepoIntentWarning renders the warning block for the matching claims,
// clamped to budgetBytes (the [collab] hint_budget_bytes snapshot, threaded
// through WithPeerIntents) on a UTF-8 boundary — the same budget and
// convention every other injected peer-signal block uses (see
// internal/cli/conn_peer.go's peerHint and conn_collab.go's intentHint). The
// session's own intent and expired rows (defensive — LiveIntents already
// filters them) never warn.
func formatRepoIntentWarning(intents []collab.Row, ws, repoRoot, selfID string, now time.Time, tier gitTier, budgetBytes int) string {
	var matched []collab.Row
	for _, r := range intents {
		if r.AuthorID == selfID || !r.ExpiresAt.After(now) {
			continue
		}
		if intentCoversRepo(r.PathGlobs, ws, repoRoot, tier) {
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
	return textfmt.ClampBytes(sb.String(), budgetBytes)
}

// intentCoversRepo reports whether a peer intent's claim reaches the
// repository rooted at repoRoot, TIER-AWARE. An UNSCOPED intent (no path
// globs) is a whole-workspace broadcast — "rebasing ops main" — and always
// covers it regardless of tier.
//
// For a SCOPED intent the required breadth depends on the op's blast radius:
// a destructive-tier op (reset, clean, rebase, checkout, branch/tag delete,
// stash drop, …) can touch any path in the repository, so any live peer
// intent whose scope reaches into the repository counts. A write-tier
// repo-state op (commit, switch, checkout -b) only moves HEAD/refs and writes
// what this session explicitly asked for, so it warns only for a genuinely
// repo-wide claim — never a narrow subtree that merely happens to sit inside
// the repository, or every commit in the common single-repo layout (repo ==
// workspace) would warn on any live intent no matter how narrowly scoped.
//
// Three repo/workspace layouts:
//   - repo IS the workspace (rel == "."): every workspace-relative glob names
//     a path inside the repo, so destructive always covers it; write-tier
//     covers only for a glob that spans the whole workspace ("**", "*", ".") —
//     collab.MatchPath already treats those as matching rel "." and a
//     narrower glob as not, so no extra logic is needed here.
//   - the workspace sits INSIDE the repo (rel is a pure ".." chain — a
//     pinned subdirectory workspace under a larger git top-level): identical
//     reasoning applies, since every workspace-relative path is still inside
//     the (larger) repository, so it is treated the same as rel == ".".
//   - the repo is nested INSIDE the workspace (rel names a subpath, e.g.
//     "plumb" for repoRoot <ws>/plumb): a write-tier repo-state op warns only
//     for a genuinely repo-wide claim (a glob matching rel itself, e.g.
//     "plumb/**"), while a destructive op warns for any glob whose scope
//     reaches into the repo subtree (e.g. "plumb/internal/**") — the same tier
//     split as the two layouts above.
//
// A repo outside the workspace tree entirely (neither of the above) can only
// be covered by an unscoped broadcast, since a workspace-relative glob cannot
// name anything in it.
func intentCoversRepo(globs []string, ws, repoRoot string, tier gitTier) bool {
	if len(globs) == 0 {
		return true
	}
	// Canonicalise both sides: repoRoot comes from `git rev-parse
	// --show-toplevel`, which resolves symlinks the pinned workspace path may
	// keep (macOS /var vs /private/var), so a naive Rel would escape with "..".
	repoRoot = canonicalRoot(repoRoot)
	ws = canonicalRoot(ws)
	rel, err := filepath.Rel(ws, repoRoot)
	if err != nil {
		return false
	}
	if rel == "." || pureAncestorRel(rel) {
		if tier == tierDestructive {
			return true
		}
		return collab.MatchPath(globs, ".")
	}
	if strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false // repoRoot sits outside the workspace tree entirely
	}
	// Repo nested under the workspace at rel: a write-tier repo-state op still
	// needs a genuinely repo-wide claim (a glob matching rel itself), but a
	// destructive op can touch any path under rel, so any glob whose scope
	// reaches into the repo subtree counts — matching the two layouts above.
	if tier == tierDestructive {
		return globReachesRepo(globs, rel)
	}
	return collab.MatchPath(globs, rel)
}

// globReachesRepo reports whether any glob's match set intersects the directory
// subtree rooted at rel — the coverage question for a destructive-tier op in a
// repo nested under the workspace (rel is the repo's workspace-relative path).
// It is deliberately conservative: a glob whose scope cannot be pinned outside
// rel warns rather than under-warns, because a destructive op can touch any
// path under the repository root.
func globReachesRepo(globs []string, rel string) bool {
	rel = filepath.ToSlash(rel)
	for _, g := range globs {
		if globReachesDir(strings.TrimSpace(g), rel) {
			return true
		}
	}
	return false
}

// globReachesDir reports whether a single glob can match some path at or under
// rel. It mirrors matchGlob's three shapes (a "dir/**" prefix, a slashless
// basename, and a full relative path) but answers intersection with a SUBTREE
// rather than with a single path.
func globReachesDir(g, rel string) bool {
	if g == "" {
		return false
	}
	// "dir/**" matches dir and everything under it: the two subtrees intersect
	// iff one root sits at-or-under the other.
	if strings.HasSuffix(g, "/**") {
		head := strings.TrimSuffix(g, "/**")
		return head == rel || strings.HasPrefix(head, rel+"/") || strings.HasPrefix(rel, head+"/")
	}
	// A slashless glob matches a basename anywhere in the tree, so it can name
	// a path under rel; the destructive tier treats an unknowable scope as
	// reaching in.
	if !strings.Contains(g, "/") {
		return true
	}
	// A slashed glob matches full relative paths. It reaches rel when it
	// matches rel itself, or when its directory prefix sits at-or-under rel (or
	// rel sits under that prefix).
	if collab.MatchPath([]string{g}, rel) {
		return true
	}
	i := strings.LastIndexByte(g, '/')
	dir := g[:i]
	return dir == rel || strings.HasPrefix(dir, rel+"/") || strings.HasPrefix(rel, dir+"/")
}

// pureAncestorRel reports whether rel — a filepath.Rel(ws, repoRoot) result —
// is a PURE ".." chain: every path component is "..", meaning repoRoot is an
// ancestor of ws with no additional path segment naming some other, unrelated
// location. A mixed result ("../sibling") means repoRoot sits outside the
// workspace tree entirely, not above it.
func pureAncestorRel(rel string) bool {
	for part := range strings.SplitSeq(rel, string(filepath.Separator)) {
		if part != ".." {
			return false
		}
	}
	return true
}
