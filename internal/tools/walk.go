package tools

// walk.go provides the shared filesystem traversal infrastructure used by
// search_in_files and find_files: gitignore-aware directory walking, binary
// file detection, and hidden-file filtering.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/plumbkit/plumb/internal/ignore"
	"github.com/plumbkit/plumb/internal/textfmt"
)

// ── binary detection ─────────────────────────────────────────────────────────

// binarySniffBytes is the prefix size used to detect binary files via a null
// byte, matching the heuristic ripgrep and git use.
const binarySniffBytes = 8000

// ── glob helpers ─────────────────────────────────────────────────────────────

// globLiteralPrefix returns the longest leading slash-delimited segment of
// glob that contains no wildcard metacharacters. Used for directory-level
// pruning: a glob like "src/**/*.go" can never match files outside "src/".
//
// Brace alternation counts as a metacharacter: "pkg/{a,b}/x.go" may match under
// either branch, so the prefix must stop at "pkg". Omitting `{` here pruned away
// the very directories a braced glob was meant to reach.
func globLiteralPrefix(glob string) string {
	if glob == "" {
		return ""
	}
	parts := strings.Split(filepath.ToSlash(glob), "/")
	var lit []string
	for _, p := range parts {
		if strings.ContainsAny(p, "*?[{}") {
			break
		}
		lit = append(lit, p)
	}
	return strings.Join(lit, "/")
}

// dirCompatibleWithPrefix returns true iff a directory at relative path rel
// could contain files whose relative path begins with prefix. That is, rel
// and prefix have an ancestor-or-equal relationship as slash-delimited paths.
func dirCompatibleWithPrefix(rel, prefix string) bool {
	if rel == "" || rel == "." || rel == prefix {
		return true
	}
	return strings.HasPrefix(rel+"/", prefix+"/") ||
		strings.HasPrefix(prefix+"/", rel+"/")
}

// ── hidden file detection ────────────────────────────────────────────────────

func isHidden(name string) bool {
	return strings.HasPrefix(name, ".")
}

// ── walker ───────────────────────────────────────────────────────────────────

// gitDirName is the one directory the shared walk NEVER enters, whatever the
// hidden-file policy says. include_hidden means "show me dotfiles", not "show
// me the object database": a repository's .git/ is thousands of loose objects
// and packfiles that no caller of find_files, search_in_files, or find_replace
// has ever wanted — and find_replace rewriting inside it would corrupt the
// repository. Both search tools already advertise "no .git/" in their
// descriptions; this is what makes that true rather than an accident of .git
// happening to start with a dot.
const gitDirName = ".git"

// walkOptions controls the filesystem traversal shared by both tools.
type walkOptions struct {
	root          string
	maxDepth      int  // 0 = unlimited; N visits entries at depths 0..N-1
	includeHidden bool // include dot-files/dirs
	respectIgnore bool // honour .gitignore / .ignore

	// boundary is the caller's per-connection path guard, consulted for every
	// SYMLINK the walk meets. Callers pass the guard their tool holds, so the
	// access level follows the tool: a read guard for the search tools, a write
	// guard for find_replace. nil falls back to the walk root — see
	// escapesBoundary.
	boundary BoundaryGuard

	// onWithheld, when set, is called with the absolute path of each entry the
	// boundary check withheld, so the tool can report the omission instead of
	// silently under-reporting.
	onWithheld func(absPath string)
}

// walkFn is called for each non-ignored, non-hidden file.
// Returning fs.SkipDir skips the directory (only valid when d.IsDir()).
type walkFn func(path string, d fs.DirEntry, depth int) error

// walk traverses root respecting gitignore rules, hidden-file policy, and
// depth limit. It visits directories before their contents (pre-order) so the
// callback can return fs.SkipDir to prune. The walk aborts as soon as ctx is
// cancelled; pass context.Background() for callers that don't need cancellation.
func walk(ctx context.Context, opts walkOptions, fn walkFn) error {
	if ctx == nil {
		ctx = context.Background()
	}
	var st ignore.Stack
	if opts.respectIgnore {
		st = st.Load(opts.root)
	}
	return walkDir(ctx, opts.root, 0, st, opts, fn)
}

// escapesBoundary reports whether an entry must be withheld from the walk
// because it is a symlink resolving outside what the caller may read.
//
// Only a symlink can escape: every other entry was reached by descending from a
// root the tool boundary-checked before the walk began. Git stores symlinks
// natively, so a hostile repository can commit one pointing at an absolute path
// (`innocent.env -> /home/you/.ssh/id_rsa`) and have a mere clone plant it —
// which is why the check belongs HERE, in the shared traversal, rather than in
// each tool that consumes it.
//
// The link type comes from the os.ReadDir entry, so a tree with no symlinks
// pays no extra syscall. That same fact is why the walk never descends THROUGH
// a link: ReadDir reports a symlink as a symlink, never as a directory, so a
// link to an outside directory is withheld here as one entry instead of
// becoming a whole subtree. A dangling link resolves to nothing outside the
// workspace, so it is not withheld — it is simply unreadable further down.
//
// With a guard the decision is the connection's whole path policy, so a link
// into a legitimately readable non-workspace root still resolves. Without one
// the walk falls back to its own root and fails closed: no daemon-wired tool
// passes a nil guard, but a future caller that forgets to should under-report,
// never over-disclose.
func escapesBoundary(ctx context.Context, absPath string, d fs.DirEntry, opts walkOptions) bool {
	if d.Type()&fs.ModeSymlink == 0 {
		return false
	}
	if opts.boundary != nil {
		return opts.boundary.check(ctx, absPath) != nil
	}
	return !PathWithinWorkspace(opts.root, absPath)
}

// maxWithheldNamed bounds how many withheld paths an advisory names, so a tree
// full of escaping links cannot swamp the result it is annotating.
const maxWithheldNamed = 5

// withheldSymlinkNote renders the advisory for entries the boundary check
// withheld. Skipping them silently would be its own hazard: an agent reads a
// clean "no matches" as proof of absence, so the tool says what it did not look
// at. Only the in-workspace link paths are named — naming their targets would
// disclose the very out-of-workspace paths being withheld.
func withheldSymlinkNote(rels []string) string {
	if len(rels) == 0 {
		return ""
	}
	sort.Strings(rels)
	named := rels
	var more string
	if len(named) > maxWithheldNamed {
		named = named[:maxWithheldNamed]
		more = fmt.Sprintf(", +%d more", len(rels)-maxWithheldNamed)
	}
	return fmt.Sprintf("\n\nNote: %d %s withheld — %s resolving outside the workspace boundary: %s%s.",
		len(rels), textfmt.Plural(len(rels), "entry", "entries"),
		textfmt.Plural(len(rels), "a symlink", "symlinks"),
		strings.Join(named, ", "), more)
}

// shouldVisitEntry reports whether an entry passes the .git, hidden-file, and
// gitignore filters.
func shouldVisitEntry(name, absPath string, isDir bool, opts walkOptions, st ignore.Stack) bool {
	if isDir && name == gitDirName {
		return false // unconditional — see gitDirName
	}
	if !opts.includeHidden && isHidden(name) {
		return false
	}
	if opts.respectIgnore && st.IsIgnored(absPath, isDir) {
		return false
	}
	return true
}

func walkDir(ctx context.Context, dir string, depth int, st ignore.Stack, opts walkOptions, fn walkFn) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil // unreadable directory — skip silently
	}

	// Load ignore rules for this directory (already loaded for root above).
	if depth > 0 && opts.respectIgnore {
		st = st.Load(dir)
	}

	for _, d := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := d.Name()
		absPath := filepath.Join(dir, name)

		if !shouldVisitEntry(name, absPath, d.IsDir(), opts, st) {
			continue
		}
		if escapesBoundary(ctx, absPath, d, opts) {
			if opts.onWithheld != nil {
				opts.onWithheld(absPath)
			}
			continue
		}

		if d.IsDir() {
			if err := walkSubdir(ctx, absPath, d, depth, st, opts, fn); err != nil {
				return err
			}
			continue
		}
		if err := fn(absPath, d, depth); err != nil {
			return err
		}
	}
	return nil
}

// walkSubdir visits one directory entry and descends into it unless the depth
// limit or the callback says otherwise. Split out of walkDir's loop to keep
// that loop flat.
//
// A non-SkipDir error from fn on a DIRECTORY is deliberately discarded, as it
// always has been: pruning is the only signal a directory visit is allowed to
// send, and a callback that reports a real problem does so from the file visit.
func walkSubdir(ctx context.Context, absPath string, d fs.DirEntry, depth int, st ignore.Stack, opts walkOptions, fn walkFn) error {
	if pastDepthLimit(opts, depth) {
		return nil // the directory itself sits at or past the limit
	}
	if err := fn(absPath, d, depth); errors.Is(err, fs.SkipDir) {
		return nil
	}
	// Prune STRICTLY: this directory's children sit at depth+1, so once that is
	// past the limit there is nothing inside worth reading. The old check
	// descended one level too far and then discarded everything it found — a
	// ReadDir plus a .gitignore load per top-level directory for a max_depth=1
	// listing.
	if pastDepthLimit(opts, depth+1) {
		return nil
	}
	return walkDir(ctx, absPath, depth+1, st, opts, fn)
}

// pastDepthLimit reports whether an entry at this depth is outside the walk's
// maxDepth. maxDepth 0 means unlimited, so nothing is ever past it.
func pastDepthLimit(opts walkOptions, depth int) bool {
	return opts.maxDepth > 0 && depth >= opts.maxDepth
}
