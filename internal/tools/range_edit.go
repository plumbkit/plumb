package tools

import (
	"fmt"
	"strings"
)

// lineOffsets returns the byte offset of the start of each line, one entry per
// line (1-based index into the returned slice — line N starts at offsets[N-1]).
//
// Lines are delimited by \n. A trailing \n terminates the last line and does
// not create an empty extra entry, consistent with read_file's line numbering.
// Returns nil for an empty string.
func lineOffsets(content string) []int {
	if content == "" {
		return nil
	}
	offsets := []int{0}
	// Record the start of each line that follows a \n, but not for a \n that
	// is the very last byte (trailing newline terminates, not starts, a line).
	for i := range len(content) - 1 {
		if content[i] == '\n' {
			offsets = append(offsets, i+1)
		}
	}
	return offsets
}

// applyRangeEdit replaces lines startLine..endLine (both 1-based, inclusive)
// in content with newStr. It is an alternative to old_string matching for cases
// where a block of lines must be deleted or replaced without a unique anchor.
//
// Range mode works in whole lines: a non-empty newStr that does not end with a
// newline is given the line ending of the text it replaces, so it can never be
// glued onto the line that follows (#543). The one exception falls out of the
// same rule: a range running to EOF in a file with no final newline replaced
// unterminated text, so newStr stays unterminated and the file keeps its
// missing final newline. An empty newStr deletes the range.
//
// Special values:
//   - startLine == -1: append newStr at end of file. When content does not end
//     with a newline, the file's line ending is inserted first as a separator
//     and the file keeps its missing final newline; otherwise newStr is
//     terminated like any other range replacement.
//   - endLine == 0: defaults to startLine (single-line operation).
//   - endLine < 0 (e.g. -1): extends the range to the last line of the file.
//
// A startLine that is out of range (< 1 or > total lines) returns an error.
// endLine is silently capped at the total line count.
func applyRangeEdit(content string, startLine, endLine int, newStr string) (string, error) {
	if startLine == -1 {
		return appendLines(content, newStr), nil
	}

	offsets := lineOffsets(content)
	totalLines := len(offsets)

	if totalLines == 0 {
		if startLine == 1 {
			return newStr, nil
		}
		return "", fmt.Errorf("start_line %d out of range (file is empty)", startLine)
	}
	if startLine < 1 || startLine > totalLines {
		return "", fmt.Errorf("start_line %d out of range (file has %d line(s))", startLine, totalLines)
	}

	end := endLine
	if end == 0 {
		end = startLine
	}
	if end < 0 {
		end = totalLines
	}
	if end > totalLines {
		end = totalLines
	}
	if end < startLine {
		return "", fmt.Errorf("end_line %d must be >= start_line %d", end, startLine)
	}

	startOff := offsets[startLine-1]

	var endOff int
	if end == totalLines {
		endOff = len(content)
	} else {
		endOff = offsets[end] // byte offset of the first character of line end+1
	}

	return content[:startOff] + terminateLike(newStr, content[startOff:endOff]) + content[endOff:], nil
}

// appendLines implements applyRangeEdit's append mode (startLine == -1).
// Appending nothing leaves the file as it is, including a missing final newline.
func appendLines(content, newStr string) string {
	if content == "" || newStr == "" {
		return content + newStr
	}
	if trailingLineEnding(content) == "" {
		sep := "\n"
		if strings.Contains(content, "\r\n") {
			sep = "\r\n"
		}
		return content + sep + newStr
	}
	return content + terminateLike(newStr, content)
}

// terminateLike returns newStr ending with the same line terminator as
// replaced. An empty newStr (a deletion) and one already ending with a newline
// are returned unchanged, as is every newStr when replaced is unterminated
// (the last line of a file with no final newline).
func terminateLike(newStr, replaced string) string {
	if newStr == "" || strings.HasSuffix(newStr, "\n") {
		return newStr
	}
	return newStr + trailingLineEnding(replaced)
}

// trailingLineEnding returns the line terminator s ends with: "\r\n", "\n",
// or "" when s does not end with a newline.
func trailingLineEnding(s string) string {
	switch {
	case strings.HasSuffix(s, "\r\n"):
		return "\r\n"
	case strings.HasSuffix(s, "\n"):
		return "\n"
	}
	return ""
}
