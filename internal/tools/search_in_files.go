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

// searchMaxContextLines is the ceiling for context_lines. Raised from 10 once
// formatSearchOutput gained a total-output budget: the old value existed because
// the response size was otherwise unbounded, and it was declared only in the
// JSON schema, so it was enforced by the client rather than by plumb.
const searchMaxContextLines = 50

// searchDefaultMaxFileBytes guards against a single multi-hundred-MB text
// file (a log, a JSON dump, generated SQL) stalling the walk. Files larger
// than this are skipped before opening. Callers can override via max_file_bytes.
const searchDefaultMaxFileBytes int64 = 50 * 1024 * 1024

var searchInFilesSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "pattern": {
      "type": "string",
      "description": "Text to find; a regex when use_regex is true."
    },
    "use_regex": {
      "type": "boolean",
      "default": false,
      "description": "Treat pattern as Go RE2 (default false)."
    },
    "path": {
      "type": "string",
      "description": "Directory to search (default: the workspace root)."
    },
    "glob": {
      "type": "string",
      "description": "Files to search, e.g. '*.go' or '**/*_test.go'."
    },
    "exclude": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Globs to skip (base name or relative path), e.g. [\"vendor\", \"*.pb.go\"]; matching directories are pruned."
    },
    "case_sensitive": {
      "type": "boolean",
      "description": "Default smart-case (insensitive when pattern is all lowercase); false forces insensitive."
    },
    "context_lines": {
      "type": "integer",
      "description": "Context lines around each match (default 0). Output is capped at 200 KiB, labelled.",
      "minimum": 0,
      "maximum": 50
    },
    "max_results": {
      "type": "integer",
      "description": "Matching lines returned (default 200).",
      "minimum": 1,
      "maximum": 2000
    },
    "include_hidden": {
      "type": "boolean",
      "description": "Include dot-files and dot-directories (default false)."
    },
    "max_file_bytes": {
      "type": "integer",
      "description": "Skip larger files (default 50 MiB).",
      "minimum": 1
    },
    "include_enclosing_symbol": {
      "type": "boolean",
      "description": "Annotate each match with its enclosing function or type from the language server, when available."
    }
  },
  "required": ["pattern"],
  "additionalProperties": false
}`)

// SearchInFiles implements grep-like search across workspace files.
//
// Concurrency: Execute is safe for concurrent use.
type SearchInFiles struct {
	ws        WorkspaceFn
	client    lsp.Client
	symCache  *cache.Cache
	cacheTTL  time.Duration
	guard     BoundaryGuard
	contested ContestedFn
}

func NewSearchInFiles(ws WorkspaceFn, client lsp.Client, c *cache.Cache, ttl time.Duration) *SearchInFiles {
	return &SearchInFiles{ws: ws, client: client, symCache: c, cacheTTL: ttl}
}

func (t *SearchInFiles) WithBoundary(guard BoundaryGuard) *SearchInFiles {
	t.guard = guard
	return t
}

// WithContested wires the connection's contested-pin reporter so a RELATIVE
// path is refused once the pin is contested (issue #182). Nil-safe.
func (t *SearchInFiles) WithContested(fn ContestedFn) *SearchInFiles {
	t.contested = fn
	return t
}

func (t *SearchInFiles) Name() string                 { return "search_in_files" }
func (t *SearchInFiles) InputSchema() json.RawMessage { return searchInFilesSchema }
func (t *SearchInFiles) Description() string {
	return "Exact scan of current file contents, literal or regex: for finding every occurrence, verification, audits, or preparing a safe replacement. Confined to the project and .gitignore-aware (no .git, node_modules or build output); binary and oversized files are skipped. Smart-case, with context lines and glob filters; a glob with a directory prefix prunes the walk."
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

	root, onlyFile, pathNote, err := resolveSearchRoot(ctx, a, t.ws, t.guard, t.contested)
	if err != nil {
		return "", err
	}
	re, err := compileSearchRegex(a)
	if err != nil {
		return "", err
	}

	var paths []searchPathPair
	var withheld []string
	var walkErr error
	if onlyFile != "" {
		paths = []searchPathPair{{abs: onlyFile, rel: filepath.Base(onlyFile)}}
	} else {
		paths, withheld, walkErr = t.collectSearchPaths(ctx, a, root)
	}
	withheldNote := withheldSymlinkNote(withheld)
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
		return pathNote + fmt.Sprintf("No matches for %q.", a.Pattern) +
			literalRegexHint(a.Pattern, a.UseRegex, false) + withheldNote, nil
	}

	ann := t.annotateWithSymbols(ctx, a, results)
	return pathNote + formatSearchOutput(results, ann, a, timedOut, truncated, totalLines, totalSkipped) +
		literalRegexHint(a.Pattern, a.UseRegex, true) + withheldNote, nil
}

func parseSearchInFilesArgs(raw json.RawMessage) (searchInFilesArgs, error) {
	var a searchInFilesArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, fmt.Errorf("search_in_files: invalid arguments: %w", err)
	}
	if a.Pattern == "" {
		return a, errors.New("search_in_files: pattern must not be empty")
	}
	// The declared maximum was schema-only — nothing server-side ever read it, so
	// the ceiling was enforced by whichever MCP client validated the schema and
	// not by plumb. Enforce it here too, the way read_file's search mode does.
	if a.ContextLines > searchMaxContextLines {
		return a, fmt.Errorf("search_in_files: context_lines must be between 0 and %d (got %d)",
			searchMaxContextLines, a.ContextLines)
	}
	// Brace alternation used to be refused here because filepath.Match has none;
	// doubleStarMatchFile now expands it, so "**/*.{go,md}" works. A malformed or
	// runaway group is still rejected, with the expander's own message.
	if _, err := expandBraces(a.Glob); err != nil {
		return a, fmt.Errorf("search_in_files: %w", err)
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
func resolveSearchRoot(ctx context.Context, a searchInFilesArgs, ws WorkspaceFn, guard BoundaryGuard, contested ContestedFn) (root, onlyFile, note string, err error) {
	var rerr error
	root, rerr = resolvePath(ctx, a.Path, ws, contested)
	if rerr != nil {
		return "", "", "", fmt.Errorf("search_in_files: %w", rerr)
	}
	if checkErr := guard.check(ctx, root); checkErr != nil {
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

func compileSearchRegex(a searchInFilesArgs) (*regexp.Regexp, error) {
	// Smart-case applies only when the caller said nothing: case-sensitive when
	// the pattern contains an uppercase letter, insensitive otherwise. An EXPLICIT
	// case_sensitive wins either way — including false, which forces
	// insensitivity for an uppercase pattern. That is why the field is a *bool:
	// collapsing nil and false made `case_sensitive: false` a silent no-op, so a
	// deliberate case-insensitive search for an uppercase pattern (and the
	// -i / ignore_case alias that rewrites to exactly this) ran case-SENSITIVE and
	// answered "No matches". find_replace and search_memories always honoured the
	// explicit value; these two did not.
	caseSensitive := !allLower(a.Pattern)
	if a.CaseSensitive != nil {
		caseSensitive = *a.CaseSensitive
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
