package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// findFilesDefaultDeadline caps a single find_files call when the parent
// context has no deadline. Matches search_in_files: prevents a runaway walk
// over a giant tree from outliving the MCP client's own timeout.
//
// find_files deliberately does NOT implement mcp.ExecTimeoutBounded, and the
// two are not equivalent. That marker is not merely a shorter budget: the
// dispatcher runs Execute on a child goroutine and returns to the caller when
// its timer fires, so the call escapes even a blocked syscall. This deadline is
// cooperative only — the root os.Stat in buildFindFilesConfig and os.ReadDir
// inside the walk are uninterruptible — so a stalled network/FUSE mount can
// outlast it. The trade is deliberate: opting in would newly time out broad
// walks over large healthy trees. Note that the retired list_files and
// list_directory did carry the bound, so old-name callers lose it; revisit if
// stalled-mount reports arrive.
const findFilesDefaultDeadline = 30 * time.Second

// findFilesSortScanCap bounds the walk when a size/modified ranking is asked
// for. Those orders cannot be honoured by the walk's early stop — "largest
// first" over whichever max_results entries the traversal happened to reach is
// not largest-first at all — so the stop is lifted and replaced by this
// ceiling, which matches max_results' own schema maximum. A name sort keeps the
// early stop: the traversal is already path-ordered, so truncation drops the
// tail either way.
const findFilesSortScanCap = 5000

var findFilesSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "pattern": {
      "type": "string",
      "description": "Glob (or regex if use_regex=true) matched against the file/directory name. When the pattern contains '/' it matches the full relative path. OPTIONAL — omit it to list every entry. A literal \".\" only matches a file named \".\"."
    },
    "path": {
      "type": "string",
      "description": "Directory to search in (absolute path, file:// URI, or workspace-relative path). Defaults to the workspace root."
    },
    "type": {
      "type": "string",
      "enum": ["file", "dir", "any"],
      "description": "Restrict to files, directories, or both. Default: 'file'."
    },
    "extension": {
      "type": "string",
      "description": "Filter by file extension, e.g. 'go' or '.go'."
    },
    "max_depth": {
      "type": "integer",
      "description": "Maximum directory depth to descend. 1 lists one level only, like ls. Default: unlimited.",
      "minimum": 1
    },
    "max_results": {
      "type": "integer",
      "description": "Maximum number of results to return. Default 500.",
      "minimum": 1,
      "maximum": 5000
    },
    "include_hidden": {
      "type": "boolean",
      "description": "Include hidden files and directories (starting with '.'). Default false."
    },
    "include_details": {
      "type": "boolean",
      "description": "Render each entry with a [FILE]/[DIR]/[LINK] marker, its size and last-modified time (symlinks as 'name -> target') instead of a bare path list. Default false."
    },
    "sort_by": {
      "type": "string",
      "enum": ["name", "size", "modified"],
      "description": "Order of the result list: 'name' (directories first, then path), 'size' (largest first), 'modified' (newest first). Default: name."
    },
    "use_regex": {
      "type": "boolean",
      "description": "Treat pattern as a regular expression instead of a glob. Default false."
    }
  },
  "additionalProperties": false
}`)

// FindFiles implements fd-like recursive file/directory finding, and is also
// plumb's one directory listing tool — list_files and list_directory were
// folded into it.
type FindFiles struct {
	ws    WorkspaceFn
	guard BoundaryGuard
}

func NewFindFiles(ws WorkspaceFn) *FindFiles { return &FindFiles{ws: ws} }

func (t *FindFiles) WithBoundary(guard BoundaryGuard) *FindFiles {
	t.guard = guard
	return t
}

func (t *FindFiles) Name() string                 { return "find_files" }
func (t *FindFiles) InputSchema() json.RawMessage { return findFilesSchema }
func (t *FindFiles) Description() string {
	return "Workspace-scoped file/directory finder and directory lister. Unlike shell find/fd/ls, " +
		"results are confined to the active project (no .git/, node_modules/, build output, or anything else .gitignore excludes), " +
		"every call is recorded in the project's stats, and the pattern semantics are consistent across hosts. " +
		"pattern is optional — omit it to list everything. Supports glob and regex patterns, extension and type (file/dir/any) filters, " +
		"depth limits (max_depth=1 lists one level, like ls), sort_by name/size/modified, and include_details for a per-entry " +
		"[FILE]/[DIR]/[LINK] marker, size, and modified time."
}

type findFilesArgs struct {
	Pattern        string `json:"pattern"`
	Path           string `json:"path"`
	Type           string `json:"type"`
	Extension      string `json:"extension"`
	MaxDepth       int    `json:"max_depth"`
	MaxResults     int    `json:"max_results"`
	IncludeHidden  bool   `json:"include_hidden"`
	IncludeDetails bool   `json:"include_details"`
	SortBy         string `json:"sort_by"`
	UseRegex       bool   `json:"use_regex"`
}

// findFilesConfig holds the resolved walk parameters derived from findFilesArgs.
type findFilesConfig struct {
	root            string
	ext             string
	matchFn         func(string) bool
	patternHasSlash bool
	globPrefix      string
	note            string        // leading advisory prepended to the result, "" when there is none
	guard           BoundaryGuard // consulted by the walk for every symlink it meets
}

// findFilesWalker accumulates results for a single find_files call. Keeping
// state in a struct lets the walk callback (visit) be a named method instead
// of a closure, reducing cyclomatic complexity.
type findFilesWalker struct {
	ctx        context.Context
	cfg        findFilesConfig
	a          findFilesArgs
	collectCap int
	stat       bool // populate each hit's size/mtime/symlink fields
	hits       []findFileHit
	truncated  bool
	// withheld holds the relative paths of entries the walk skipped for
	// resolving outside the boundary.
	withheld []string
}

func (t *FindFiles) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	a, err := parseFindFilesArgs(raw)
	if err != nil {
		return "", err
	}
	applyFindFilesDefaults(&a)

	ctx, cancel := applyFindFilesDeadline(ctx)
	defer cancel()

	cfg, err := buildFindFilesConfig(ctx, a, t.ws, t.guard)
	if err != nil {
		return "", err
	}

	hits, truncated, withheld, walkErr := findFilesWalkTree(ctx, a, cfg)
	withheldNote := withheldSymlinkNote(withheld)
	sortFindFileHits(hits, a.SortBy)
	if len(hits) > a.MaxResults {
		hits, truncated = hits[:a.MaxResults], true
	}
	if len(hits) == 0 {
		out, err := emptyFindFilesResult(a, cfg, walkErr)
		if err != nil {
			return "", err
		}
		return cfg.note + out + withheldNote, nil
	}
	return cfg.note + formatFindFilesOutput(hits, a, cfg.root, truncated, walkErr) + withheldNote, nil
}

func parseFindFilesArgs(raw json.RawMessage) (findFilesArgs, error) {
	var a findFilesArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, fmt.Errorf("find_files: invalid arguments: %w", err)
	}
	if err := checkFindFilesDepth(raw); err != nil {
		return a, err
	}
	return a, nil
}

// checkFindFilesDepth enforces max_depth's declared "minimum": 1. The struct's
// zero value cannot tell an explicit 0 from an absent key, and 0 means
// "unlimited" downstream — so an out-of-range depth would silently INVERT the
// caller's intent (0 reads as "shallowest possible", answers as "the whole
// tree"). A pointer probe over the raw object turns it into a clean rejection,
// the same shape read_file's "limit must be >= 1" guard uses.
func checkFindFilesDepth(raw json.RawMessage) error {
	var probe struct {
		MaxDepth *int `json:"max_depth"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil // not an object — the decode above already reported it
	}
	if probe.MaxDepth != nil && *probe.MaxDepth < 1 {
		return fmt.Errorf("find_files: max_depth must be >= 1 (got %d) — omit it to descend without limit", *probe.MaxDepth)
	}
	return nil
}

func applyFindFilesDefaults(a *findFilesArgs) {
	if a.MaxResults <= 0 {
		a.MaxResults = 500
	}
	if a.Type == "" {
		a.Type = "file"
	}
	if a.SortBy == "" {
		a.SortBy = "name"
	}
}

func applyFindFilesDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); !ok {
		return context.WithTimeout(ctx, findFilesDefaultDeadline)
	}
	return ctx, func() {}
}

func buildFindFilesConfig(ctx context.Context, a findFilesArgs, ws WorkspaceFn, guard BoundaryGuard) (findFilesConfig, error) {
	root := resolvePath(ctx, a.Path, ws)
	if err := guard.check(ctx, root); err != nil {
		return findFilesConfig{}, fmt.Errorf("find_files: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return findFilesConfig{}, fmt.Errorf("find_files: path %q: %w", root, err)
	}
	// A file path walks its PARENT rather than erroring — long-standing find_files
	// behaviour (a caller who points at a file usually means "around here"), kept
	// so canonical callers are not broken. It is announced, though: the retired
	// list_directory hard-errored on a file, so its alias must never answer a
	// different question in silence.
	var note string
	if !info.IsDir() {
		parent := filepath.Dir(root)
		note = fmt.Sprintf("note: %s is a file — listing its parent directory %s.\n\n", root, parent)
		root = parent
	}
	ext := strings.ToLower(strings.TrimPrefix(a.Extension, "."))
	matchFn, err := buildMatcher(a.Pattern, a.UseRegex)
	if err != nil {
		return findFilesConfig{}, fmt.Errorf("find_files: invalid pattern %q: %w", a.Pattern, err)
	}
	patternHasSlash := strings.Contains(a.Pattern, "/")
	var globPrefix string
	if patternHasSlash && !a.UseRegex {
		globPrefix = globLiteralPrefix(a.Pattern)
	}
	return findFilesConfig{
		root: root, ext: ext, matchFn: matchFn,
		patternHasSlash: patternHasSlash, globPrefix: globPrefix, note: note, guard: guard,
	}, nil
}

func findFilesWalkTree(ctx context.Context, a findFilesArgs, cfg findFilesConfig) ([]findFileHit, bool, []string, error) {
	ranked := a.SortBy == "size" || a.SortBy == "modified"
	collectCap := a.MaxResults
	if ranked && findFilesSortScanCap > collectCap {
		collectCap = findFilesSortScanCap
	}
	w := &findFilesWalker{ctx: ctx, cfg: cfg, a: a, collectCap: collectCap, stat: ranked || a.IncludeDetails}
	opts := walkOptions{
		root:          cfg.root,
		maxDepth:      a.MaxDepth,
		includeHidden: a.IncludeHidden,
		respectIgnore: true,
		boundary:      cfg.guard,
		onWithheld:    w.withhold,
	}
	walkErr := walk(ctx, opts, w.visit)
	return w.hits, w.truncated, w.withheld, walkErr
}

// withhold records an entry the walk skipped for resolving outside the
// boundary, relative to the walk root so the note names an in-workspace path.
func (w *findFilesWalker) withhold(absPath string) {
	rel, err := filepath.Rel(w.cfg.root, absPath)
	if err != nil {
		rel = filepath.Base(absPath)
	}
	w.withheld = append(w.withheld, filepath.ToSlash(rel))
}

func (w *findFilesWalker) visit(path string, d fs.DirEntry, depth int) error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	if w.truncated {
		return nil
	}
	isDir := d.IsDir()
	if isDir && w.shouldPrune(path) {
		return fs.SkipDir
	}
	rel, _ := filepath.Rel(w.cfg.root, path)
	rel = filepath.ToSlash(rel)
	if !w.matches(rel, d, isDir, depth) {
		return nil
	}
	// Truncation means the walk STOPPED SHORT — proven by a match arriving with
	// the collection ceiling already full, never by the ceiling merely being
	// reached. A result set of exactly max_results entries that exhausted the
	// tree is complete, and saying otherwise sends the caller narrowing a search
	// that had nothing left to find.
	if len(w.hits) >= w.collectCap {
		w.truncated = true
		return nil
	}
	w.hits = append(w.hits, newFindFileHit(path, rel, d, isDir, w.stat))
	return nil
}

// shouldPrune reports whether a directory subtree cannot hold a match for a
// slash-bearing glob, so the walk can skip it without descending.
func (w *findFilesWalker) shouldPrune(path string) bool {
	if w.cfg.globPrefix == "" || path == w.cfg.root {
		return false
	}
	rel, _ := filepath.Rel(w.cfg.root, path)
	return !dirCompatibleWithPrefix(filepath.ToSlash(rel), w.cfg.globPrefix)
}

// matches applies the depth, type, extension, and pattern filters. The depth
// check is belt-and-braces over the shared walk's own strict prune: the walk no
// longer descends into a directory whose children would sit past the limit, so
// nothing over-deep should reach here — but this tool's contract ("max_depth=1
// lists one level") is stated in its schema, and it enforces it itself rather
// than inheriting it from a shared traversal another tool could retune.
func (w *findFilesWalker) matches(rel string, d fs.DirEntry, isDir bool, depth int) bool {
	if w.a.MaxDepth > 0 && depth >= w.a.MaxDepth {
		return false
	}
	if !w.passesTypeFilter(isDir) || !w.passesExtFilter(d, isDir) {
		return false
	}
	target := d.Name()
	if w.cfg.patternHasSlash {
		target = rel
	}
	return w.cfg.matchFn(target)
}

func (w *findFilesWalker) passesTypeFilter(isDir bool) bool {
	switch w.a.Type {
	case "file":
		return !isDir
	case "dir":
		return isDir
	default:
		return true
	}
}

func (w *findFilesWalker) passesExtFilter(d fs.DirEntry, isDir bool) bool {
	if w.cfg.ext == "" || isDir {
		return true
	}
	return strings.ToLower(strings.TrimPrefix(filepath.Ext(d.Name()), ".")) == w.cfg.ext
}

// emptyFindFilesResult renders the no-hits answer. A detailed listing reports
// an empty directory the way list_directory did; a plain one names what was
// looked for, which is a pattern only when the caller supplied one.
func emptyFindFilesResult(a findFilesArgs, cfg findFilesConfig, walkErr error) (string, error) {
	switch {
	case errors.Is(walkErr, context.DeadlineExceeded):
		return fmt.Sprintf("find_files %s timed out before any matches were found (budget %s — narrow with path or max_depth).",
			findFilesSubject(a, cfg.root), findFilesDefaultDeadline), nil
	case errors.Is(walkErr, context.Canceled):
		return "", walkErr
	case walkErr != nil:
		return "", fmt.Errorf("find_files: walking %s: %w", cfg.root, walkErr)
	case a.IncludeDetails:
		return cfg.root + "\n\n(empty)\n", nil
	case a.Pattern == "":
		return "No entries found under " + cfg.root + ".", nil
	default:
		return fmt.Sprintf("No files found matching %q.", a.Pattern), nil
	}
}

// findFilesSubject names what a call was looking for, so the timeout message
// reads correctly whether or not a pattern was given.
func findFilesSubject(a findFilesArgs, root string) string {
	if a.Pattern == "" {
		return "under " + root
	}
	return fmt.Sprintf("for %q", a.Pattern)
}

// buildMatcher returns a function that tests a name/path against the pattern.
// An empty pattern matches everything — find_files doubles as a plain lister.
func buildMatcher(pattern string, useRegex bool) (func(string) bool, error) {
	if pattern == "" {
		return func(string) bool { return true }, nil
	}
	if useRegex {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, err
		}
		return re.MatchString, nil
	}

	// Glob mode. Brace alternation is expanded first (filepath.Match has none of
	// its own and would match "*.{ts,js}" against a file literally named that),
	// then each alternative gets the ** or plain matcher. A name matches when ANY
	// alternative does.
	alts, err := expandBraces(pattern)
	if err != nil {
		return nil, fmt.Errorf("find_files: %w", err)
	}
	matchers := make([]func(string) bool, 0, len(alts))
	for _, alt := range alts {
		m, err := buildGlobMatcher(alt)
		if err != nil {
			return nil, err
		}
		matchers = append(matchers, m)
	}
	if len(matchers) == 1 {
		return matchers[0], nil
	}
	return func(name string) bool {
		for _, m := range matchers {
			if m(name) {
				return true
			}
		}
		return false
	}, nil
}

// buildGlobMatcher returns a matcher for a single brace-free glob: doubleStarMatch
// when it contains **, filepath.Match otherwise.
func buildGlobMatcher(pattern string) (func(string) bool, error) {
	if strings.Contains(pattern, "**") {
		return func(name string) bool {
			return doubleStarMatch(pattern, name)
		}, nil
	}
	// Validate the glob before returning.
	if _, err := filepath.Match(pattern, ""); err != nil {
		return nil, err
	}
	return func(name string) bool {
		m, _ := filepath.Match(pattern, name)
		return m
	}, nil
}
