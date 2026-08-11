package paths

import "path/filepath"

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
// It normalises symlinks only. Two spellings that differ solely in case still
// compare unequal on a case-insensitive filesystem, because resolving that
// would need a per-component directory read and would be wrong on the
// case-sensitive filesystems plumb also runs on.
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
