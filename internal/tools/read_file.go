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
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

var readFileSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "file_path": {
      "type": "string",
      "description": "Absolute path, file:// URI, or workspace-relative path of the file to read."
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
      "description": "Number of lines to return starting at the first line (Claude Code-style window; first line defaults to 1). Mutually exclusive with end_line. Not usable together with pattern (search mode) — use max_matches instead.",
      "minimum": 1
    },
    "pattern": {
      "type": "string",
      "description": "Search the file for this pattern instead of returning a window: returns each matching line with its 1-based line number (and optional context), bounded output. Literal text by default; a regular expression when use_regex is true. The whole file is scanned line-by-line regardless of size, so an over-cap file stays searchable. Combine with start_line/end_line to restrict the search to that line window; not usable with limit."
    },
    "use_regex": {
      "type": "boolean",
      "default": false,
      "description": "Treat pattern as a Go RE2 regular expression. Default false — pattern is literal text. Only consulted when pattern is set."
    },
    "case_sensitive": {
      "type": "boolean",
      "description": "Force case-sensitive matching for pattern. Default: smart-case — case-insensitive when pattern is all lowercase, case-sensitive otherwise. Only consulted when pattern is set."
    },
    "context_lines": {
      "type": "integer",
      "description": "Lines of context to show before and after each match (like rg -C). Default 0. Only consulted when pattern is set.",
      "minimum": 0,
      "maximum": 10
    },
    "max_matches": {
      "type": "integer",
      "description": "Maximum number of matching lines to return in search mode. Default 200. Output is truncated (and labelled) beyond this. Only consulted when pattern is set.",
      "minimum": 1,
      "maximum": 2000
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
// followed by a blank line, then the content (or selected line range). Each
// content line is prefixed with a display-only 1-based file line-number gutter
// ("<n>\t…", the cat -n convention); the gutter is not part of the file and
// must be stripped before a line is used as an edit_file old_string.
// sha256 is computed over the full file, not the sliced excerpt.
//
// If a non-nil ReadTracker is supplied, every successful read records the
// observed mtime so edit_file's strict mode can verify the agent did read
// the file before editing it.
//
// Concurrency: Execute is safe for concurrent use.
type ReadFile struct {
	tracker      *ReadTracker  // may be nil; strict-mode tracking disabled when nil
	writes       *WriteTracker // may be nil; powers the concurrent-edit-on-read warning
	guard        BoundaryGuard
	clientNameFn func() string       // may be nil; gates the edit-lane hint to conflict-prone clients
	outsideFn    func(string) string // may be nil; returns a root label when the path is outside the workspace
	outlineFn    func(string) bool   // may be nil; reports whether the path has a structural engine (file_outline is worthwhile)
	ws           WorkspaceFn         // may be nil; anchors a workspace-relative file_path to the pinned root
}

func NewReadFile(tracker *ReadTracker) *ReadFile { return &ReadFile{tracker: tracker} }

// WithWrites wires the per-session WriteTracker so read_file can warn when a
// file changed on disk since plumb last wrote it this session (a concurrent
// peer/external edit). Nil-safe; without it no concurrent-edit warning is shown.
func (t *ReadFile) WithWrites(w *WriteTracker) *ReadFile {
	t.writes = w
	return t
}

// concurrentEditNote returns a one-line warning when plumb wrote this file
// earlier in the session and its on-disk mtime has since advanced — i.e. a peer
// or external process edited it after plumb's write. Returns "" otherwise.
func (t *ReadFile) concurrentEditNote(fpath string, mtime time.Time) string {
	recorded, ok := t.writes.WroteMtime(fpath)
	if !ok || recorded == 0 || mtime.UnixNano() <= recorded {
		return ""
	}
	return "# plumb-warn: changed on disk since plumb last wrote it this session — a peer or external process may have edited it; this read reflects the new content\n"
}

func (t *ReadFile) WithBoundary(guard BoundaryGuard) *ReadFile {
	t.guard = guard
	return t
}

// WithWorkspace wires the pinned-workspace accessor so a relative file_path is
// resolved against the workspace root rather than the daemon's working
// directory. Nil-safe (a relative path then stays relative and the boundary
// check rejects it).
func (t *ReadFile) WithWorkspace(ws WorkspaceFn) *ReadFile {
	t.ws = ws
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

// WithOutlineHint wires an accessor reporting whether path has a structural
// engine (Go AST, tree-sitter, including Markdown/config) so a one-call
// file_outline would return a useful map. read_file uses it to gate the
// large-read nudge — a suggestion to call file_outline on a big structured file
// is only helpful when there is structure to outline. Nil-safe (no nudge).
func (t *ReadFile) WithOutlineHint(fn func(string) bool) *ReadFile {
	t.outlineFn = fn
	return t
}

func (t *ReadFile) outlineSupported(path string) bool {
	return t.outlineFn != nil && t.outlineFn(path)
}

func (t *ReadFile) Name() string                 { return "read_file" }
func (t *ReadFile) InputSchema() json.RawMessage { return readFileSchema }
func (t *ReadFile) Description() string {
	return "Read the text contents of a file (absolute path, file:// URI, or workspace-relative path). " +
		"Use start_line/end_line to stream a slice of a large file. " +
		"Each line is prefixed with a 1-based line number + tab (cat -n style) for exact range math; this gutter " +
		"is display-only — strip the leading '<n>\\t' before reusing a line as an edit_file/find_replace old_string. " +
		"Binary files are rejected; output is capped at 200 KiB (use line ranges on large files). " +
		"The header carries the file's mtime (RFC3339Nano) and SHA-256 — pass them back as expected_mtime/" +
		"expected_sha on edit_file for optimistic-concurrency checks. " +
		"Pass pattern to search WITHIN the file instead of windowing: it returns each matching line with its " +
		"1-based line number (and optional context_lines), so an over-cap file stays searchable in one tool — " +
		"literal text by default (smart-case: case-insensitive when all lowercase), Go RE2 regex when use_regex; " +
		"output is bounded by max_matches (default 200) and labelled when truncated. Combine pattern with " +
		"start_line/end_line to restrict the search to a line window; pattern with limit is rejected. " +
		"Essential for clients without filesystem access (Claude Desktop, Cursor MCP, etc.)."
}

type readFileArgs struct {
	Path          string `json:"file_path"`
	StartLine     *int   `json:"start_line"`
	EndLine       *int   `json:"end_line"`
	Offset        *int   `json:"offset"`
	Limit         *int   `json:"limit"`
	Pattern       string `json:"pattern"`
	UseRegex      bool   `json:"use_regex"`
	CaseSensitive *bool  `json:"case_sensitive"`
	ContextLines  int    `json:"context_lines"`
	MaxMatches    int    `json:"max_matches"`
}

// ExecTimeoutBounded opts read_file into the dispatcher's tool-execution
// deadline: a blocking stat/open on a stalled mount fails fast instead of
// hanging the call to the client's own timeout. See mcp.ExecTimeoutBounded.
func (*ReadFile) ExecTimeoutBounded() {}

func (t *ReadFile) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a readFileArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("read_file: invalid arguments: %w", err)
	}
	if a.Path == "" {
		return "", fmt.Errorf("read_file: file_path is required")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}

	// Accept absolute paths, file:// URIs, and workspace-relative paths.
	fpath := resolvePath(a.Path, t.ws)
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
	concurrentNote := t.concurrentEditNote(fpath, mtime)

	// Search mode: pattern locates matching lines within the file (streamed
	// line-by-line, so an over-cap file stays searchable) instead of returning a
	// positional window.
	if a.Pattern != "" {
		return t.searchWithinFile(ctx, fpath, info, mtime, concurrentNote, a)
	}

	body, err := readFileBody(fpath, a)
	if err != nil {
		return "", err
	}

	sha, err := fileSHA256(fpath)
	if err != nil {
		slog.Warn("read_file: computing sha256", "path", fpath, "err", err)
	}
	t.tracker.Record(fpath, mtime, sha)

	firstLine := 1
	if body.start != nil && *body.start > 1 {
		firstLine = *body.start
	}
	largeNote := t.largeReadNote(fpath, len(body.content), body.truncated, body.ranged)
	return t.formatOutput(mtime, sha, body.content, info.Size(), firstLine, body.hasLines, body.truncated, t.outsideLabel(fpath), concurrentNote, largeNote), nil
}

// readBody is the decoded result of reading (a slice of) a file.
type readBody struct {
	content   string
	hasLines  bool // real content vs an "(no lines in range …)" placeholder
	truncated bool // hit the 200 KiB hard cap
	ranged    bool // a start_line/end_line/offset/limit window was requested
	start     *int // resolved first line (nil ⇒ from line 1)
}

// readFileBody opens fpath, rejects binaries, applies the optional line window,
// and caps the result at maxReadFileBytes. Extracted from Execute so the
// orchestrator stays under the complexity bound.
func readFileBody(fpath string, a readFileArgs) (readBody, error) {
	f, err := os.Open(fpath)
	if err != nil {
		return readBody{}, fmt.Errorf("read_file: %w", err)
	}
	defer f.Close()

	// Sniff up to binarySniffBytes for null bytes. We hand the prefix bytes
	// back into the read path via io.MultiReader so no Seek is needed — Seek
	// fails on pipes/devices and is wasted work on regular files.
	sniff := make([]byte, binarySniffBytes)
	n, _ := io.ReadFull(f, sniff)
	sniff = sniff[:n]
	if bytes.IndexByte(sniff, 0) >= 0 {
		return readBody{}, fmt.Errorf("read_file: %q appears to be a binary file", fpath)
	}
	src := io.MultiReader(bytes.NewReader(sniff), f)

	start, end, err := resolveLineWindow(a)
	if err != nil {
		return readBody{}, fmt.Errorf("read_file: %w", err)
	}
	content, hasLines, err := readContentMaybeRanged(src, start, end)
	if err != nil {
		return readBody{}, fmt.Errorf("read_file: %w", err)
	}

	truncated := false
	if len(content) > maxReadFileBytes {
		content = content[:maxReadFileBytes]
		if idx := strings.LastIndex(content, "\n"); idx > 0 {
			content = content[:idx]
		}
		truncated = true
	}
	return readBody{content: content, hasLines: hasLines, truncated: truncated, ranged: start != nil || end != nil, start: start}, nil
}

// largeReadFileThreshold is the whole-file body size above which read_file
// nudges toward file_outline. Well below the 200 KiB hard cap but above a
// typical client's comfortable token budget for a single file, so the agent is
// pointed at the structural map before its own context cap (which plumb cannot
// see) forces a spill. 32 KiB ≈ 8k–10k tokens.
const largeReadFileThreshold = 32 * 1024

// largeReadNote returns a one-line nudge toward file_outline when an unranged,
// non-truncated read returns a large body for a structurally-known file. It is
// suppressed for ranged reads (the agent is already slicing), truncated reads
// (the truncation note already names file_outline), and paths with no
// structural engine (nothing useful to outline). Returns "" otherwise.
func (t *ReadFile) largeReadNote(path string, size int, truncated, ranged bool) string {
	if truncated || ranged || size <= largeReadFileThreshold || !t.outlineSupported(path) {
		return ""
	}
	return fmt.Sprintf("# plumb-note: large file (%d KiB) — file_outline returns its structure "+
		"in ~200 tokens (symbols/sections, no bodies); read_file with start_line/end_line reads a slice\n", size/1024)
}

// formatOutput assembles the read_file response: the plumb-read header line
// (mtime + optional sha + indent), an optional edit-lane hint line for clients
// whose native Edit tool conflicts with plumb's read-state, a blank separator,
// then the (possibly truncated) content.
func (t *ReadFile) formatOutput(mtime time.Time, sha, content string, baseline int64, firstLine int, number, truncated bool, outsideLabel, concurrentNote, largeNote string) string {
	var sb strings.Builder
	mtimeStr := mtime.Format(time.RFC3339Nano)
	// lines/chars describe the body actually returned (a ranged read reflects the
	// slice). chars is rune count, not bytes — context-window limits are
	// character-denominated and bytes mislead for any non-ASCII text
	// (from dogfooding feedback). baseline is the whole-file byte size, so
	// the savings scorer can value a ranged read against the cost of reading it all.
	lines, chars := displayLineCount(content), utf8.RuneCountInString(content)
	if sha != "" {
		fmt.Fprintf(&sb, "# plumb-read mtime=%s sha256=%s indent=%s lines=%d chars=%d baseline=%d\n", mtimeStr, sha, classifyIndent(content), lines, chars, baseline)
	} else {
		fmt.Fprintf(&sb, "# plumb-read mtime=%s indent=%s lines=%d chars=%d baseline=%d\n", mtimeStr, classifyIndent(content), lines, chars, baseline)
	}
	if concurrentNote != "" {
		sb.WriteString(concurrentNote)
	}
	if largeNote != "" {
		sb.WriteString(largeNote)
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
	if number {
		content = withLineGutter(content, firstLine)
	}
	sb.WriteString(content)
	if truncated {
		sb.WriteString("\n… (output truncated at 200 KiB — use start_line/end_line to read specific sections, " +
			"or file_outline for a one-call structural map of the whole file: symbols/sections without the body)")
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
// The bool return reports whether the string is real file content (true) versus
// an "(no lines in range …)" placeholder (false); only real content gets the
// display line-number gutter.
func readContentMaybeRanged(src io.Reader, startLine, endLine *int) (string, bool, error) {
	if startLine == nil && endLine == nil {
		data, err := io.ReadAll(src)
		if err != nil {
			return "", false, err
		}
		return string(data), true, nil
	}
	start, end := resolveReadRange(startLine, endLine)
	if end >= 0 && start > end {
		return fmt.Sprintf("(no lines in range %d–%d)", start, end), false, nil
	}
	return readLineRange(src, start, end)
}

// withLineGutter prefixes each line of content with its 1-based file line
// number, right-aligned and tab-separated — the cat -n convention Claude Code's
// native Read uses, so agents already strip it for str_replace. firstLine is
// the file line number of content's first line (1 for a whole-file read; the
// range start for a sliced read). The gutter is display-only: callers strip the
// leading "<n>\t" before using a line as an edit_file/find_replace old_string.
func withLineGutter(content string, firstLine int) string {
	if content == "" {
		return content
	}
	trailingNL := strings.HasSuffix(content, "\n")
	body := content
	if trailingNL {
		body = body[:len(body)-1]
	}
	lines := strings.Split(body, "\n")
	width := len(strconv.Itoa(firstLine + len(lines) - 1))
	var sb strings.Builder
	sb.Grow(len(content) + len(lines)*(width+1))
	for i, line := range lines {
		if i > 0 {
			sb.WriteByte('\n')
		}
		fmt.Fprintf(&sb, "%*d\t%s", width, firstLine+i, line)
	}
	if trailingNL {
		sb.WriteByte('\n')
	}
	return sb.String()
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

func readLineRange(src io.Reader, start, end int) (string, bool, error) {
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
		return "", false, err
	}
	if lineNo < start {
		endLabel := fmt.Sprintf("%d", end)
		if end < 0 {
			endLabel = "EOF"
		}
		return fmt.Sprintf("(no lines in range %d–%s; file has %d lines)", start, endLabel, lineNo), false, nil
	}
	return sb.String(), true, nil
}

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
