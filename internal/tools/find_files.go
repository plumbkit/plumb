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
const findFilesDefaultDeadline = 30 * time.Second

var findFilesSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "pattern": {
      "type": "string",
      "description": "Glob (or regex if use_regex=true) matched against the file/directory name. When the pattern contains '/' it matches the full relative path. Use \"*\" to match everything — a literal \".\" only matches a file named \".\"."
    },
    "path": {
      "type": "string",
      "description": "Directory to search in (file:// URI or absolute path). Defaults to the workspace root."
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
      "description": "Maximum directory depth to descend. Default: unlimited.",
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
    "use_regex": {
      "type": "boolean",
      "description": "Treat pattern as a regular expression instead of a glob. Default false."
    }
  },
  "required": ["pattern"]
}`)

// FindFiles implements fd-like recursive file/directory finding.
type FindFiles struct{ ws WorkspaceFn }

func NewFindFiles(ws WorkspaceFn) *FindFiles { return &FindFiles{ws: ws} }

func (t *FindFiles) Name() string                 { return "find_files" }
func (t *FindFiles) InputSchema() json.RawMessage { return findFilesSchema }
func (t *FindFiles) Description() string {
	return "Workspace-scoped file/directory finder. Prefer this over shelling out to find/fd: " +
		"results are confined to the active project (no .git/, node_modules/, build output, or anything else .gitignore excludes), " +
		"every call is recorded in the project's stats, and the pattern semantics are consistent across hosts. " +
		"Supports glob and regex patterns, extension filters, type filters (file/dir), and depth limits. " +
		"Essential for clients without filesystem access of their own (Claude Desktop, Cursor MCP, etc.)."
}

type findFilesArgs struct {
	Pattern       string `json:"pattern"`
	Path          string `json:"path"`
	Type          string `json:"type"`
	Extension     string `json:"extension"`
	MaxDepth      int    `json:"max_depth"`
	MaxResults    int    `json:"max_results"`
	IncludeHidden bool   `json:"include_hidden"`
	UseRegex      bool   `json:"use_regex"`
}

// findFilesConfig holds the resolved walk parameters derived from findFilesArgs.
type findFilesConfig struct {
	root            string
	ext             string
	matchFn         func(string) bool
	patternHasSlash bool
	globPrefix      string
}

// findFilesWalker accumulates results for a single find_files call. Keeping
// state in a struct lets the walk callback (visit) be a named method instead
// of a closure, reducing cyclomatic complexity.
type findFilesWalker struct {
	ctx       context.Context
	cfg       findFilesConfig
	a         findFilesArgs
	hits      []string
	truncated bool
}

func (t *FindFiles) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	a, err := parseFindFilesArgs(raw)
	if err != nil {
		return "", err
	}
	applyFindFilesDefaults(&a)

	ctx, cancel := applyFindFilesDeadline(ctx)
	defer cancel()

	cfg, err := buildFindFilesConfig(a, t.ws)
	if err != nil {
		return "", err
	}

	hits, truncated, walkErr := findFilesWalkTree(ctx, a, cfg)

	timedOut := errors.Is(walkErr, context.DeadlineExceeded)
	cancelled := errors.Is(walkErr, context.Canceled)
	if len(hits) == 0 {
		if timedOut {
			return fmt.Sprintf("find_files for %q timed out before any matches were found (budget %s — narrow with path or max_depth).", a.Pattern, findFilesDefaultDeadline), nil
		}
		if cancelled {
			return "", walkErr
		}
		if walkErr != nil {
			return "", fmt.Errorf("find_files: walking %s: %w", cfg.root, walkErr)
		}
		return fmt.Sprintf("No files found matching %q.", a.Pattern), nil
	}

	return formatFindFilesOutput(hits, a, truncated, walkErr), nil
}

func parseFindFilesArgs(raw json.RawMessage) (findFilesArgs, error) {
	var a findFilesArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, fmt.Errorf("find_files: invalid arguments: %w", err)
	}
	if a.Pattern == "" {
		return a, fmt.Errorf("find_files: pattern must not be empty")
	}
	return a, nil
}

func applyFindFilesDefaults(a *findFilesArgs) {
	if a.MaxResults <= 0 {
		a.MaxResults = 500
	}
	if a.Type == "" {
		a.Type = "file"
	}
}

func applyFindFilesDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); !ok {
		return context.WithTimeout(ctx, findFilesDefaultDeadline)
	}
	return ctx, func() {}
}

func buildFindFilesConfig(a findFilesArgs, ws WorkspaceFn) (findFilesConfig, error) {
	root := resolvePath(a.Path, ws)
	info, err := os.Stat(root)
	if err != nil {
		return findFilesConfig{}, fmt.Errorf("find_files: path %q: %w", root, err)
	}
	if !info.IsDir() {
		root = filepath.Dir(root)
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
		patternHasSlash: patternHasSlash, globPrefix: globPrefix,
	}, nil
}

func findFilesWalkTree(ctx context.Context, a findFilesArgs, cfg findFilesConfig) ([]string, bool, error) {
	w := &findFilesWalker{ctx: ctx, cfg: cfg, a: a}
	opts := walkOptions{
		root:          cfg.root,
		maxDepth:      a.MaxDepth,
		includeHidden: a.IncludeHidden,
		respectIgnore: true,
	}
	walkErr := walk(ctx, opts, w.visit)
	return w.hits, w.truncated, walkErr
}

func (w *findFilesWalker) visit(path string, d fs.DirEntry, _ int) error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	if w.truncated {
		return nil
	}
	isDir := d.IsDir()
	// Prune incompatible directory subtrees before any other filtering.
	if isDir && w.cfg.globPrefix != "" && path != w.cfg.root {
		rel, _ := filepath.Rel(w.cfg.root, path)
		if !dirCompatibleWithPrefix(filepath.ToSlash(rel), w.cfg.globPrefix) {
			return fs.SkipDir
		}
	}
	if !w.passesTypeFilter(isDir) {
		return nil
	}
	if !w.passesExtFilter(d, isDir) {
		return nil
	}
	rel, _ := filepath.Rel(w.cfg.root, path)
	rel = filepath.ToSlash(rel)
	target := d.Name()
	if w.cfg.patternHasSlash {
		target = rel
	}
	if !w.cfg.matchFn(target) {
		return nil
	}
	w.hits = append(w.hits, rel)
	if len(w.hits) >= w.a.MaxResults {
		w.truncated = true
	}
	return nil
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

func formatFindFilesOutput(hits []string, a findFilesArgs, truncated bool, walkErr error) string {
	var sb strings.Builder
	for _, h := range hits {
		sb.WriteString(h)
		sb.WriteByte('\n')
	}
	switch {
	case truncated:
		fmt.Fprintf(&sb, "\n(truncated at %d results — use a more specific pattern or set max_depth)", a.MaxResults)
	case errors.Is(walkErr, context.DeadlineExceeded):
		fmt.Fprintf(&sb, "\n%d result(s) (partial — walk timed out after %s; narrow with path or max_depth)", len(hits), findFilesDefaultDeadline)
	case walkErr != nil:
		fmt.Fprintf(&sb, "\n%d result(s) (partial — walk stopped: %v)", len(hits), walkErr)
	default:
		fmt.Fprintf(&sb, "\n%d result(s)", len(hits))
	}
	return sb.String()
}

// buildMatcher returns a function that tests a name/path against the pattern.
func buildMatcher(pattern string, useRegex bool) (func(string) bool, error) {
	if useRegex {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, err
		}
		return re.MatchString, nil
	}

	// Glob mode: if the pattern contains **, use doubleStarMatch.
	// Otherwise use filepath.Match which handles *, ?, [...].
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
