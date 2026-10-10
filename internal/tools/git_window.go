package tools

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// git_window.go owns the read-tier output window (PLAN-454): the start_line,
// end_line and pattern arguments that let a caller ask a read command a
// line-shaped question instead of receiving its whole output.
//
// It exists because plumb's read tier has no shell to pipe through, while some
// read commands answer with far more than a response can carry: `show
// <rev>:<path>` on a large file, a `diff` of a wide refactor, a `log` over a
// year of history. Before this the only choices were "truncated at 100 KiB" —
// where the lines the caller wanted are simply gone, which is exactly what
// sent a reviewer to a native shell fallback — or asking git for something
// coarser. The window is applied to the child's FULL output, ahead of the
// response caps in formatGitOutput, so a range deep inside a large blob is
// answered exactly instead of being truncated away first.
//
// The semantics are read_file's, deliberately: literal text unless use_regex,
// smart-case unless case_sensitive says otherwise, 1-based inclusive lines, and
// a trailing note that says which lines of how many were returned. A caller
// that has learned one tool's window has learned this one's.
//
// A zero gitWindow is no window: the output is returned whole, as before, and
// every existing call is byte-for-byte unchanged.

// maxGitWindowMatches caps a pattern window's match list. A pattern that matches
// tens of thousands of lines would otherwise be cut mid-line by the response
// byte cap; a counted, labelled cut is more useful than a torn one.
const maxGitWindowMatches = 200

// gitWindow is one read call's window request, derived from gitToolArgs.
type gitWindow struct {
	// Start and End are the 1-based inclusive line range; nil means unbounded on
	// that side (the first line, the last line). With Pattern set they narrow the
	// region searched rather than selecting a contiguous slice.
	Start, End *int
	// Pattern selects matching lines instead of a contiguous range: literal text
	// unless UseRegex, with read_file's smart-case rule and an explicit
	// CaseSensitive winning over it, exactly as compileToolPattern applies them.
	Pattern       string
	UseRegex      bool
	CaseSensitive *bool
}

// wanted reports whether the caller asked for a window at all. A zero window
// takes no code path and changes no output.
func (w gitWindow) wanted() bool {
	return w.Start != nil || w.End != nil || w.Pattern != ""
}

// windowGitArgs reads the window a call asked for and fails the argument errors
// that must be refused before git runs: a window is a caller error when it is
// malformed, never something to discover from git's output.
//
// start_line > end_line is refused rather than answered with an empty range:
// read_file tolerates it only because its offset/limit algebra can produce it
// from a single `limit`, and a git caller that swapped two numbers wants to be
// told, not handed a placeholder that reads like an empty file.
func windowGitArgs(a gitToolArgs) (gitWindow, error) {
	w := gitWindow{
		Start:         a.StartLine,
		End:           a.EndLine,
		Pattern:       a.Pattern,
		UseRegex:      a.UseRegex,
		CaseSensitive: a.CaseSensitive,
	}
	if !w.wanted() {
		if w.UseRegex || w.CaseSensitive != nil {
			return gitWindow{}, errors.New("git: use_regex and case_sensitive qualify pattern, which is empty")
		}
		return w, nil
	}
	if w.Start != nil && *w.Start < 1 {
		return gitWindow{}, errors.New("git: start_line must be >= 1 (lines are 1-based)")
	}
	if w.End != nil && *w.End < 1 {
		return gitWindow{}, errors.New("git: end_line must be >= 1 (lines are 1-based)")
	}
	if w.Start != nil && w.End != nil && *w.End < *w.Start {
		return gitWindow{}, fmt.Errorf("git: end_line (%d) is before start_line (%d)", *w.End, *w.Start)
	}
	return w, nil
}

// applyGitWindow returns the requested slice of out, with a trailing note
// naming what was returned. out is the command's whole output — the window is
// applied before the response caps, never after.
//
// A window that selects nothing is a normal answer, not an error: it comes back
// as a placeholder in the same shape read_file uses, plus the literal/regex hint
// when the pattern carried regex syntax that use_regex:false quoted.
func applyGitWindow(out string, w gitWindow) (string, error) {
	if !w.wanted() {
		return out, nil
	}
	lines := outputLines(out)
	total := len(lines)
	if w.Pattern != "" {
		return windowByPattern(lines, w)
	}
	return windowByRange(lines, w, total), nil
}

// outputLines splits command output into lines without inventing a final empty
// line: git output ends with a newline, and a range that counted that trailing
// newline as line total+1 would be off by one against every editor.
func outputLines(out string) []string {
	if out == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(out, "\n"), "\n")
}

// windowByRange slices the 1-based inclusive [start, end] range and labels it.
// An empty result names the range and how many lines the output actually had:
// silence there reads as "the command printed nothing", which is a different
// fact from "your range was past the end".
func windowByRange(lines []string, w gitWindow, total int) string {
	start := 1
	if w.Start != nil {
		start = *w.Start
	}
	end := total
	if w.End != nil && *w.End < end {
		end = *w.End
	}
	if start > total {
		// The requested end is kept as asked (an unbounded one reads as EOF), the
		// way read_file words the same answer: "9–12" tells a caller what they
		// asked for, where a clamped "9–5" reads like a bug.
		endLabel := "EOF"
		if w.End != nil {
			endLabel = strconv.Itoa(*w.End)
		}
		return fmt.Sprintf("(no lines in range %d–%s; the output has %d lines)", start, endLabel, total)
	}
	body := strings.Join(lines[start-1:end], "\n") + "\n"
	return body + rangeNote(start, end, total)
}

// rangeNote is the trailing label for a sliced range, and says "of N" only when
// the slice really is a slice of something larger — otherwise the note would be
// noise on a request that asked for exactly what the command printed.
func rangeNote(start, end, total int) string {
	if start == 1 && end == total {
		return fmt.Sprintf("… (all %d lines)", total)
	}
	return fmt.Sprintf("… (lines %d–%d of %d)", start, end, total)
}

// windowByPattern selects the lines matching w.Pattern within the (optional)
// line range, rendering them exactly as read_file's search mode does — the same
// right-aligned line-number gutter, the same "--" between non-adjacent matches,
// so the two tools' answers are read the same way.
func windowByPattern(lines []string, w gitWindow) (string, error) {
	re, err := compileToolPattern("git", w.Pattern, w.UseRegex, w.CaseSensitive)
	if err != nil {
		return "", err
	}
	start := 1
	if w.Start != nil {
		start = *w.Start
	}
	end := len(lines)
	if w.End != nil && *w.End < end {
		end = *w.End
	}
	var matches []matchLine
	scanned := 0
	for i := start; i <= end && i <= len(lines); i++ {
		scanned++
		if !re.MatchString(lines[i-1]) {
			continue
		}
		if len(matches) == maxGitWindowMatches {
			break
		}
		matches = append(matches, matchLine{lineNo: i, text: lines[i-1]})
	}
	if len(matches) == 0 {
		return fmt.Sprintf("(no lines matched %q in lines %d–%d of %d)",
			w.Pattern, start, end, len(lines)) + literalRegexHint(w.Pattern, w.UseRegex, false), nil
	}
	body := renderSearchBody(matches)
	note := fmt.Sprintf("… (%d matching line(s), %d scanned)", len(matches), scanned)
	if len(matches) == maxGitWindowMatches {
		note = fmt.Sprintf("… (first %d matching lines, %d scanned — narrow with start_line/end_line)",
			maxGitWindowMatches, scanned)
	}
	return body + note + literalRegexHint(w.Pattern, w.UseRegex, true), nil
}
