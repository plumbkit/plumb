package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

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
		"binary files are skipped, every call is recorded in the project's stats so you can see what's been searched, " +
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
}

func (t *SearchInFiles) Execute(_ context.Context, raw json.RawMessage) (string, error) {
	var a searchInFilesArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("search_in_files: invalid arguments: %w", err)
	}
	if a.Pattern == "" {
		return "", fmt.Errorf("search_in_files: pattern must not be empty")
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
	}

	var results []fileMatch
	totalLines := 0
	truncated := false

	opts := walkOptions{
		root:          root,
		includeHidden: a.IncludeHidden,
		respectIgnore: true,
	}

	_ = walk(opts, func(path string, d fs.DirEntry, _ int) error {
		if d.IsDir() {
			return nil
		}
		if truncated {
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

		// Open and binary-check.
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()

		// Read all content; detect binary on the first chunk.
		var buf bytes.Buffer
		sniff := make([]byte, binarySniffBytes)
		n, _ := f.Read(sniff)
		if bytes.IndexByte(sniff[:n], 0) >= 0 {
			return nil // binary file
		}
		buf.Write(sniff[:n])
		_, _ = buf.ReadFrom(f)
		content := buf.Bytes()

		// Scan lines and collect matches.
		lines := splitLines(content)
		type hit struct{ lineNo int }
		var hits []hit
		for i, line := range lines {
			if re.Match(line) {
				hits = append(hits, hit{i})
			}
		}
		if len(hits) == 0 {
			return nil
		}

		rel, _ := filepath.Rel(root, path)

		// Format matches with optional context, merging overlapping windows.
		var formatted []string
		shown := make(map[int]bool)
		for _, h := range hits {
			lo := max(0, h.lineNo-a.ContextLines)
			hi := min(len(lines)-1, h.lineNo+a.ContextLines)
			for i := lo; i <= hi; i++ {
				if shown[i] {
					continue
				}
				shown[i] = true
				prefix := "  "
				if i == h.lineNo {
					prefix = "> " // mark the actual match line
				}
				formatted = append(formatted,
					fmt.Sprintf("  %d:%s%s", i+1, prefix, strings.TrimRight(string(lines[i]), "\r")))
			}
			totalLines++
			if totalLines >= a.MaxResults {
				truncated = true
				break
			}
		}

		results = append(results, fileMatch{
			relPath: filepath.ToSlash(rel),
			lines:   formatted,
		})
		return nil
	})

	if len(results) == 0 {
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

	summary := fmt.Sprintf("%d file(s) matched", len(results))
	if truncated {
		summary += fmt.Sprintf(" (truncated at %d lines — narrow with glob or a tighter pattern)", a.MaxResults)
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

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
