package tools

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// --- in-file search (pattern mode) ---------------------------------------
//
// When read_file is given a pattern it scans the whole file line-by-line and
// returns matching lines with their 1-based line numbers (and optional
// context), bounded by max_matches and a 200 KiB output cap. The file itself is
// never held in memory whole — only the emitted matches are — so a file well
// over the read cap stays searchable. This mirrors search_in_files' conventions
// (literal-by-default, smart-case) so agents carry one mental model across both.

const (
	readSearchDefaultMaxMatches = 200
	readSearchMaxMaxMatches     = 2000
	readSearchMaxContextLines   = 10
)

// matchLine is one emitted line in a search result: a match or a context line.
type matchLine struct {
	lineNo int
	text   string
}

// searchWithinFile implements pattern (grep-within-file) mode. It validates the
// search-specific arguments, restricts the scan to an optional start_line/
// end_line window, records the read (so strict mode is satisfied), and formats
// the bounded, labelled result.
func (t *ReadFile) searchWithinFile(ctx context.Context, fpath string, info os.FileInfo, mtime time.Time, concurrentNote string, a readFileArgs) (string, error) {
	if a.Limit != nil {
		return "", fmt.Errorf("read_file: pattern cannot be combined with limit — use max_matches to bound search output, and start_line/end_line to restrict the searched range")
	}
	if a.ContextLines < 0 || a.ContextLines > readSearchMaxContextLines {
		return "", fmt.Errorf("read_file: context_lines must be between 0 and %d", readSearchMaxContextLines)
	}
	maxMatches := a.MaxMatches
	if maxMatches <= 0 {
		maxMatches = readSearchDefaultMaxMatches
	}
	if maxMatches > readSearchMaxMaxMatches {
		maxMatches = readSearchMaxMaxMatches
	}

	start := 0
	if a.StartLine != nil {
		start = *a.StartLine
	} else if a.Offset != nil {
		start = *a.Offset
	}
	end := 0
	if a.EndLine != nil {
		end = *a.EndLine
	}

	re, err := compileReadFilePattern(a.Pattern, a.UseRegex, a.CaseSensitive)
	if err != nil {
		return "", err
	}

	matches, matchCount, scanned, truncated, err := scanFileMatches(ctx, fpath, re, start, end, a.ContextLines, maxMatches)
	if err != nil {
		return "", err
	}

	sha, err := fileSHA256(fpath)
	if err != nil {
		slog.Warn("read_file: computing sha256", "path", fpath, "err", err)
	}
	t.tracker.Record(fpath, mtime, sha)

	return t.formatSearchOutput(fpath, mtime, sha, info.Size(), concurrentNote, a, matches, matchCount, scanned, truncated, start, end), nil
}

// compileReadFilePattern builds the matcher for search mode: literal text by
// default (metacharacters quoted), Go RE2 regex when useRegex. Smart-case —
// case-insensitive when the pattern is all lowercase and the caller did not
// force case_sensitive. Identical semantics to search_in_files.
func compileReadFilePattern(pattern string, useRegex bool, caseSensitive *bool) (*regexp.Regexp, error) {
	cs := caseSensitive != nil && *caseSensitive
	if !cs && !allLower(pattern) {
		cs = true
	}
	flags := ""
	if !cs {
		flags = "(?i)"
	}
	if useRegex {
		re, err := regexp.Compile(flags + pattern)
		if err != nil {
			return nil, fmt.Errorf("read_file: invalid regex %q: %w", pattern, err)
		}
		return re, nil
	}
	return regexp.MustCompile(flags + regexp.QuoteMeta(pattern)), nil
}

// scanFileMatches streams fpath line-by-line and collects lines matching re,
// each with contextLines of surrounding context (like rg -C), restricted to the
// 1-based [start, end] window (0 means unbounded on that side). Collection stops
// once maxMatches matches are gathered or the assembled output would exceed the
// 200 KiB cap; either sets truncated. Returns the emitted lines (matches +
// context, in file order), the match count, the number of lines scanned, and
// whether the result was truncated.
func scanFileMatches(ctx context.Context, fpath string, re *regexp.Regexp, start, end, contextLines, maxMatches int) (lines []matchLine, matchCount, scanned int, truncated bool, err error) {
	f, err := os.Open(fpath)
	if err != nil {
		return nil, 0, 0, false, fmt.Errorf("read_file: %w", err)
	}
	defer f.Close()

	// Reject binaries via the same null-byte sniff read_file uses, feeding the
	// prefix back through io.MultiReader so no Seek is needed.
	sniff := make([]byte, binarySniffBytes)
	n, _ := io.ReadFull(f, sniff)
	sniff = sniff[:n]
	if bytes.IndexByte(sniff, 0) >= 0 {
		return nil, 0, 0, false, fmt.Errorf("read_file: %q appears to be a binary file", fpath)
	}
	src := io.MultiReader(bytes.NewReader(sniff), f)

	scanner := bufio.NewScanner(src)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024) // up to 4 MiB per line

	c := matchCollector{contextLines: contextLines, maxMatches: maxMatches, budget: maxReadFileBytes}
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		if lineNo&0xFFF == 0 {
			if cerr := ctx.Err(); cerr != nil {
				return nil, 0, 0, false, cerr
			}
		}
		if start > 0 && lineNo < start {
			continue
		}
		if end > 0 && lineNo > end {
			break
		}
		scanned++
		text := scanner.Text()
		if !c.feed(matchLine{lineNo: lineNo, text: text}, re.MatchString(text)) {
			break
		}
	}
	if serr := scanner.Err(); serr != nil {
		return nil, 0, 0, false, fmt.Errorf("read_file: %w", serr)
	}
	return c.lines, c.matchCount, scanned, c.truncated, nil
}

// matchCollector accumulates matching lines and their surrounding context as
// scanFileMatches streams a file, keeping the scan loop simple. It maintains a
// before-context ring, a decrementing after-context counter, and a byte budget;
// feed reports whether scanning should continue.
type matchCollector struct {
	contextLines int
	maxMatches   int
	budget       int
	before       []matchLine // pending before-context ring
	after        int         // remaining after-context lines to emit
	lines        []matchLine
	matchCount   int
	truncated    bool
}

func (c *matchCollector) add(sl matchLine) {
	c.lines = append(c.lines, sl)
	c.budget -= len(sl.text) + 1
}

// feed processes one scanned line (matched reports whether it matches the
// pattern) and returns false when scanning should stop (match cap hit with a
// further match present, or the output byte budget exhausted).
func (c *matchCollector) feed(sl matchLine, matched bool) bool {
	switch {
	case matched:
		if c.matchCount >= c.maxMatches {
			c.truncated = true // a further match exists beyond the cap
			return false
		}
		for _, b := range c.before {
			c.add(b)
		}
		c.before = c.before[:0]
		c.add(sl)
		c.matchCount++
		c.after = c.contextLines
	case c.after > 0:
		c.add(sl)
		c.after--
	case c.contextLines > 0:
		c.before = append(c.before, sl)
		if len(c.before) > c.contextLines {
			c.before = c.before[1:]
		}
	}
	if c.budget <= 0 {
		c.truncated = true
		return false
	}
	return true
}

// formatSearchOutput assembles the search response: the plumb-read header (mtime
// + sha + indent classified over the matched text, so the agent knows what
// character to author old_string with), any concurrent-edit/out-of-workspace
// notes, a one-line search summary, then the gutter-rendered matches with rg
// style "--" separators between disjoint groups, and a truncation label when the
// output was bounded.
func (t *ReadFile) formatSearchOutput(fpath string, mtime time.Time, sha string, baseline int64, concurrentNote string, a readFileArgs, matches []matchLine, matchCount, scanned int, truncated bool, start, end int) string {
	rawText := searchRawText(matches)
	body := renderSearchBody(matches)
	var sb strings.Builder
	mtimeStr := mtime.Format(time.RFC3339Nano)
	lines, chars := displayLineCount(body), utf8.RuneCountInString(body)
	if sha != "" {
		fmt.Fprintf(&sb, "# plumb-read mtime=%s sha256=%s indent=%s lines=%d chars=%d baseline=%d\n", mtimeStr, sha, classifyIndent(rawText), lines, chars, baseline)
	} else {
		fmt.Fprintf(&sb, "# plumb-read mtime=%s indent=%s lines=%d chars=%d baseline=%d\n", mtimeStr, classifyIndent(rawText), lines, chars, baseline)
	}
	if concurrentNote != "" {
		sb.WriteString(concurrentNote)
	}
	if label := t.outsideLabel(fpath); label != "" {
		fmt.Fprintf(&sb, "# plumb-note: read-only — outside the workspace (%s); not editable\n", label)
	}

	rangeNote := ""
	if start > 0 || end > 0 {
		rangeNote = " within lines " + searchRangeLabel(start, end)
	}
	if matchCount == 0 {
		fmt.Fprintf(&sb, "# plumb-search: no matches for %q (scanned %d %s%s)\n\n", a.Pattern, scanned, plural(scanned, "line", "lines"), rangeNote)
		fmt.Fprintf(&sb, "No matches for %q in %s%s.", a.Pattern, filepath.Base(fpath), rangeNote)
		sb.WriteString(readFileLiteralHint(a.Pattern, a.UseRegex))
		return sb.String()
	}

	countPhrase := fmt.Sprintf("%d %s", matchCount, plural(matchCount, "match", "matches"))
	if truncated {
		countPhrase = "first " + countPhrase
	}
	fmt.Fprintf(&sb, "# plumb-search: %s for %q (scanned %d %s%s)\n\n", countPhrase, a.Pattern, scanned, plural(scanned, "line", "lines"), rangeNote)
	sb.WriteString(body)
	if truncated {
		fmt.Fprintf(&sb, "\n… (search output truncated at %d %s — narrow the pattern, or restrict the scan with start_line/end_line)", matchCount, plural(matchCount, "match", "matches"))
	}
	return sb.String()
}

// searchRawText joins the matched/context line texts (no gutter) so indent
// classification reflects the file's real leading whitespace.
func searchRawText(matches []matchLine) string {
	var sb strings.Builder
	for i, m := range matches {
		if i > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(m.text)
	}
	return sb.String()
}

// renderSearchBody prints each emitted line with its own 1-based line-number
// gutter (right-aligned to the widest line number), inserting an rg-style "--"
// separator between groups that are not contiguous.
func renderSearchBody(matches []matchLine) string {
	if len(matches) == 0 {
		return ""
	}
	width := len(strconv.Itoa(matches[len(matches)-1].lineNo))
	var sb strings.Builder
	prev := 0
	for i, m := range matches {
		if i > 0 && m.lineNo != prev+1 {
			sb.WriteString("--\n")
		}
		fmt.Fprintf(&sb, "%*d\t%s\n", width, m.lineNo, m.text)
		prev = m.lineNo
	}
	return sb.String()
}

// searchRangeLabel renders the restricted search window for the summary line.
func searchRangeLabel(start, end int) string {
	switch {
	case start > 0 && end > 0:
		return fmt.Sprintf("%d–%d", start, end)
	case start > 0:
		return fmt.Sprintf("%d–EOF", start)
	default:
		return fmt.Sprintf("1–%d", end)
	}
}

// readFileLiteralHint mirrors search_in_files' nudge: on a zero-match literal
// search whose pattern contains obvious regex syntax (| alternation or .*/.+),
// point out it was matched literally so a clean "no matches" is not misread.
func readFileLiteralHint(pattern string, useRegex bool) string {
	if useRegex {
		return ""
	}
	if !strings.Contains(pattern, "|") && !strings.Contains(pattern, ".*") && !strings.Contains(pattern, ".+") {
		return ""
	}
	return "\nNote: the pattern contains regex syntax (| alternation or .*) but use_regex is false, so it was matched literally. Pass use_regex: true to treat it as a pattern."
}
