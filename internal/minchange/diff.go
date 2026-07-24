package minchange

import "strings"

// Diff is a parsed unified diff: the output of `git diff` (with or without
// --cached), split into per-file changes.
type Diff struct {
	Files []FileDiff
}

// FileDiff is the change to a single file.
type FileDiff struct {
	// Path is the new-side path (b/…), or the old path for a deletion.
	Path string
	// OldPath is the a/… path; differs from Path on a rename.
	OldPath  string
	IsNew    bool
	IsDelete bool
	IsBinary bool
	Hunks    []Hunk
}

// Hunk is one @@ region of a FileDiff.
type Hunk struct {
	// NewStart is the 1-based first line number on the new side.
	NewStart int
	Lines    []Line
}

// LineKind classifies a hunk body line.
type LineKind byte

const (
	// Context is an unchanged line (leading space).
	Context LineKind = ' '
	// Added is a new line (leading +).
	Added LineKind = '+'
	// Removed is a deleted line (leading -).
	Removed LineKind = '-'
)

// Line is one line inside a hunk, carrying its new-side line number for added
// and context lines (0 for removed lines, which have no new-side position).
type Line struct {
	Kind LineKind
	Text string
	// NewLineNo is the 1-based new-side line number for Added/Context lines; 0
	// for Removed lines.
	NewLineNo int
}

// ParseUnifiedDiff parses git's unified-diff text. It is tolerant of the
// surrounding metadata git emits (index, mode, similarity, rename lines) and of
// binary-file markers, extracting only what the checks need: per-file paths,
// new/delete/binary status, and hunks with new-side line numbers. Unparseable
// noise is ignored rather than fatal — this is an advisory tool.
func ParseUnifiedDiff(raw string) *Diff {
	p := &diffParser{d: &Diff{}}
	for _, line := range strings.Split(raw, "\n") {
		p.feed(line)
	}
	p.flushFile()
	return p.d
}

// diffParser holds the streaming parse state.
type diffParser struct {
	d         *Diff
	cur       *FileDiff
	hunk      *Hunk
	newLineNo int
}

func (p *diffParser) flushHunk() {
	if p.cur != nil && p.hunk != nil {
		p.cur.Hunks = append(p.cur.Hunks, *p.hunk)
		p.hunk = nil
	}
}

func (p *diffParser) flushFile() {
	p.flushHunk()
	if p.cur != nil {
		p.d.Files = append(p.d.Files, *p.cur)
		p.cur = nil
	}
}

// feed consumes one line of the diff, updating parser state.
func (p *diffParser) feed(line string) {
	if strings.HasPrefix(line, "diff --git ") {
		p.flushFile()
		p.cur = &FileDiff{}
		if a, b, ok := parseDiffGitHeader(line); ok {
			p.cur.OldPath, p.cur.Path = a, b
		}
		return
	}
	if p.cur == nil {
		return // preamble before the first file header — ignore
	}
	if p.feedFileHeader(line) {
		return
	}
	if strings.HasPrefix(line, "@@") {
		p.flushHunk()
		p.newLineNo = parseHunkNewStart(line)
		p.hunk = &Hunk{NewStart: p.newLineNo}
		return
	}
	if p.hunk != nil {
		appendHunkLine(p.hunk, line, &p.newLineNo)
	}
}

// feedFileHeader handles the per-file metadata lines (binary marker, --- / +++
// paths), returning true when it consumed the line. The --- / +++ cases apply
// only before the file's first hunk: inside a hunk body those prefixes are
// content (a removed "-- " SQL comment renders as "--- …"), and must reach the
// hunk-line handler instead of being misread as a new file header.
func (p *diffParser) feedFileHeader(line string) bool {
	switch {
	case strings.HasPrefix(line, "Binary files ") || strings.HasPrefix(line, "GIT binary patch"):
		p.cur.IsBinary = true
	case p.hunk == nil && strings.HasPrefix(line, "--- "):
		if hp := headerPath(line[4:]); hp == "" {
			p.cur.IsNew = true
		} else {
			p.cur.OldPath = hp
		}
	case p.hunk == nil && strings.HasPrefix(line, "+++ "):
		if hp := headerPath(line[4:]); hp == "" {
			p.cur.IsDelete = true
		} else {
			p.cur.Path = hp
		}
	default:
		return false
	}
	return true
}

// appendHunkLine classifies one hunk body line and advances the new-side line
// counter. A "\ No newline at end of file" marker and any other non-body line
// are ignored.
func appendHunkLine(h *Hunk, line string, newLineNo *int) {
	if line == "" {
		// A bare empty line inside a hunk is an unchanged empty line.
		h.Lines = append(h.Lines, Line{Kind: Context, Text: "", NewLineNo: *newLineNo})
		*newLineNo++
		return
	}
	switch line[0] {
	case '+':
		h.Lines = append(h.Lines, Line{Kind: Added, Text: line[1:], NewLineNo: *newLineNo})
		*newLineNo++
	case '-':
		h.Lines = append(h.Lines, Line{Kind: Removed, Text: line[1:], NewLineNo: 0})
	case ' ':
		h.Lines = append(h.Lines, Line{Kind: Context, Text: line[1:], NewLineNo: *newLineNo})
		*newLineNo++
	case '\\':
		// "\ No newline at end of file" — not a content line.
	}
}

// parseDiffGitHeader extracts the a/ and b/ paths from a "diff --git a/x b/y"
// line. It handles unquoted paths without spaces (the common case); quoted or
// space-bearing paths fall through to the ---/+++ lines, which are parsed
// separately.
func parseDiffGitHeader(line string) (aPath, bPath string, ok bool) {
	rest := strings.TrimPrefix(line, "diff --git ")
	fields := strings.Fields(rest)
	if len(fields) != 2 {
		return "", "", false
	}
	return stripABPrefix(fields[0]), stripABPrefix(fields[1]), true
}

// headerPath extracts the path from a --- / +++ header value, returning "" for
// /dev/null (which signals a file creation or deletion).
func headerPath(v string) string {
	v = strings.TrimSpace(v)
	// git appends a tab + timestamp on some configs; keep only the path field.
	if i := strings.IndexByte(v, '\t'); i >= 0 {
		v = v[:i]
	}
	if v == "/dev/null" {
		return ""
	}
	return stripABPrefix(v)
}

// stripABPrefix removes git's a/ or b/ path prefix.
func stripABPrefix(p string) string {
	switch {
	case strings.HasPrefix(p, "a/"):
		return p[2:]
	case strings.HasPrefix(p, "b/"):
		return p[2:]
	default:
		return p
	}
}

// parseHunkNewStart reads the new-side start line from a hunk header
// "@@ -l,s +l,s @@". Returns 1 on any parse failure (a safe lower bound).
func parseHunkNewStart(line string) int {
	plus := strings.IndexByte(line, '+')
	if plus < 0 {
		return 1
	}
	rest := line[plus+1:]
	// rest is like "l,s @@ heading" or "l @@ heading".
	end := len(rest)
	for i, r := range rest {
		if r == ',' || r == ' ' {
			end = i
			break
		}
	}
	n := atoiSafe(rest[:end])
	if n <= 0 {
		return 1
	}
	return n
}

// atoiSafe parses a non-negative integer, returning 0 on any non-digit input.
func atoiSafe(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}
