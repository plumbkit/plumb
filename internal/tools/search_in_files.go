package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
)

// searchDefaultDeadline caps any single search_in_files call when the parent
// context has no deadline. Prevents a runaway walk (e.g. workspace resolved to
// $HOME, or a giant text file dragging on) from hanging the daemon past the
// MCP client's own timeout — which would otherwise leave a wedged goroutine
// behind that the user can't cancel.
const searchDefaultDeadline = 30 * time.Second

// searchMaxLineBytes raises bufio.Scanner's per-line cap (default 64 KB) so
// minified or generated single-line files don't silently truncate. Lines
// longer than this are still skipped, but we won't drop the rest of the file.
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
    }
  },
  "required": ["pattern"]
}`)

// SearchInFiles implements grep-like search across workspace files.
type SearchInFiles struct{}

func NewSearchInFiles() *SearchInFiles { return &SearchInFiles{} }

func (t *SearchInFiles) Name() string             { return "search_in_files" }
func (t *SearchInFiles) InputSchema() json.RawMessage { return searchInFilesSchema }
func (t *SearchInFiles) Description() string {
	return "Workspace-scoped regex content search. Prefer this over shelling out to grep/rg: " +
		"results are confined to the active project (no .git/, node_modules/, build artefacts, or anything else .gitignore excludes), " +
		"binary files are skipped (null-byte sniff of the first 8 KB), files larger than max_file_bytes (50 MiB default) are skipped before opening, " +
		"globs with a literal directory prefix (e.g. \"src/**/*.go\") prune sibling directories from the walk, " +
		"every call is recorded in the project's stats so you can see what's been searched, " +
		"and the cache layer dedupes repeat queries within a session. " +
		"Smart-case (case-insensitive when the pattern is all lowercase), supports context lines and glob file filters."
}

type searchInFilesArgs struct {
	Pattern       string `json:"pattern"`
	Path          string `json:"path"`
	Glob          string `json:"glob"`
	CaseSensitive *bool  `json:"case_sensitive"`
	ContextLines  int    `json:"context_lines"`
	MaxResults    int    `json:"max_results"`
	IncludeHidden bool   `json:"include_hidden"`
	MaxFileBytes  int64  `json:"max_file_bytes"`
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
	root := strings.TrimPrefix(a.Path, "file://")
	if root == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("search_in_files: getting cwd: %w", err)
		}
		root = cwd
	}
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
		relPath string
		lines   []string // formatted "LINE: content" entries
		hits    int      // raw hit count, used for max_results truncation
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
			if globPrefix != "" && path != root {
				rel, _ := filepath.Rel(root, path)
				if !dirCompatibleWithPrefix(filepath.ToSlash(rel), globPrefix) {
					return fs.SkipDir
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

	// Phase 2: scan candidate files in parallel.
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
		var buf bytes.Buffer
		buf.Write(sniff[:n])
		if _, err := buf.ReadFrom(f); err != nil {
			return nil
		}

		lines := splitLines(buf.Bytes())
		var hitLines []int
		for i, line := range lines {
			if re.Match(line) {
				hitLines = append(hitLines, i)
			}
		}
		if len(hitLines) == 0 {
			return nil
		}

		// Format hits with optional context, merging overlapping windows.
		var formatted []string
		shown := make(map[int]bool)
		for _, h := range hitLines {
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
					fmt.Sprintf("  %d:%s%s", i+1, prefix, strings.TrimRight(string(lines[i]), "\r")))
			}
		}
		return &fileMatch{relPath: p.rel, lines: formatted, hits: len(hitLines)}
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
	truncated := false
	for r := range resultsCh {
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

	var sb strings.Builder
	for _, fm := range results {
		sb.WriteString(fm.relPath)
		sb.WriteByte('\n')
		for _, l := range fm.lines {
			sb.WriteString(l)
			sb.WriteByte('\n')
		}
		sb.WriteByte('\n')
	}

	summary := fmt.Sprintf("%d file(s) matched, %d hits", len(results), totalLines)
	switch {
	case timedOut:
		summary += fmt.Sprintf(" (partial — search timed out after %s; narrow with path/glob)", searchDefaultDeadline)
	case truncated:
		summary += fmt.Sprintf(" (truncated past %d hits — narrow with glob or a tighter pattern)", a.MaxResults)
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

// splitLines splits b into lines, preserving the newline character for each line.
func splitLines(b []byte) [][]byte {
	var lines [][]byte
	sc := bufio.NewScanner(bytes.NewReader(b))
	// Raise the per-line cap from bufio's 64 KB default so generated/minified
	// single-line files don't get silently chopped mid-scan.
	sc.Buffer(make([]byte, 64*1024), searchMaxLineBytes)
	for sc.Scan() {
		cp := make([]byte, len(sc.Bytes()))
		copy(cp, sc.Bytes())
		lines = append(lines, cp)
	}
	return lines
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

