package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

var readFileSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {
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
    }
  },
  "required": ["path"]
}`)

const maxReadFileBytes = 200 * 1024 // 200 KiB

// ReadFile reads a file and returns its contents as text.
// Supports line-range slicing for large files (streamed — only the requested
// lines are read into memory).
//
// Output begins with a header line carrying the file's mtime in RFC3339Nano:
//
//	# plumb-read mtime=2026-05-11T13:46:38.895137000+10:00
//
// Subsequent edit_file calls may pass this value as expected_mtime to assert
// the file has not changed between read and edit. The header is followed by
// a blank line, then the content (or selected line range).
//
// If a non-nil ReadTracker is supplied, every successful read records the
// observed mtime so edit_file's strict mode can verify the agent did read
// the file before editing it.
//
// Concurrency: Execute is safe for concurrent use.
type ReadFile struct {
	tracker *ReadTracker // may be nil; strict-mode tracking disabled when nil
}

func NewReadFile(tracker *ReadTracker) *ReadFile { return &ReadFile{tracker: tracker} }

func (t *ReadFile) Name() string               { return "read_file" }
func (t *ReadFile) InputSchema() json.RawMessage { return readFileSchema }
func (t *ReadFile) Description() string {
	return "Read the text contents of a file. Accepts an absolute path or a file:// URI. " +
		"Use start_line and end_line to read a slice of a large file without loading it entirely " +
		"(only the requested lines are streamed into memory). " +
		"Binary files are detected and rejected. Output is capped at 200 KiB — use line ranges on large files. " +
		"The output begins with a header carrying the file's mtime (RFC3339Nano); pass that " +
		"value back as expected_mtime to edit_file for optimistic-concurrency guarantees. " +
		"Essential for clients without filesystem access of their own (Claude Desktop, Cursor MCP, etc.)."
}

type readFileArgs struct {
	Path      string `json:"path"`
	StartLine *int   `json:"start_line"`
	EndLine   *int   `json:"end_line"`
}

func (t *ReadFile) Execute(_ context.Context, raw json.RawMessage) (string, error) {
	var a readFileArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("read_file: invalid arguments: %w", err)
	}
	if a.Path == "" {
		return "", fmt.Errorf("read_file: path is required")
	}

	// Accept both file:// URIs and plain paths.
	fpath := strings.TrimPrefix(a.Path, "file://")

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

	content, err := readContentMaybeRanged(src, a.StartLine, a.EndLine)
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

	var sb strings.Builder
	fmt.Fprintf(&sb, "# plumb-read mtime=%s indent=%s\n\n", mtime.Format(time.RFC3339Nano), classifyIndent(content))
	sb.WriteString(content)
	if truncated {
		sb.WriteString("\n… (output truncated at 200 KiB — use start_line/end_line to read specific sections)")
	}
	return sb.String(), nil
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

	start := 1
	if startLine != nil && *startLine > 1 {
		start = *startLine
	}
	end := -1 // unbounded
	if endLine != nil {
		end = *endLine
	}
	if end >= 0 && start > end {
		return fmt.Sprintf("(no lines in range %d–%d)", start, end), nil
	}

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
