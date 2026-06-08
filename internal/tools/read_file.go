package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

var readFileSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "file_path": {
      "type": "string",
      "description": "Absolute path or file:// URI of the file to read."
    },
    "start_line": {
      "type": "integer",
      "description": "First line to return (1-based, inclusive). Omit to start from the beginning.",
      "minimum": 1
    },
    "end_line": {
      "type": "integer",
      "description": "Last line to return (1-based, inclusive). Omit to read to the end of the file.",
      "minimum": 1
    },
    "offset": {
      "type": "integer",
      "description": "First line to read, 1-based (Claude Code-style alias for start_line; start_line wins if both are given).",
      "minimum": 1
    },
    "limit": {
      "type": "integer",
      "description": "Number of lines to return starting at the first line (Claude Code-style window; first line defaults to 1). Mutually exclusive with end_line.",
      "minimum": 1
    }
  },
  "required": ["file_path"],
  "additionalProperties": false
}`)

const maxReadFileBytes = 200 * 1024 // 200 KiB

// ReadFile reads a file and returns its contents as text.
// Supports line-range slicing for large files (streamed — only the requested
// lines are read into memory).
//
// Output begins with a header line carrying the file's mtime and SHA-256:
//
//	# plumb-read mtime=2026-05-11T13:46:38.895137000+10:00 sha256=3a7bd3e2…
//
// Subsequent edit_file calls may pass either value as expected_mtime or
// expected_sha to guard against concurrent modifications. The header is
// followed by a blank line, then the content (or selected line range).
// sha256 is computed over the full file, not the sliced excerpt.
//
// If a non-nil ReadTracker is supplied, every successful read records the
// observed mtime so edit_file's strict mode can verify the agent did read
// the file before editing it.
//
// Concurrency: Execute is safe for concurrent use.
type ReadFile struct {
	tracker      *ReadTracker // may be nil; strict-mode tracking disabled when nil
	guard        BoundaryGuard
	clientNameFn func() string       // may be nil; gates the edit-lane hint to conflict-prone clients
	outsideFn    func(string) string // may be nil; returns a root label when the path is outside the workspace
}

func NewReadFile(tracker *ReadTracker) *ReadFile { return &ReadFile{tracker: tracker} }

func (t *ReadFile) WithBoundary(guard BoundaryGuard) *ReadFile {
	t.guard = guard
	return t
}

// WithClient wires the MCP client-name accessor so read_file can append the
// edit-lane hint only for clients whose native Edit tool conflicts with plumb's
// read-state (see edit_lane.go). Nil-safe; without it no hint is emitted.
func (t *ReadFile) WithClient(fn func() string) *ReadFile {
	t.clientNameFn = fn
	return t
}

// WithOutsideLabel wires an accessor that, given a resolved path, returns the
// allowed-root label when the path lies outside the workspace (a read-only
// dependency or configured read root), or "" when inside it. read_file uses it
// to annotate out-of-workspace reads so the agent knows the content is not
// editable. Nil-safe.
func (t *ReadFile) WithOutsideLabel(fn func(string) string) *ReadFile {
	t.outsideFn = fn
	return t
}

func (t *ReadFile) outsideLabel(path string) string {
	if t.outsideFn == nil {
		return ""
	}
	return t.outsideFn(path)
}

func (t *ReadFile) Name() string                 { return "read_file" }
func (t *ReadFile) InputSchema() json.RawMessage { return readFileSchema }
func (t *ReadFile) Description() string {
	return "Read the text contents of a file. Accepts an absolute path or a file:// URI. " +
		"Use start_line and end_line to read a slice of a large file without loading it entirely " +
		"(only the requested lines are streamed into memory). " +
		"Binary files are detected and rejected. Output is capped at 200 KiB — use line ranges on large files. " +
		"The output begins with a header carrying the file's mtime (RFC3339Nano) and SHA-256 hash. " +
		"Pass mtime back as expected_mtime to edit_file for fast optimistic-concurrency checks; " +
		"pass the hash as expected_sha for content-based checks that survive mtime aliasing. " +
		"Essential for clients without filesystem access of their own (Claude Desktop, Cursor MCP, etc.)."
}

type readFileArgs struct {
	Path      string `json:"file_path"`
	StartLine *int   `json:"start_line"`
	EndLine   *int   `json:"end_line"`
	Offset    *int   `json:"offset"`
	Limit     *int   `json:"limit"`
}

func (t *ReadFile) Execute(_ context.Context, raw json.RawMessage) (string, error) {
	var a readFileArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("read_file: invalid arguments: %w", err)
	}
	if a.Path == "" {
		return "", fmt.Errorf("read_file: file_path is required")
	}

	// Accept both file:// URIs and plain paths.
	fpath := strings.TrimPrefix(a.Path, "file://")
	if err := t.guard.check(fpath); err != nil {
		return "", fmt.Errorf("read_file: %w", err)
	}

	info, err := os.Stat(fpath)
	if err != nil {
		return "", fmt.Errorf("read_file: %w", err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("read_file: %q is a directory — use list_files to browse directories", fpath)
	}
	mtime := info.ModTime()
	t.tracker.Record(fpath, mtime)

	f, err := os.Open(fpath)
	if err != nil {
		return "", fmt.Errorf("read_file: %w", err)
	}
	defer f.Close()

	// Sniff up to binarySniffBytes for null bytes. We hand the prefix bytes
	// back into the read path via io.MultiReader so no Seek is needed — Seek
	// fails on pipes/devices and is wasted work on regular files.
	sniff := make([]byte, binarySniffBytes)
	n, _ := io.ReadFull(f, sniff)
	sniff = sniff[:n]
	if bytes.IndexByte(sniff, 0) >= 0 {
		return "", fmt.Errorf("read_file: %q appears to be a binary file", fpath)
	}
	src := io.MultiReader(bytes.NewReader(sniff), f)

	start, end, err := resolveLineWindow(a)
	if err != nil {
		return "", fmt.Errorf("read_file: %w", err)
	}
	content, err := readContentMaybeRanged(src, start, end)
	if err != nil {
		return "", fmt.Errorf("read_file: %w", err)
	}

	truncated := false
	if len(content) > maxReadFileBytes {
		content = content[:maxReadFileBytes]
		if idx := strings.LastIndex(content, "\n"); idx > 0 {
			content = content[:idx]
		}
		truncated = true
	}

	sha, err := fileSHA256(fpath)
	if err != nil {
		slog.Warn("read_file: computing sha256", "path", fpath, "err", err)
	}

	return t.formatOutput(mtime, sha, content, truncated, t.outsideLabel(fpath)), nil
}

// formatOutput assembles the read_file response: the plumb-read header line
// (mtime + optional sha + indent), an optional edit-lane hint line for clients
// whose native Edit tool conflicts with plumb's read-state, a blank separator,
// then the (possibly truncated) content.
func (t *ReadFile) formatOutput(mtime time.Time, sha, content string, truncated bool, outsideLabel string) string {
	var sb strings.Builder
	mtimeStr := mtime.Format(time.RFC3339Nano)
	// lines/chars describe the body actually returned (a ranged read reflects the
	// slice). chars is rune count, not bytes — context-window limits are
	// character-denominated and bytes mislead for any non-ASCII text
	// (internal/feedbacks.md 2026-06-08).
	lines, chars := displayLineCount(content), utf8.RuneCountInString(content)
	if sha != "" {
		fmt.Fprintf(&sb, "# plumb-read mtime=%s sha256=%s indent=%s lines=%d chars=%d\n", mtimeStr, sha, classifyIndent(content), lines, chars)
	} else {
		fmt.Fprintf(&sb, "# plumb-read mtime=%s indent=%s lines=%d chars=%d\n", mtimeStr, classifyIndent(content), lines, chars)
	}
	if outsideLabel != "" {
		fmt.Fprintf(&sb, "# plumb-note: read-only — outside the workspace (%s); not editable\n", outsideLabel)
	}
	// For clients whose native Edit tool conflicts with plumb's read-state
	// tracking, append a copy-paste-ready pointer to edit_file at the exact
	// moment the agent is about to act on what it just read. Suppressed for
	// out-of-workspace reads — those files are not editable, so an edit hint
	// would contradict the read-only note above.
	if outsideLabel == "" && clientHasNativeEditConflict(t.clientNameFn) {
		sb.WriteString(nativeEditReadHint(mtimeStr))
	}
	sb.WriteByte('\n')
	sb.WriteString(content)
	if truncated {
		sb.WriteString("\n… (output truncated at 200 KiB — use start_line/end_line to read specific sections)")
	}
	return sb.String()
}

// resolveLineWindow reconciles plumb's absolute start_line/end_line range with
// Claude Code's native offset/limit window into the (start, end) pair
// readContentMaybeRanged expects. offset is a synonym for start_line (start_line
// wins when both are given); limit — "N lines from the first line" — is
// translated to an absolute end_line and is mutually exclusive with end_line.
func resolveLineWindow(a readFileArgs) (start, end *int, err error) {
	start = a.StartLine
	if start == nil {
		start = a.Offset
	}
	if a.Limit == nil {
		return start, a.EndLine, nil
	}
	if a.EndLine != nil {
		return nil, nil, fmt.Errorf("specify end_line or limit, not both")
	}
	if *a.Limit < 1 {
		return nil, nil, fmt.Errorf("limit must be >= 1")
	}
	s := 1
	if start != nil {
		s = *start
	}
	e := s + *a.Limit - 1
	return &s, &e, nil
}

// classifyIndent inspects the leading whitespace of each non-empty line in
// content and returns one of "tabs", "spaces", "mixed", or "none". The
// classification is over the body actually returned (so a line-ranged read
// reflects the slice the agent received). It exists so clients that render
// tabs as visual spaces don't leave the agent guessing what character to
// use when authoring old_str.
func classifyIndent(content string) string {
	sawTab, sawSpace := false, false
	for line := range strings.SplitSeq(content, "\n") {
		if len(line) == 0 {
			continue
		}
		switch line[0] {
		case '\t':
			sawTab = true
		case ' ':
			sawSpace = true
		}
		if sawTab && sawSpace {
			return "mixed"
		}
	}
	switch {
	case sawTab:
		return "tabs"
	case sawSpace:
		return "spaces"
	default:
		return "none"
	}
}

// readContentMaybeRanged reads either the whole stream or just the requested
// 1-based [startLine, endLine] range. When a range is given we use a bufio
// Scanner that stops at endLine — so a 50MB file with a 100-line range only
// reads ~100 lines, not the whole file.
func readContentMaybeRanged(src io.Reader, startLine, endLine *int) (string, error) {
	if startLine == nil && endLine == nil {
		data, err := io.ReadAll(src)
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
	start, end := resolveReadRange(startLine, endLine)
	if end >= 0 && start > end {
		return fmt.Sprintf("(no lines in range %d–%d)", start, end), nil
	}
	return readLineRange(src, start, end)
}

func resolveReadRange(startLine, endLine *int) (start, end int) {
	start = 1
	if startLine != nil && *startLine > 1 {
		start = *startLine
	}
	end = -1 // -1 means unbounded
	if endLine != nil {
		end = *endLine
	}
	return start, end
}

func readLineRange(src io.Reader, start, end int) (string, error) {
	scanner := bufio.NewScanner(src)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024) // up to 4 MiB per line
	var sb strings.Builder
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		if lineNo < start {
			continue
		}
		if end >= 0 && lineNo > end {
			break
		}
		if sb.Len() > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	if lineNo < start {
		endLabel := fmt.Sprintf("%d", end)
		if end < 0 {
			endLabel = "EOF"
		}
		return fmt.Sprintf("(no lines in range %d–%s; file has %d lines)", start, endLabel, lineNo), nil
	}
	return sb.String(), nil
}
