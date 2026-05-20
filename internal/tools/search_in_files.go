package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/golimpio/plumb/internal/cache"
	"github.com/golimpio/plumb/internal/lsp"
	"github.com/golimpio/plumb/internal/lsp/protocol"
)

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
      "description": "Regular expression (or literal string) to search for"
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
  "required": ["pattern"]
}`)

// SearchInFiles implements grep-like search across workspace files.
//
// Concurrency: Execute is safe for concurrent use.
type SearchInFiles struct {
	ws       WorkspaceFn
	client   lsp.Client
	symCache *cache.Cache
	cacheTTL time.Duration
}

func NewSearchInFiles(ws WorkspaceFn, client lsp.Client, c *cache.Cache, ttl time.Duration) *SearchInFiles {
	return &SearchInFiles{ws: ws, client: client, symCache: c, cacheTTL: ttl}
}

func (t *SearchInFiles) Name() string                 { return "search_in_files" }
func (t *SearchInFiles) InputSchema() json.RawMessage { return searchInFilesSchema }
func (t *SearchInFiles) Description() string {
	return "Workspace-scoped regex content search across file contents. " +
		"For symbol name lookups (finding a function, type, or variable by name), prefer workspace_symbols — " +
		"it uses the LSP index and returns results instantly. " +
		"Use search_in_files for content patterns, string literals, comments, or arbitrary text. " +
		"Prefer this over shelling out to grep/rg: " +
		"results are confined to the active project (no .git/, node_modules/, build artefacts, or anything else .gitignore excludes), " +
		"binary files are skipped (null-byte sniff of the first 8 KB), files larger than max_file_bytes (50 MiB default) are skipped before opening, " +
		"globs with a literal directory prefix (e.g. \"src/**/*.go\") prune sibling directories from the walk. " +
		"Smart-case (case-insensitive when the pattern is all lowercase), supports context lines and glob file filters."
}

type searchInFilesArgs struct {
	Pattern                string   `json:"pattern"`
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

func (t *SearchInFiles) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a searchInFilesArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("search_in_files: invalid arguments: %w", err)
	}
	if a.Pattern == "" {
		return "", fmt.Errorf("search_in_files: pattern must not be empty")
	}

	// If the caller hasn't bounded the call, apply a default wall-clock budget
	// so a pathological walk (huge tree, large files, $HOME as cwd) can never
	// outlive the MCP client's own timeout and leave the daemon wedged.
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, searchDefaultDeadline)
		defer cancel()
	}

	// Resolve search root.
	root := resolvePath(a.Path, t.ws)
	info, err := os.Stat(root)
	if err != nil {
		return "", fmt.Errorf("search_in_files: path %q: %w", root, err)
	}
	if !info.IsDir() {
		root = filepath.Dir(root)
	}

	// Defaults.
	if a.MaxResults <= 0 {
		a.MaxResults = 200
	}
	if a.ContextLines < 0 {
		a.ContextLines = 0
	}
	if a.MaxFileBytes == 0 {
		a.MaxFileBytes = searchDefaultMaxFileBytes
	}

	// Compile regex with smart-case.
	caseSensitive := a.CaseSensitive != nil && *a.CaseSensitive
	if !caseSensitive && !allLower(a.Pattern) {
		caseSensitive = true
	}
	reStr := a.Pattern
	if !caseSensitive {
		reStr = "(?i)" + reStr
	}
	re, err := regexp.Compile(reStr)
	if err != nil {
		return "", fmt.Errorf("search_in_files: invalid pattern %q: %w", a.Pattern, err)
	}

	type fileMatch struct {
		relPath      string
		absPath      string   // absolute path, used for LSP URI when include_enclosing_symbol is set
		lines        []string // formatted "LINE: content" entries
		hitLineNums  []int    // 1-based line numbers of actual match lines (parallel to hits)
		hits         int      // raw hit count, used for max_results truncation
		skippedLines int
	}

	opts := walkOptions{
		root:          root,
		includeHidden: a.IncludeHidden,
		respectIgnore: true,
	}

	globPrefix := globLiteralPrefix(a.Glob)

	// Phase 1: collect candidate file paths via a single-threaded walk. The
	// walk is cheap; the per-file scan is the bottleneck and gets fanned out
	// to a worker pool below.
	type pathPair struct{ abs, rel string }
	var paths []pathPair

	walkErr := walk(ctx, opts, func(path string, d fs.DirEntry, _ int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && (globPrefix != "" || len(a.Exclude) > 0) {
				rel, _ := filepath.Rel(root, path)
				relSlash := filepath.ToSlash(rel)
				if globPrefix != "" && !dirCompatibleWithPrefix(relSlash, globPrefix) {
					return fs.SkipDir
				}
				for _, excl := range a.Exclude {
					if m, _ := doubleStarMatchFile(excl, relSlash); m {
						return fs.SkipDir
					}
				}
			}
			return nil
		}

		// Glob filter.
		if a.Glob != "" {
			matched, err := filepath.Match(a.Glob, d.Name())
			if err != nil || !matched {
				// Also try matching against relative path for patterns like **/*.go.
				rel, _ := filepath.Rel(root, path)
				matched2, _ := doubleStarMatchFile(a.Glob, filepath.ToSlash(rel))
				if !matched2 {
					return nil
				}
			}
		}

		// Exclude filter.
		if len(a.Exclude) > 0 {
			rel, _ := filepath.Rel(root, path)
			relSlash := filepath.ToSlash(rel)
			for _, excl := range a.Exclude {
				if m, _ := doubleStarMatchFile(excl, relSlash); m {
					return nil
				}
			}
		}

		// Size guard: skip outsized text files before opening. Walking past
		// these is the dominant pathology that pushes search past the MCP
		// timeout.
		if fi, err := d.Info(); err == nil && fi.Size() > a.MaxFileBytes {
			return nil
		}

		rel, _ := filepath.Rel(root, path)
		paths = append(paths, pathPair{abs: path, rel: filepath.ToSlash(rel)})
		return nil
	})

	// Phase 2: scan candidate files in parallel. Each file is streamed
	// line-by-line via bufio.Scanner so we never materialise the whole file
	// in memory — the previous bytes.Buffer approach was O(file_size) per worker.
	scan := func(p pathPair) *fileMatch {
		f, err := os.Open(p.abs)
		if err != nil {
			return nil
		}
		defer f.Close()

		// Sniff first 8 KB for a null byte; bail before reading the rest.
		sniff := make([]byte, binarySniffBytes)
		n, _ := f.Read(sniff)
		if bytes.IndexByte(sniff[:n], 0) >= 0 {
			return nil
		}

		// Prepend the sniffed bytes via MultiReader to avoid seeking.
		// Use a custom split function (closure) so oversized lines are skipped
		// in-place rather than aborting the scan via ErrTooLong.
		var hitLineIdxs []int
		var lines []searchLine
		lineNo := 1
		skippedLines := 0

		scanner := bufio.NewScanner(io.MultiReader(bytes.NewReader(sniff[:n]), f))
		scanner.Buffer(make([]byte, 64*1024), 2*searchMaxLineBytes)
		scanner.Split(func(data []byte, atEOF bool) (advance int, token []byte, err error) {
			if atEOF && len(data) == 0 {
				return 0, nil, nil
			}
			if i := bytes.IndexByte(data, '\n'); i >= 0 {
				line := data[:i]
				if len(line) > 0 && line[len(line)-1] == '\r' {
					line = line[:len(line)-1]
				}
				if len(line) > searchMaxLineBytes {
					skippedLines++ // oversized: advance past the newline, return no token
					lineNo++
					return i + 1, nil, nil
				}
				return i + 1, line, nil
			}
			if atEOF {
				line := data
				if len(line) > 0 && line[len(line)-1] == '\r' {
					line = line[:len(line)-1]
				}
				if len(line) > searchMaxLineBytes {
					skippedLines++
					return len(data), nil, nil
				}
				return len(data), line, nil
			}
			return 0, nil, nil
		})

		for scanner.Scan() {
			data := scanner.Bytes()
			cp := make([]byte, len(data))
			copy(cp, data)
			idx := len(lines)
			lines = append(lines, searchLine{number: lineNo, data: cp})
			if re.Match(cp) {
				hitLineIdxs = append(hitLineIdxs, idx)
			}
			lineNo++
		}
		if scanner.Err() != nil {
			// A line exceeded 2*searchMaxLineBytes; everything up to it has been
			// processed; count the scanner abort as one more skipped line.
			skippedLines++
		}

		if len(hitLineIdxs) == 0 {
			if skippedLines > 0 {
				return &fileMatch{relPath: p.rel, skippedLines: skippedLines}
			}
			return nil
		}

		// Format hits with optional context, merging overlapping windows.
		var formatted []string
		shown := make(map[int]bool)
		for _, h := range hitLineIdxs {
			lo := max(0, h-a.ContextLines)
			hi := min(len(lines)-1, h+a.ContextLines)
			for i := lo; i <= hi; i++ {
				if shown[i] {
					continue
				}
				shown[i] = true
				prefix := "  "
				if i == h {
					prefix = "> " // mark the actual match line
				}
				formatted = append(formatted,
					fmt.Sprintf("  %d:%s%s", lines[i].number, prefix, strings.TrimRight(string(lines[i].data), "\r")))
			}
		}
		hitNums := make([]int, len(hitLineIdxs))
		for i, h := range hitLineIdxs {
			hitNums[i] = lines[h].number
		}
		return &fileMatch{
			relPath: p.rel, absPath: p.abs,
			lines: formatted, hitLineNums: hitNums,
			hits: len(hitLineIdxs), skippedLines: skippedLines,
		}
	}

	wctx, cancel := context.WithCancel(ctx)
	defer cancel()

	workers := max(1, min(runtime.NumCPU(), len(paths)))

	pathsCh := make(chan pathPair)
	resultsCh := make(chan *fileMatch, workers)

	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for p := range pathsCh {
				if wctx.Err() != nil {
					continue
				}
				if r := scan(p); r != nil {
					select {
					case resultsCh <- r:
					case <-wctx.Done():
						return
					}
				}
			}
		})
	}

	go func() {
		defer close(pathsCh)
		for _, p := range paths {
			select {
			case <-wctx.Done():
				return
			case pathsCh <- p:
			}
		}
	}()

	go func() {
		wg.Wait()
		close(resultsCh)
	}()

	var results []*fileMatch
	totalLines := 0
	totalSkippedLines := 0
	truncated := false
	for r := range resultsCh {
		totalSkippedLines += r.skippedLines
		if r.hits == 0 {
			continue
		}
		if truncated {
			continue // drain remaining sends so workers can exit
		}
		results = append(results, r)
		totalLines += r.hits
		if totalLines >= a.MaxResults {
			truncated = true
			cancel()
		}
	}

	sort.Slice(results, func(i, j int) bool { return results[i].relPath < results[j].relPath })

	timedOut := errors.Is(walkErr, context.DeadlineExceeded)
	cancelled := errors.Is(walkErr, context.Canceled)

	if len(results) == 0 {
		if timedOut {
			return fmt.Sprintf("Search for %q timed out before any matches were found (budget %s — narrow with path/glob, or set a tighter pattern).", a.Pattern, searchDefaultDeadline), nil
		}
		if cancelled {
			return "", walkErr
		}
		return fmt.Sprintf("No matches for %q.", a.Pattern), nil
	}

	// Phase 3: optional per-file symbol annotation via LSP.
	// One DocumentSymbols call per distinct matched file; results cached.
	var fileAnnotations map[string]map[int]string // absPath → (1-based line → symbol name)
	if a.IncludeEnclosingSymbol && t.client != nil {
		fileAnnotations = make(map[string]map[int]string)
		for _, fm := range results {
			uri := protocol.FileURI(fm.absPath)
			syms := t.docSymbolsCached(ctx, uri)
			if len(syms) == 0 {
				continue
			}
			m := make(map[int]string, len(fm.hitLineNums))
			for _, lineNo := range fm.hitLineNums {
				if sym := deepestEnclosingSymbol(syms, uint32(lineNo-1)); sym != "" {
					m[lineNo] = sym
				}
			}
			if len(m) > 0 {
				fileAnnotations[fm.absPath] = m
			}
		}
	}

	var sb strings.Builder
	for _, fm := range results {
		sb.WriteString(fm.relPath)
		sb.WriteByte('\n')
		ann := fileAnnotations[fm.absPath] // nil when feature off or no symbols
		hitIdx := 0
		for _, l := range fm.lines {
			sb.WriteString(l)
			sb.WriteByte('\n')
			// After a hit line (marker ":> "), append the enclosing symbol.
			if ann != nil && strings.Contains(l, ":> ") && hitIdx < len(fm.hitLineNums) {
				lineNo := fm.hitLineNums[hitIdx]
				hitIdx++
				if name, ok := ann[lineNo]; ok {
					fmt.Fprintf(&sb, "  [in: %s]\n", name)
				}
			}
		}
		sb.WriteByte('\n')
	}

	var summary string
	switch {
	case timedOut:
		summary = fmt.Sprintf("Showing %d hit(s) across %d file(s) — partial (search timed out after %s; narrow with path/glob or set a tighter pattern).", totalLines, len(results), searchDefaultDeadline)
	case truncated:
		summary = fmt.Sprintf("Showing first %d hit(s) across %d file(s) — limit reached (pass max_results=N to raise, or narrow with glob/path/pattern).", a.MaxResults, len(results))
	default:
		summary = fmt.Sprintf("%d hit(s) across %d file(s).", totalLines, len(results))
	}
	if totalSkippedLines > 0 {
		summary += fmt.Sprintf(" (%d oversized line(s) skipped)", totalSkippedLines)
	}
	sb.WriteString(summary)
	return sb.String(), nil
}

// allLower reports whether s contains no uppercase Unicode letters (smart-case).
func allLower(s string) bool {
	for _, r := range s {
		if unicode.IsUpper(r) {
			return false
		}
	}
	return true
}

type searchLine struct {
	number int
	data   []byte
}

// doubleStarMatchFile matches a glob that may contain ** against a slash-separated path.
func doubleStarMatchFile(glob, path string) (bool, error) {
	// Try base name first for simple globs.
	base := path
	if idx := strings.LastIndex(path, "/"); idx >= 0 {
		base = path[idx+1:]
	}
	if m, err := filepath.Match(glob, base); m || err != nil {
		return m, err
	}
	return doubleStarMatch(glob, path), nil
}

// docSymbolsCached returns DocumentSymbols for uri, consulting t.symCache first.
// Returns nil when the LSP call fails; callers treat nil as "no annotation".
func (t *SearchInFiles) docSymbolsCached(ctx context.Context, uri string) []protocol.DocumentSymbol {
	key := uri + ":docSymbols"
	if t.symCache != nil {
		if v, ok := t.symCache.Get(key); ok {
			return v.([]protocol.DocumentSymbol)
		}
	}
	syms, err := t.client.DocumentSymbols(ctx, protocol.DocumentSymbolParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri},
	})
	if err != nil {
		return nil
	}
	if t.symCache != nil {
		t.symCache.Set(key, syms, t.cacheTTL)
	}
	return syms
}

// deepestEnclosingSymbol returns "Name (kind)" for the innermost symbol whose
// range contains the given 0-based line number, or "" when none matches.
func deepestEnclosingSymbol(syms []protocol.DocumentSymbol, line uint32) string {
	best := ""
	bestSize := uint32(0)
	var walk func([]protocol.DocumentSymbol, uint32)
	walk = func(ss []protocol.DocumentSymbol, depth uint32) {
		for _, s := range ss {
			if s.Range.Start.Line > line || s.Range.End.Line < line {
				continue
			}
			size := s.Range.End.Line - s.Range.Start.Line
			if best == "" || size < bestSize || (size == bestSize && depth > 0) {
				best = fmt.Sprintf("%s (%s)", s.Name, symbolKindName(s.Kind))
				bestSize = size
			}
			walk(s.Children, depth+1)
		}
	}
	walk(syms, 0)
	return best
}
