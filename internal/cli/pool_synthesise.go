package cli

// pool_synthesise.go — the synthetic workspace root: the last-resort fallback
// when Detect finds no marker. Split out of pool_detect.go when that file hit
// the size cap; the home-directory identity machinery it leans on lives in
// pool_homeguard.go.

import (
	"os"
	"path/filepath"

	"github.com/plumbkit/plumb/internal/paths"
)

// SynthesiseRoot returns a synthetic workspace root for seedDir, used as a
// last resort when Detect has already failed. It walks up from seedDir
// looking for a .git directory (the conventional project-root signal for
// unrecognised languages). If found, that directory is returned. If the
// filesystem root is reached without finding .git, seedDir itself is
// returned as the safest approximation.
//
// SynthesiseRoot must only be called on the Detect error path in
// OnBeforeTool — never inside route() or LSP-routing paths.
//
// Like Detect, the result is canonicalised (issue #263): a markerless root is
// still a root, and issue #182's contract is explicit that an explicit pin must
// not behave differently for want of a marker.
//
// The home directory is special-cased three ways, compared by filesystem
// identity (not string) so a symlinked or firmlinked spelling cannot slip past:
//
//   - The walk never ascends INTO $HOME. A dotfiles repo checked out at the
//     home directory is common, and without the guard every markerless
//     directory beneath it synthesised to $HOME — making the whole home
//     directory a single read-write root for the session, with every SSH key,
//     browser profile and credential file under it inside the boundary.
//   - Reaching $HOME STOPS the walk (fall back to the seed). Merely skipping
//     the .git check there and continuing upward meant a .git ABOVE the home
//     directory synthesised $HOME's PARENT — a root strictly WIDER than the
//     escape the guard exists to block.
//   - A seed that IS $HOME is honoured only when the caller marks it
//     explicit. explicit means the seed is a workspace the caller genuinely
//     declared — a session_start workspace arg, live or replayed (issue #182:
//     an explicit pin must always succeed, and pointing plumb at your home
//     directory is a declaration of intent). An incidental seed — a tool-path
//     argument, a client-reported root, a persisted pin that never came from
//     session_start — is NOT a declaration: one read_file of ~/.zshrc must
//     not pin the entire home directory. For those callers a $HOME seed
//     returns "" (refused), and the session stays unattached.
func (p *workspacePool) SynthesiseRoot(seedDir string, explicit bool) string {
	// Clean the seed before anything else. A synthesised root is stored as the
	// session's workspace, and several tools boundary-check that string directly
	// rather than a path derived from it, so a root carrying an unresolved ".."
	// is refused by the policy for the life of the session — with a message
	// telling the caller to pass a different path, which it cannot, because it
	// never named one. Detect cleans its own input; only this fallback did not.
	//
	// Cleaning here is deliberately the opposite of what PathPolicy.Check does to
	// a path ARGUMENT, which refuses rather than resolves. The asymmetry is the
	// point: an argument is a claim about a file inside a boundary that already
	// exists, so guessing which of two readings the caller meant could retarget a
	// write. A root is not inside anything — it DEFINES the boundary — so there
	// is no second file for it to be confused with, and Detect has always Cleaned
	// its own return for the same reason.
	homeInfo := homeDirInfos()
	homePaths := homeDirPaths()
	d := filepath.Clean(seedDir)
	// A seed AT or ABOVE a home directory is refused unless the caller declared
	// it — and refusing is done by RETURNING "", so every caller inherits it.
	//
	// This was previously a check bolted onto one of the three callers, and
	// review found the other two still pinning the wide root: the first-tool-call
	// seeding path (find_files/search_in_files take a DIRECTORY as `path`, so a
	// call naming /Users seeds it directly) and rehydratePin replaying a
	// roots-origin row, which runs unconditionally in the default configuration.
	// Stopping the WALK cannot fix that, because the walk's fallback returns the
	// seed — and here the seed is the offending directory.
	//
	// Containment covers "/" for free, which the reordering below does not: a
	// find_files({path: "/"}) seed reaches the containment branch first.
	if sameDirAs(d, homeInfo) || containsHomeDir(d, homePaths) {
		if !explicit {
			return ""
		}
		return paths.Canonical(d)
	}
	for {
		if sameDirAs(d, homeInfo) || containsHomeDir(d, homePaths) {
			// Reached a home directory, or one that CONTAINS it, ascending: stop
			// and fall back to the seed. Never consult the .git there, never walk
			// above it — a root at or above a home directory can only ever be
			// wider than the boundary this guard exists to keep.
			//
			// Falling back to the seed is safe HERE, unlike at the top: the seed
			// reached this loop only by passing the refusal above, so it is
			// already known not to be at or above a home directory.
			return paths.Canonical(filepath.Clean(seedDir))
		}
		parent := filepath.Dir(d)
		if parent == d {
			// At the filesystem root, having found no marker: fall back to the
			// seed. The .git check is deliberately below this test, matching
			// detect(), so a /.git cannot make "/" the workspace — belt to the
			// containment branch's braces, since "/" contains every home
			// directory and is refused above on any machine where one is
			// determinable at all.
			return paths.Canonical(filepath.Clean(seedDir))
		}
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return paths.Canonical(d)
		}
		d = parent
	}
}
