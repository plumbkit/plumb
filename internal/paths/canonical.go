package paths

import (
	"path/filepath"
	"strings"
)

// Canonical resolves p to its symlink-free spelling, so two references to one
// directory compare equal regardless of how the caller reached it. It is the
// answer to "are these the same place?" — the macOS /tmp → /private/tmp
// firmlink, a checkout under a symlinked parent, a $TMPDIR alias.
//
// Three behaviours, in the order they apply:
//
//   - A RELATIVE path is cleaned and returned untouched, with no filesystem
//     access. Resolving it would anchor it to the process working directory,
//     which for the singleton daemon belongs to whichever client happened to
//     spawn it — the silent cross-repository write of issue #181.
//     Canonicalisation must never invent a location.
//   - An existing absolute path is resolved with filepath.EvalSymlinks.
//   - A NOT-YET-EXISTING absolute path resolves its nearest existing ancestor
//     and re-joins the missing tail, so a directory about to be created under a
//     symlinked parent already canonicalises to where it will really live.
//
// The absolute path is handed to EvalSymlinks UNCLEANED. filepath.Clean
// collapses ".." lexically, which diverges from the kernel's left-to-right
// resolution as soon as a ".." follows a symlink: cleaning first would let two
// paths naming DIFFERENT directories canonicalise to the same string, turning a
// same-place test into a false positive. EvalSymlinks resolves components in
// kernel order, so it is the only step allowed to remove a "..".
//
// When nothing resolves — an unreadable parent, or a path with no resolvable
// ancestor at all — it degrades to filepath.Clean rather than failing, leaving
// the caller exactly the un-canonicalised spelling it already had. A workspace
// that cannot be canonicalised is still a usable workspace; refusing one would
// turn an unreadable parent directory into a dead session.
//
// One residual imprecision, stated rather than papered over: in the MISSING-path
// branch the ".." has already been collapsed lexically, since there is no real
// chain to resolve it against. If the component before that ".." is a symlink
// pointing under a different parent, the result names the lexical parent rather
// than the kernel's. Skipping the ancestor walk for such paths was measured and
// recovers nothing — the lexical collapse has already happened, so both
// spellings produce the same string — while costing canonicalisation for the
// ordinary case of an aliased parent. Callers that need a path to be SAFE, not
// merely comparable, must still run it past the boundary policy, which refuses
// an unresolved ".." outright; Canonical is an identity function, not an
// authorisation check.
//
// It normalises symlinks only, and it must stay that way: the result is a PATH
// that callers write to (safeWrite creates files under it), so it has to name a
// real file on the volume it came from. Two spellings differing solely in case
// therefore still compare unequal here even where the filesystem folds them
// into one file. That question belongs to CanonicalKey, whose result is an
// opaque identity key rather than a path and may be lowercased freely.
func Canonical(p string) string {
	if p == "" {
		return ""
	}
	if !filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return canonicalMissing(p)
}

// WorkspaceRel expresses absPath relative to workspace, and reports whether it
// lies inside. Use it wherever a CANONICAL workspace root is compared against a
// path that came from somewhere else — an agent argument, a recorded stats row,
// a client-reported URI. (Callers whose two sides share an origin, like a walk
// rooted at the workspace or an LSP path against the root that server was
// initialised with, do not need it and still use filepath.Rel.)
//
// The two arguments routinely come from different places: workspace is the
// canonical root the pool resolved, absPath is whatever spelling the agent or
// client named. A plain filepath.Rel of those two reports a file sitting in the
// project as an escape the moment the project is reachable two ways — the macOS
// /tmp → /private/tmp firmlink, a checkout under a symlinked parent (issue #263).
// Silently, since "outside the workspace" is a legitimate answer these callers
// simply drop.
//
// So a lexical mismatch is not trusted on its own: when the raw spellings
// disagree, both sides are canonicalised and compared again, because only the
// filesystem can tell a real escape from a second spelling. The lexical pass runs
// first and answers the overwhelmingly common case with no filesystem access at
// all, which matters because this sits on the per-tool-call enrich path.
//
// absPath must be absolute; a caller holding a workspace-relative path already
// has its answer. Only a genuine escape is rejected — exactly ".." or a "../"
// prefix — so an in-workspace directory named "..config" still resolves. The
// returned path is slash-separated.
//
// This is a naming question, not a containment check. Because the lexical pass
// answers first, a path that lies inside the workspace lexically but escapes it
// through a symlink is reported as inside — matching what these callers did
// before, and harmless for them, since they are choosing how to DISPLAY or MATCH
// a path rather than deciding whether it may be touched. Anything deciding
// access must go through the boundary policy, which resolves unconditionally.
func WorkspaceRel(workspace, absPath string) (rel string, inside bool) {
	if workspace == "" || absPath == "" {
		return "", false
	}
	if rel, ok := relWithin(workspace, absPath); ok {
		return rel, true
	}
	return relWithin(Canonical(workspace), Canonical(absPath))
}

// relWithin is WorkspaceRel's comparison against one pair of spellings.
func relWithin(base, path string) (string, bool) {
	rel, err := filepath.Rel(base, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// canonicalMissing canonicalises an absolute path EvalSymlinks could not
// resolve, almost always because it does not exist yet. It walks up to the
// nearest ancestor that does resolve and re-joins the tail below it.
func canonicalMissing(p string) string {
	clean := filepath.Clean(p)
	dir := clean
	var tail []string
	for {
		parent := filepath.Dir(dir)
		if parent == dir {
			return clean // walked to the filesystem root resolving nothing
		}
		tail = append(tail, filepath.Base(dir))
		dir = parent
		resolved, err := filepath.EvalSymlinks(dir)
		if err != nil {
			continue
		}
		for i := len(tail) - 1; i >= 0; i-- {
			resolved = filepath.Join(resolved, tail[i])
		}
		return resolved
	}
}
