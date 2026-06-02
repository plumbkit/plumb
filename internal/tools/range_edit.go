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
	for i := 0; i < len(content)-1; i++ {
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
// Special values:
//   - startLine == -1: append newStr at end of file. A \n separator is inserted
//     first when content does not already end with one.
//   - endLine == 0: defaults to startLine (single-line operation).
//   - endLine < 0 (e.g. -1): extends the range to the last line of the file.
//
// A startLine that is out of range (< 1 or > total lines) returns an error.
// endLine is silently capped at the total line count.
func applyRangeEdit(content string, startLine, endLine int, newStr string) (string, error) {
	if startLine == -1 {
		if len(content) > 0 && !strings.HasSuffix(content, "\n") {
			return content + "\n" + newStr, nil
		}
		return content + newStr, nil
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

	return content[:startOff] + newStr + content[endOff:], nil
}
