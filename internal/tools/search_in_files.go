package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/cache"
	"github.com/plumbkit/plumb/internal/lsp"
)

// This tool is split across files by concern: the walk + parallel scan live in
// search_in_files_scan.go; LSP enclosing-symbol annotation and output
// formatting in search_in_files_symbols.go. This file holds the MCP Tool
// surface, argument parsing, and the Execute orchestration.

// searchDefaultDeadline caps any single search_in_files call when the parent
// context has no deadline. Prevents a runaway walk (e.g. workspace resolved to
// $HOME, or a giant text file dragging on) from hanging the daemon past the
// MCP client's own timeout — which would otherwise leave a wedged goroutine
// behind that the user can't cancel.
const searchDefaultDeadline = 30 * time.Second

// searchMaxLineBytes caps individual lines so a minified or generated file
// cannot dominate a search. Oversized lines are skipped while the rest of the
// file is still scanned.
const searchMaxLineBytes = 1 << 20 // 1 MiB

// searchDefaultMaxFileBytes guards against a single multi-hundred-MB text
// file (a log, a JSON dump, generated SQL) stalling the walk. Files larger
// than this are skipped before opening. Callers can override via max_file_bytes.
const searchDefaultMaxFileBytes int64 = 50 * 1024 * 1024

var searchInFilesSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "pattern": {
      "type": "string",
      "description": "Plain text to search for by default; regular expression when use_regex is true."
    },
    "use_regex": {
      "type": "boolean",
      "default": false,
      "description": "Treat pattern as a regular expression (Go RE2). Default false — pattern is literal text."
    },
    "path": {
      "type": "string",
      "description": "Directory to search in (file:// URI or absolute path). Defaults to the workspace root."
    },
    "glob": {
      "type": "string",
      "description": "Glob to restrict which files are searched, e.g. '*.go' or '**/*_test.go'"
    },
    "exclude": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Glob patterns for paths to exclude, e.g. [\"vendor\", \"*.pb.go\", \"testdata/**\"]. Matched against the entry's base name and relative path. Matching directories are pruned from the walk; matching files are skipped."
    },
    "case_sensitive": {
      "type": "boolean",
      "description": "Force case-sensitive matching. Default: smart-case — case-insensitive when pattern is all lowercase, case-sensitive otherwise."
    },
    "context_lines": {
      "type": "integer",
      "description": "Number of lines of context to show before and after each match (like rg -C). Default 0.",
      "minimum": 0,
      "maximum": 10
    },
    "max_results": {
      "type": "integer",
      "description": "Maximum number of matching lines to return. Default 200.",
      "minimum": 1,
      "maximum": 2000
    },
    "include_hidden": {
      "type": "boolean",
      "description": "Include hidden files and directories (starting with '.'). Default false."
    },
    "max_file_bytes": {
      "type": "integer",
      "description": "Skip files larger than this many bytes. Default 52428800 (50 MiB).",
      "minimum": 1
    },
    "include_enclosing_symbol": {
      "type": "boolean",
      "description": "When true and an LSP is available, annotate each match with the deepest enclosing symbol (function, method, type, etc.) from the language server. One LSP query per distinct matched file; results cached within the call. Silently omitted when the LSP is unavailable."
    }
  },
  "required": ["pattern"],
  "additionalProperties": false
}`)

// SearchInFiles implements grep-like search across workspace files.
//
// Concurrency: Execute is safe for concurrent use.
type SearchInFiles struct {
	ws       WorkspaceFn
	client   lsp.Client
	symCache *cache.Cache
	cacheTTL time.Duration
	guard    BoundaryGuard
}

func NewSearchInFiles(ws WorkspaceFn, client lsp.Client, c *cache.Cache, ttl time.Duration) *SearchInFiles {
	return &SearchInFiles{ws: ws, client: client, symCache: c, cacheTTL: ttl}
}

func (t *SearchInFiles) WithBoundary(guard BoundaryGuard) *SearchInFiles {
	t.guard = guard
	return t
}

func (t *SearchInFiles) Name() string                 { return "search_in_files" }
func (t *SearchInFiles) InputSchema() json.RawMessage { return searchInFilesSchema }
func (t *SearchInFiles) Description() string {
	return "Exact scan of current file contents — literal text by default, regex when use_regex=true. " +
		"Use search_in_files when you need every occurrence, exact verification, audits, or safe replacement prep. " +
		"For broad conceptual discovery (\"where is daemon locking handled?\"), prefer workspace_search (ranked, across code/docs/memories) or topology_search instead. " +
		"For symbol name lookups (finding a function, type, or variable by name), prefer workspace_symbols — " +
		"it uses the LSP index and returns results instantly. " +
		"Prefer this over shelling out to grep/rg: " +
		"results are confined to the active project (no .git/, node_modules/, build artefacts, or anything else .gitignore excludes), " +
		"binary files are skipped (null-byte sniff of the first 8 KB), files larger than max_file_bytes (50 MiB default) are skipped before opening, " +
		"globs with a literal directory prefix (e.g. \"src/**/*.go\") prune sibling directories from the walk. " +
		"Smart-case (case-insensitive when the pattern is all lowercase), supports context lines and glob file filters."
}

type searchInFilesArgs struct {
	Pattern                string   `json:"pattern"`
	UseRegex               bool     `json:"use_regex"`
	Path                   string   `json:"path"`
	Glob                   string   `json:"glob"`
	Exclude                []string `json:"exclude"`
	CaseSensitive          *bool    `json:"case_sensitive"`
	ContextLines           int      `json:"context_lines"`
	MaxResults             int      `json:"max_results"`
	IncludeHidden          bool     `json:"include_hidden"`
	MaxFileBytes           int64    `json:"max_file_bytes"`
	IncludeEnclosingSymbol bool     `json:"include_enclosing_symbol"`
}

// searchPathPair is a resolved candidate file for parallel scanning.
type searchPathPair struct{ abs, rel string }

// searchFileMatch holds per-file results from a parallel scan.
type searchFileMatch struct {
	relPath      string
	absPath      string
	lines        []string
	hitLineNums  []int
	hits         int
	skippedLines int
}

func (t *SearchInFiles) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	a, err := parseSearchInFilesArgs(raw)
	if err != nil {
		return "", err
	}
	applySearchDefaults(&a)

	ctx, cancel := applySearchDeadline(ctx)
	defer cancel()

	root, onlyFile, pathNote, err := resolveSearchRoot(a, t.ws, t.guard)
	if err != nil {
		return "", err
	}
	re, err := compileSearchRegex(a)
	if err != nil {
		return "", err
	}

	var paths []searchPathPair
	var walkErr error
	if onlyFile != "" {
		paths = []searchPathPair{{abs: onlyFile, rel: filepath.Base(onlyFile)}}
	} else {
		paths, walkErr = t.collectSearchPaths(ctx, a, root)
	}
	results, totalLines, totalSkipped, truncated := t.runParallelScan(ctx, paths, a, re)

	sort.Slice(results, func(i, j int) bool { return results[i].relPath < results[j].relPath })

	timedOut := errors.Is(walkErr, context.DeadlineExceeded)
	cancelled := errors.Is(walkErr, context.Canceled)
	if len(results) == 0 {
		if timedOut {
			return pathNote + fmt.Sprintf("Search for %q timed out before any matches were found (budget %s — narrow with path/glob, or set a tighter pattern).", a.Pattern, searchDefaultDeadline), nil
		}
		if cancelled {
			return "", walkErr
		}
		return pathNote + fmt.Sprintf("No matches for %q.", a.Pattern) + literalMetacharHint(a), nil
	}

	ann := t.annotateWithSymbols(ctx, a, results)
	return pathNote + formatSearchOutput(results, ann, a, timedOut, truncated, totalLines, totalSkipped), nil
}

func parseSearchInFilesArgs(raw json.RawMessage) (searchInFilesArgs, error) {
	var a searchInFilesArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, fmt.Errorf("search_in_files: invalid arguments: %w", err)
	}
	if a.Pattern == "" {
		return a, fmt.Errorf("search_in_files: pattern must not be empty")
	}
	if strings.ContainsAny(a.Glob, "{}") {
		return a, fmt.Errorf(
			"search_in_files: glob %q contains brace alternation {...} which filepath.Match does not support; "+
				"run separate searches for each extension instead (e.g. two calls with \"*.go\" and \"*.ts\")",
			a.Glob,
		)
	}
	return a, nil
}

func applySearchDefaults(a *searchInFilesArgs) {
	if a.MaxResults <= 0 {
		a.MaxResults = 200
	}
	if a.ContextLines < 0 {
		a.ContextLines = 0
	}
	if a.MaxFileBytes == 0 {
		a.MaxFileBytes = searchDefaultMaxFileBytes
	}
}

func applySearchDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); !ok {
		return context.WithTimeout(ctx, searchDefaultDeadline)
	}
	return ctx, func() {}
}

// resolveSearchRoot resolves the search root directory. When a names a single
// file rather than a directory, the search is scoped to THAT file (onlyFile is
// the absolute path; root is its parent so relative paths still resolve) — a
// file path is more specific than its directory, so scoping to it is what the
// caller almost always meant (from dogfooding feedback).
func resolveSearchRoot(a searchInFilesArgs, ws WorkspaceFn, guard BoundaryGuard) (root, onlyFile, note string, err error) {
	root = resolvePath(a.Path, ws)
	if checkErr := guard.check(root); checkErr != nil {
		return "", "", "", fmt.Errorf("search_in_files: %w", checkErr)
	}
	info, statErr := os.Stat(root)
	if statErr != nil {
		return "", "", "", fmt.Errorf("search_in_files: path %q: %w", root, statErr)
	}
	if !info.IsDir() {
		note = fmt.Sprintf("Note: path was a file — searching only %s.\n\n", filepath.Base(root))
		return filepath.Dir(root), root, note, nil
	}
	return root, "", "", nil
}

// literalMetacharHint returns a one-line nudge when a literal-mode (use_regex
// false) search used a pattern containing unambiguous regex syntax — `|`
// alternation or `.*`/`.+` — which was therefore matched literally. It only
// fires on a zero-match result, the false-negative the feedback log flagged
// (e.g. searching "A|B|C" literally and reading the clean "No matches" as
// "these don't exist"). Conservative on purpose: a bare `.` does not trigger it.
func literalMetacharHint(a searchInFilesArgs) string {
	if a.UseRegex {
		return ""
	}
	if !strings.Contains(a.Pattern, "|") && !strings.Contains(a.Pattern, ".*") && !strings.Contains(a.Pattern, ".+") {
		return ""
	}
	return "\nNote: the pattern contains regex syntax (| alternation or .*) but use_regex is false, so it was matched literally. Pass use_regex: true to treat it as a pattern."
}

func compileSearchRegex(a searchInFilesArgs) (*regexp.Regexp, error) {
	// Smart-case: case-sensitive when the pattern contains any uppercase letter
	// or when the caller forces it; case-insensitive otherwise.
	caseSensitive := a.CaseSensitive != nil && *a.CaseSensitive
	if !caseSensitive && !allLower(a.Pattern) {
		caseSensitive = true
	}
	flags := ""
	if !caseSensitive {
		flags = "(?i)"
	}
	if a.UseRegex {
		re, err := regexp.Compile(flags + a.Pattern)
		if err != nil {
			return nil, fmt.Errorf("search_in_files: invalid regex %q: %w", a.Pattern, err)
		}
		return re, nil
	}
	// Literal mode (default): QuoteMeta so metacharacters match themselves.
	return regexp.MustCompile(flags + regexp.QuoteMeta(a.Pattern)), nil
}
