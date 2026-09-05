package tools

// walk.go provides the shared filesystem traversal infrastructure used by
// search_in_files and find_files: gitignore-aware directory walking, binary
// file detection, and hidden-file filtering.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/plumbkit/plumb/internal/textfmt"
)

// ── gitignore ────────────────────────────────────────────────────────────────

// ignorePattern is one compiled line from a .gitignore / .ignore file.
type ignorePattern struct {
	negate   bool   // line starts with !
	dirOnly  bool   // line ends with /
	rooted   bool   // line starts with / (after negation strip)
	hasSlash bool   // line contains / (match against path, not just name)
	glob     string // the cleaned glob to match
}

// parseIgnoreLine parses one non-blank, non-comment gitignore line.
func parseIgnoreLine(raw string) (ignorePattern, bool) {
	line := strings.TrimRight(raw, " \t") // trailing whitespace is ignored
	if line == "" || strings.HasPrefix(line, "#") {
		return ignorePattern{}, false
	}

	p := ignorePattern{}
	if strings.HasPrefix(line, "!") {
		p.negate = true
		line = line[1:]
	}
	if strings.HasSuffix(line, "/") {
		p.dirOnly = true
		line = strings.TrimSuffix(line, "/")
	}
	if strings.HasPrefix(line, "/") {
		p.rooted = true
		line = line[1:]
	}
	p.hasSlash = strings.Contains(line, "/")
	p.glob = line
	return p, true
}

// matchesPath reports whether the pattern matches relPath (slash-separated,
// relative to the directory that owns this pattern set). isDir is true when
// the entry is a directory.
func (p ignorePattern) matchesPath(relPath string, isDir bool) bool {
	if p.dirOnly && !isDir {
		return false
	}
	if !p.hasSlash && !p.rooted {
		// Match against base name only (unless the pattern contains a slash).
		return doubleStarMatch(p.glob, filepath.Base(relPath))
	}
	return doubleStarMatch(p.glob, relPath)
}

// doubleStarMatch is filepath.Match extended to support the ** wildcard.
// ** matches zero or more path components.
func doubleStarMatch(pattern, name string) bool {
	// Fast path: no doublestar.
	if !strings.Contains(pattern, "**") {
		m, _ := filepath.Match(pattern, name)
		return m
	}

	// Split on **/ segments and match greedily.
	// e.g. "a/**/b" → ["a/", "b"]
	parts := strings.SplitN(pattern, "**/", 2)
	if len(parts) == 1 {
		// Trailing **: "dir/**" matches anything under dir/.
		prefix := strings.TrimSuffix(pattern, "**")
		return strings.HasPrefix(name, prefix)
	}
	left, right := parts[0], parts[1]

	if left == "" {
		// **/right — try matching right against name, or against any suffix.
		if doubleStarMatch(right, name) {
			return true
		}
		// Walk through each directory prefix.
		idx := strings.Index(name, "/")
		for idx >= 0 {
			name = name[idx+1:]
			if doubleStarMatch(right, name) {
				return true
			}
			idx = strings.Index(name, "/")
		}
		return false
	}

	// left/**/right — name must start with left, then ** matches mid, then right.
	leftGlob := strings.TrimSuffix(left, "/")
	// Match the prefix portion.
	if !strings.HasPrefix(name, leftGlob+"/") {
		m, _ := filepath.Match(leftGlob, strings.SplitN(name, "/", 2)[0])
		if !m {
			return false
		}
		idx := strings.Index(name, "/")
		if idx < 0 {
			return false
		}
		name = name[idx+1:]
	} else {
		name = name[len(leftGlob)+1:]
	}
	return doubleStarMatch("**/"+right, name)
}

// ignoreSet holds the patterns from one directory's ignore files.
type ignoreSet struct {
	dir      string // absolute directory owning these patterns
	patterns []ignorePattern
}

// ignoreStack accumulates ignore rules as the walker descends directories.
// Rules from parent directories are inherited; child directories can override.
type ignoreStack []*ignoreSet

// load reads .gitignore and .ignore from dir and appends a new set if any
// patterns were found.
func (st *ignoreStack) load(dir string) ignoreStack {
	var patterns []ignorePattern
	for _, name := range []string{".gitignore", ".ignore"} {
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			p, ok := parseIgnoreLine(sc.Text())
			if ok {
				patterns = append(patterns, p)
			}
		}
		// Best-effort like the os.Open failure above: a truncated read still
		// uses whatever patterns were parsed before the scan error.
		_ = sc.Err()
		_ = f.Close()
	}
	if len(patterns) == 0 {
		return *st
	}
	next := make(ignoreStack, len(*st)+1)
	copy(next, *st)
	next[len(*st)] = &ignoreSet{dir: dir, patterns: patterns}
	return next
}

// decide returns the verdict of the LAST rule in the whole stack that matches
// absPath, and whether any rule matched at all. The stack is ordered
// outermost-first, and patterns within a set are already last-match-wins, so
// scanning both in order and keeping the final hit gives deeper rules
// precedence over shallower ones — which is what makes a child's negation able
// to override a parent.
//
// A set only speaks for paths beneath its OWN directory. Without that check a
// deeper set reaches upward: filepath.Rel yields "../outside.log" for a sibling
// above the set, and a pattern with no slash matches on base name alone, so
// sub/.gitignore's `*.log` would hide a file that is not under sub/ at all.
// That could not bite while only the first matching set was consulted during a
// top-down walk; it can now, so it is refused explicitly.
func (st *ignoreStack) decide(absPath string, isDir bool) (ignored, matched bool) {
	for _, s := range *st {
		rel, err := filepath.Rel(s.dir, absPath)
		if err != nil {
			continue
		}
		rel = filepath.ToSlash(rel)
		if rel == ".." || strings.HasPrefix(rel, "../") {
			continue // not beneath this set's directory
		}
		for _, p := range s.patterns {
			if p.matchesPath(rel, isDir) {
				ignored, matched = !p.negate, true
			}
		}
	}
	return ignored, matched
}

// ancestorDirs returns absPath's ancestor directories strictly below root,
// outermost first. Used to enforce gitignore's excluded-parent rule.
func ancestorDirs(root, absPath string) []string {
	rel, err := filepath.Rel(root, absPath)
	if err != nil {
		return nil
	}
	rel = filepath.ToSlash(rel)
	if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return nil
	}
	parts := strings.Split(rel, "/")
	if len(parts) < 2 {
		return nil
	}
	out := make([]string, 0, len(parts)-1)
	cur := root
	for _, p := range parts[:len(parts)-1] {
		cur = filepath.Join(cur, p)
		out = append(out, cur)
	}
	return out
}

// isIgnored reports whether absPath should be excluded from traversal.
//
// Two rules from gitignore(5), both load-bearing and neither previously
// implemented. The old shape returned true on the FIRST set that ignored the
// path, walking outermost-first, so a parent's `*.py` decided the answer before
// a child's `!keep.py` was ever consulted — the ignoreStack type comment
// promised child overrides that could not happen. Deeper-wins is now decided in
// decide().
//
// The second rule is that an excluded directory cannot be re-included from
// inside it: git never descends there, so a `!` line beneath one has nothing to
// apply to. The ancestor scan below enforces it, and runs ONLY when a negation
// actually put the path back in — the common case (nothing matched, or the last
// match was an exclusion) returns without touching an ancestor, so a walk over a
// tree with no negations pays nothing for it.
func (st *ignoreStack) isIgnored(absPath string, isDir bool) bool {
	if len(*st) == 0 {
		return false
	}
	ignored, matched := st.decide(absPath, isDir)
	if ignored {
		return true
	}
	if !matched {
		return false
	}
	// A negation re-included this path. It only stands if no ancestor directory
	// is itself excluded.
	for _, anc := range ancestorDirs((*st)[0].dir, absPath) {
		if ancIgnored, _ := st.decide(anc, true); ancIgnored {
			return true
		}
	}
	return false
}

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
	var st ignoreStack
	if opts.respectIgnore {
		st = st.load(opts.root)
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
func shouldVisitEntry(name, absPath string, isDir bool, opts walkOptions, st ignoreStack) bool {
	if isDir && name == gitDirName {
		return false // unconditional — see gitDirName
	}
	if !opts.includeHidden && isHidden(name) {
		return false
	}
	if opts.respectIgnore && st.isIgnored(absPath, isDir) {
		return false
	}
	return true
}

func walkDir(ctx context.Context, dir string, depth int, st ignoreStack, opts walkOptions, fn walkFn) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil // unreadable directory — skip silently
	}

	// Load ignore rules for this directory (already loaded for root above).
	if depth > 0 && opts.respectIgnore {
		st = st.load(dir)
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
func walkSubdir(ctx context.Context, absPath string, d fs.DirEntry, depth int, st ignoreStack, opts walkOptions, fn walkFn) error {
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
