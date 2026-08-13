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
	d := filepath.Clean(seedDir)
	if sameDirAs(d, homeInfo) {
		if !explicit {
			return ""
		}
		return paths.Canonical(d)
	}
	for {
		if sameDirAs(d, homeInfo) || containsHomeDir(d, homeInfo) {
			// Reached $HOME, or a directory that CONTAINS it, ascending: stop.
			// Never consult the .git there, never walk above it — a root at or
			// above the home directory can only ever be wider than the boundary
			// this guard exists to keep. Testing containment as well as identity
			// is what closes the rung above: refusing $HOME while accepting
			// /Users (or /home, or /) admits a workspace holding every home
			// directory on the machine, which review found reachable through a
			// client reporting such a folder in its roots.
			return paths.Canonical(filepath.Clean(seedDir))
		}
		parent := filepath.Dir(d)
		if parent == d {
			// At the filesystem root. Fall back to the seed WITHOUT consulting
			// /.git: returning "/" would be a workspace containing every home
			// directory, the very thing the guard above refuses one rung lower.
			// detect() has always had this ordering (its .git check is behind a
			// d != filepath.Dir(d) test); this walk did not, and the asymmetry
			// was reachable rather than theoretical.
			return paths.Canonical(filepath.Clean(seedDir))
		}
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return paths.Canonical(d)
		}
		d = parent
	}
}
