// Package ignore implements gitignore-style path exclusion: parsing
// .gitignore / .ignore files and answering whether a path is excluded.
//
// It exists as its own package because four separate walks need the same
// answer and must not each grow their own version of it — the filesystem tools
// (search_in_files, find_files, find_replace), the language-detection content
// sniff, the session_start workspace census, and the topology indexer. Three of
// those previously walked with a hardcoded skip list instead, so a repository
// that gitignores a large generated or vendored tree had it counted, indexed
// and searched anyway.
//
// Foundation layer: stdlib only, no plumb imports. See internal/arch.
package ignore

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// pattern is one compiled line from a .gitignore / .ignore file.
type pattern struct {
	negate   bool   // line starts with !
	dirOnly  bool   // line ends with /
	rooted   bool   // line starts with / (after negation strip)
	hasSlash bool   // line contains / (match against path, not just name)
	glob     string // the cleaned glob to match
}

// parseLine parses one non-blank, non-comment gitignore line.
func parseLine(raw string) (pattern, bool) {
	line := strings.TrimRight(raw, " \t") // trailing whitespace is ignored
	if line == "" || strings.HasPrefix(line, "#") {
		return pattern{}, false
	}

	p := pattern{}
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
func (p pattern) matchesPath(relPath string, isDir bool) bool {
	if p.dirOnly && !isDir {
		return false
	}
	if !p.hasSlash && !p.rooted {
		// Match against base name only (unless the pattern contains a slash).
		return DoubleStarMatch(p.glob, filepath.Base(relPath))
	}
	return DoubleStarMatch(p.glob, relPath)
}

// DoubleStarMatch is filepath.Match extended to support the ** wildcard, where
// ** matches zero or more path components.
//
// Exported because it is the glob primitive gitignore matching is built on AND
// the one find_files and search_in_files bottom out in; a second copy in the
// tools package would be the same code with a separate set of bugs. Note that
// filepath.Match has no brace syntax and neither does this: `{`, `}` and `,`
// are ordinary literal characters. Brace expansion happens one layer above it
// in the tools package, deliberately, so that giving find_files braces cannot
// silently change which files a .gitignore excludes.
func DoubleStarMatch(pat, name string) bool {
	// Fast path: no doublestar.
	if !strings.Contains(pat, "**") {
		m, _ := filepath.Match(pat, name)
		return m
	}

	// Split on **/ segments and match greedily.
	// e.g. "a/**/b" → ["a/", "b"]
	parts := strings.SplitN(pat, "**/", 2)
	if len(parts) == 1 {
		// Trailing **: "dir/**" matches anything under dir/.
		prefix := strings.TrimSuffix(pat, "**")
		return strings.HasPrefix(name, prefix)
	}
	left, right := parts[0], parts[1]

	if left == "" {
		// **/right — try matching right against name, or against any suffix.
		if DoubleStarMatch(right, name) {
			return true
		}
		// Walk through each directory prefix.
		idx := strings.Index(name, "/")
		for idx >= 0 {
			name = name[idx+1:]
			if DoubleStarMatch(right, name) {
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
	return DoubleStarMatch("**/"+right, name)
}

// set holds the patterns from one directory's ignore files.
type set struct {
	dir      string // absolute directory owning these patterns
	patterns []pattern
}

// Stack accumulates ignore rules as a walker descends directories, ordered
// outermost-first. Rules from parent directories are inherited; a child
// directory can override them.
//
// The zero value is a usable empty stack that ignores nothing. A Stack is
// immutable once built: Load returns a new one rather than mutating the
// receiver, so a walker can hand each subdirectory its own stack without the
// siblings seeing it.
type Stack []*set

// Load reads .gitignore and .ignore from dir and returns a stack with dir's
// patterns appended. When dir has no ignore file, or none with usable
// patterns, the receiver is returned unchanged.
func (st Stack) Load(dir string) Stack {
	var patterns []pattern
	for _, name := range []string{".gitignore", ".ignore"} {
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			p, ok := parseLine(sc.Text())
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
		return st
	}
	next := make(Stack, len(st)+1)
	copy(next, st)
	next[len(st)] = &set{dir: dir, patterns: patterns}
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
func (st Stack) decide(absPath string, isDir bool) (ignored, matched bool) {
	for _, s := range st {
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

// IsIgnored reports whether absPath should be excluded from traversal.
//
// Two rules from gitignore(5), both load-bearing and neither implemented
// before this package existed. The old shape returned true on the FIRST set
// that ignored the path, walking outermost-first, so a parent's `*.py` decided
// the answer before a child's `!keep.py` was ever consulted — the stack
// promised child overrides that could not happen. Deeper-wins is decided in
// decide().
//
// The second rule is that an excluded directory cannot be re-included from
// inside it: git never descends there, so a `!` line beneath one has nothing to
// apply to. The ancestor scan below enforces it, and runs ONLY when a negation
// actually put the path back in — the common case (nothing matched, or the last
// match was an exclusion) returns without touching an ancestor, so a walk over a
// tree with no negations pays nothing for it.
func (st Stack) IsIgnored(absPath string, isDir bool) bool {
	if len(st) == 0 {
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
	for _, anc := range ancestorDirs(st[0].dir, absPath) {
		if ancIgnored, _ := st.decide(anc, true); ancIgnored {
			return true
		}
	}
	return false
}
