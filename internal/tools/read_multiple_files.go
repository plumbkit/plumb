package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
)

var readMultipleFilesSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "paths": {
      "type": "array",
      "description": "Absolute paths, file:// URIs, or workspace-relative paths of files to read.",
      "items": { "type": "string" },
      "minItems": 1,
      "maxItems": 20
    },
    "start_line": {
      "type": "integer",
      "description": "First line to return (1-based, inclusive) from EVERY path in this call — same semantics as read_file's start_line, applied uniformly. Omit to start from the beginning of each file.",
      "minimum": 1
    },
    "end_line": {
      "type": "integer",
      "description": "Last line to return (1-based, inclusive) from EVERY path in this call. Omit to read to the end of each file.",
      "minimum": 1
    },
    "pattern": {
      "type": "string",
      "description": "Search EVERY path in this call for this pattern instead of returning a window — same semantics as read_file's pattern (literal by default, smart-case, Go RE2 regex when use_regex). Combine with start_line/end_line to restrict the search to that line window in every file."
    },
    "use_regex": {
      "type": "boolean",
      "default": false,
      "description": "Treat pattern as a Go RE2 regular expression. Only consulted when pattern is set."
    },
    "context_lines": {
      "type": "integer",
      "description": "Lines of context around each match (like rg -C), applied to every path. Default 0. Only consulted when pattern is set.",
      "minimum": 0,
      "maximum": 50
    },
    "max_matches": {
      "type": "integer",
      "description": "Maximum matching lines to return per file in search mode. Default 200. Only consulted when pattern is set.",
      "minimum": 1,
      "maximum": 2000
    }
  },
  "required": ["paths"],
  "additionalProperties": false
}`)

// ReadMultipleFiles reads up to 20 files in a single call, returning each
// file's content separated by a clear header. Errors for individual files are
// reported inline rather than failing the whole call, so a single unreadable
// file doesn't block the others.
//
// PLAN-357: every dependency read_file carries is threaded into the per-file
// inner reader EXCEPT the client-name accessor (WithClient). Reads ARE
// recorded per file exactly as read_file records them, so edit_file works
// under [edits] strict mode without a re-read. The native-edit-lane hint that
// WithClient would otherwise emit per file is suppressed on the inner reader
// and replaced with at most ONE consolidated hint at the end of the response
// — see readMultipleFilesEditHint. Peer-write warnings, outside-workspace
// labels, and the large-file file_outline nudge are safety/orientation
// signals, not per-call noise, so they still fire per affected file exactly
// as read_file would emit them.
//
// Concurrency: Execute is safe for concurrent use.
type ReadMultipleFiles struct {
	tracker      *ReadTracker                            // may be nil; strict-mode tracking disabled when nil
	writes       *WriteTracker                           // may be nil; powers the concurrent-edit-on-read warning
	readsFor     func(ctx context.Context) *ReadTracker  // PLAN-286: per-agent resolver; overrides tracker
	writesFor    func(ctx context.Context) *WriteTracker // PLAN-286: per-agent resolver; overrides writes
	guard        BoundaryGuard
	clientNameFn func() string       // may be nil; gates the ONE consolidated edit-lane hint below
	outsideFn    func(string) string // may be nil; returns a root label when a path is outside the workspace
	outlineFn    func(string) bool   // may be nil; reports whether a path has a structural engine
	ws           WorkspaceFn         // may be nil; anchors workspace-relative entries in paths
}

// NewReadMultipleFiles mirrors NewReadFile's constructor shape: tracker may be
// nil (strict-mode tracking disabled), and WithReadsFor overrides it per call
// on a shared connection.
func NewReadMultipleFiles(tracker *ReadTracker) *ReadMultipleFiles {
	return &ReadMultipleFiles{tracker: tracker}
}

func (t *ReadMultipleFiles) WithBoundary(guard BoundaryGuard) *ReadMultipleFiles {
	t.guard = guard
	return t
}

// WithWorkspace wires the pinned-workspace accessor so relative entries in
// paths resolve against the workspace root, matching read_file. Nil-safe.
func (t *ReadMultipleFiles) WithWorkspace(ws WorkspaceFn) *ReadMultipleFiles {
	t.ws = ws
	return t
}

// WithReadsFor wires a per-call ReadTracker resolver (PLAN-286): on a shared
// connection each logical agent records its reads against its own tracker.
// Takes precedence over the tracker passed to NewReadMultipleFiles.
func (t *ReadMultipleFiles) WithReadsFor(fn func(ctx context.Context) *ReadTracker) *ReadMultipleFiles {
	t.readsFor = fn
	return t
}

// WithWrites wires the per-session WriteTracker so a batch read can warn, per
// affected file, when it changed on disk since plumb last wrote it this
// session (a concurrent peer/external edit). Nil-safe.
func (t *ReadMultipleFiles) WithWrites(w *WriteTracker) *ReadMultipleFiles {
	t.writes = w
	return t
}

// WithWritesFor is the WriteTracker counterpart of WithReadsFor (PLAN-286).
func (t *ReadMultipleFiles) WithWritesFor(fn func(ctx context.Context) *WriteTracker) *ReadMultipleFiles {
	t.writesFor = fn
	return t
}

// WithClient wires the MCP client-name accessor so the response can carry the
// ONE consolidated edit-lane hint for clients whose native Edit tool
// conflicts with plumb's read-state (see edit_lane.go). Nil-safe; without it
// no hint is emitted. Deliberately NOT threaded into the per-file inner
// reader — see the suppression note on the type doc above.
func (t *ReadMultipleFiles) WithClient(fn func() string) *ReadMultipleFiles {
	t.clientNameFn = fn
	return t
}

// WithOutsideLabel wires an accessor that, given a resolved path, returns the
// allowed-root label when the path lies outside the workspace (a read-only
// dependency or configured read root), or "" when inside it. Threaded into
// the per-file inner reader so each affected file is labelled, matching
// read_file. Nil-safe.
func (t *ReadMultipleFiles) WithOutsideLabel(fn func(string) string) *ReadMultipleFiles {
	t.outsideFn = fn
	return t
}

// WithOutlineHint wires an accessor reporting whether a path has a structural
// engine, gating read_file's >32 KiB file_outline nudge (a single line per
// affected file, no prose block). Nil-safe.
func (t *ReadMultipleFiles) WithOutlineHint(fn func(string) bool) *ReadMultipleFiles {
	t.outlineFn = fn
	return t
}

// ReadDeps implements readRecordingTool (see read_deps.go).
func (t *ReadMultipleFiles) ReadDeps() (tracker, readsFor, writes, client bool) {
	return t.tracker != nil, t.readsFor != nil, t.writes != nil, t.clientNameFn != nil
}

func (*ReadMultipleFiles) Name() string                 { return "read_multiple_files" }
func (*ReadMultipleFiles) InputSchema() json.RawMessage { return readMultipleFilesSchema }
func (*ReadMultipleFiles) Description() string {
	return "Read up to 20 files in a single call. Each file's content is returned " +
		"under a '### <path>' heading, followed by that file's own read_file header " +
		"(mtime, sha256, line and byte counts) so it can be edited without re-reading " +
		"— reads ARE recorded per file, exactly like read_file, so edit_file works " +
		"under [edits] strict mode with no re-read. Errors for individual " +
		"files are reported inline — one unreadable file doesn't block the others. " +
		"Accepts absolute paths, file:// URIs, or workspace-relative paths. Binary files are detected and skipped. " +
		"Each file is subject to the same 200 KiB cap as read_file. " +
		"Pass start_line/end_line or pattern (with use_regex/context_lines/max_matches) to slice or search EVERY " +
		"path in the call uniformly — same semantics as read_file's own parameters, applied per file; there is no " +
		"per-path override, so a windowed batch read still records EACH file's full mtime/sha in the read tracker " +
		"(identical to read_file's own ranged-read behaviour — strict mode is mtime-based, not range-based, so a " +
		"later edit anywhere in the file is still covered). The 20-path cap is unchanged by slicing."
}

type readMultipleFilesArgs struct {
	Paths        []string `json:"paths"`
	StartLine    *int     `json:"start_line"`
	EndLine      *int     `json:"end_line"`
	Pattern      string   `json:"pattern"`
	UseRegex     bool     `json:"use_regex"`
	ContextLines int      `json:"context_lines"`
	MaxMatches   int      `json:"max_matches"`
}

// readMultipleFilesParallelism caps simultaneous file reads. 8 is a good
// balance: enough to hide latency from cold-cache reads on rotational media,
// low enough not to thrash an SSD's queue depth or exhaust open-fd limits.
const readMultipleFilesParallelism = 8

// readMultipleFilesEditHint is the ONE consolidated edit-lane hint appended at
// the end of a batch read for clients whose native Edit tool conflicts with
// plumb's read-state (see edit_lane.go). read_file emits an equivalent line
// per call because each read is its own turn; a batch read emits it once,
// pointing back at each file's own per-file header rather than naming a
// single mtime, so the line stays correct no matter how many files were read.
const readMultipleFilesEditHint = "# To edit any of these files: use edit_file (not the native Edit tool), " +
	"passing expected_mtime from that file's own header above.\n"

// rmfHeaderRe matches read_file's own provenance header line (formatOutput /
// formatSearchOutput in read_file.go and read_file_search.go share this exact
// shape), so read_multiple_files can pull the per-file facts back out of its
// inner reader's already-rendered output and restate them compactly rather
// than reimplementing read_file's own formatting.
var rmfHeaderRe = regexp.MustCompile(`^# plumb-read mtime=(\S+)(?: sha256=(\S+))? indent=(\S+) lines=(\d+) chars=(\d+) baseline=(\d+)\n`)

// rmfParsed is one file's provenance header, pulled out of its inner reader's
// rendered output, plus everything that followed the header line (any
// concurrent-edit/outside-workspace/large-file notes, the blank separator,
// then the content) untouched. ok is false when the header didn't match the
// expected shape (should not happen in practice); rest then holds the
// original content unmodified, so nothing is ever lost.
type rmfParsed struct {
	ok                                         bool
	mtime, sha, indent, lines, chars, baseline string
	rest                                       string
}

// parseReadFileHeader splits content into its read_file provenance header
// fields and everything after the header line.
func parseReadFileHeader(content string) rmfParsed {
	loc := rmfHeaderRe.FindStringSubmatchIndex(content)
	if loc == nil {
		return rmfParsed{rest: content}
	}
	m := rmfHeaderRe.FindStringSubmatch(content)
	return rmfParsed{
		ok: true, mtime: m[1], sha: m[2], indent: m[3], lines: m[4], chars: m[5], baseline: m[6],
		rest: content[loc[1]:],
	}
}

// rmfCompactHeader renders the header dedup format (PLAN-357 commit 3): path
// is already stated by the '### path' heading above it, so the per-file line
// drops the "plumb-read " prefix and states mtime, sha256, lines, chars, and
// baseline exactly as read_file does — UNCONDITIONALLY, including chars and
// baseline. A windowed batch read still returns lines=N for just the slice;
// baseline is the only signal in the response that N is a slice of a much
// bigger file rather than the whole thing (review fix: this used to be
// dropped per file on the theory that it "describes the same whole-file read
// three ways" as mtime/sha — true for an UNRANGED read, false for a ranged
// one, where baseline and lines diverge on purpose). indent is the one field
// that stays conditional: it moves to rmfPreamble when every file agrees.
func rmfCompactHeader(p rmfParsed, showIndent bool) string {
	var sb strings.Builder
	sb.WriteString("# mtime=")
	sb.WriteString(p.mtime)
	if p.sha != "" {
		sb.WriteString(" sha256=")
		sb.WriteString(p.sha)
	}
	sb.WriteString(" lines=")
	sb.WriteString(p.lines)
	sb.WriteString(" chars=")
	sb.WriteString(p.chars)
	sb.WriteString(" baseline=")
	sb.WriteString(p.baseline)
	if showIndent {
		sb.WriteString(" indent=")
		sb.WriteString(p.indent)
	}
	sb.WriteByte('\n')
	return sb.String()
}

// rmfPreamble states the one shared fact commit 3 hoists out of every
// per-file header — the indent convention, when every successfully-read file
// agrees on one — ONCE at the top of the response instead of repeating it per
// file. Returns "" when there is nothing to state (no readable files, or the
// files disagree). The pinned workspace root was considered too (the card's
// "shared facts (workspace, indent convention)") and measured out: no
// per-file header has ever stated it, so hoisting it into a preamble line
// removes nothing — it is pure ADDED content, full stop, independent of path
// length or shape. (The scenario 8 sample paths are workspace-relative, which
// only sharpens the point: there was no per-file redundancy to trade it
// against.) See the scenario 8 remeasurement in docs/use-cases.md. Restating
// the root would have made the batch response BIGGER for zero offsetting
// saving, so it stays out.
func rmfPreamble(consensusIndent string) string {
	if consensusIndent == "" {
		return ""
	}
	return "# plumb-read-batch indent=" + consensusIndent + "\n\n"
}

func (t *ReadMultipleFiles) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a readMultipleFilesArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("read_multiple_files: invalid arguments: %w", err)
	}
	if len(a.Paths) == 0 {
		return "", errors.New("read_multiple_files: paths must not be empty")
	}
	if len(a.Paths) > 20 {
		return "", fmt.Errorf("read_multiple_files: at most 20 paths per call, got %d", len(a.Paths))
	}

	results := make([]rmfResult, len(a.Paths))
	// Every read_file dependency read_file itself carries is threaded through
	// the inner reader EXCEPT WithClient — see the type doc and
	// readMultipleFilesEditHint above for why.
	reader := (&ReadFile{tracker: t.tracker, readsFor: t.readsFor}).
		WithBoundary(t.guard).
		WithWorkspace(t.ws).
		WithWrites(t.writes).
		WithWritesFor(t.writesFor).
		WithOutsideLabel(t.outsideFn).
		WithOutlineHint(t.outlineFn)

	sem := make(chan struct{}, readMultipleFilesParallelism)
	var wg sync.WaitGroup
	for i, p := range a.Paths {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			// Every one of these keys must be a canonical read_file schema
			// property — see TestInProcessCompositionsUseCanonicalKeys
			// (inprocess_call_guard_test.go), which checks this literal
			// against read_file's own schema. StartLine/EndLine are *int:
			// json.Marshal of a nil pointer inside an `any`-valued map emits
			// null, and read_file's own *int fields unmarshal null as nil —
			// "not specified", identical to omitting the key.
			raw, _ := json.Marshal(map[string]any{
				"file_path":     p,
				"start_line":    a.StartLine,
				"end_line":      a.EndLine,
				"pattern":       a.Pattern,
				"use_regex":     a.UseRegex,
				"context_lines": a.ContextLines,
				"max_matches":   a.MaxMatches,
			})
			out, err := reader.Execute(ctx, raw)
			results[i] = rmfResult{content: out, err: err}
		}()
	}
	wg.Wait()

	return rmfAssemble(a.Paths, results, t.outsideFn, t.clientNameFn), nil
}

// rmfResult is one path's inner read_file call outcome.
type rmfResult struct {
	content string
	err     error
}

// rmfAssemble builds the final response from each path's inner read_file
// result: a header-dedup pass (commit 3), then the per-file '### path' blocks,
// then at most one consolidated edit-lane hint. Split out of Execute to keep
// it under the complexity budget — see the type doc for the design.
func rmfAssemble(paths []string, results []rmfResult, outsideFn func(string) string, clientNameFn func() string) string {
	// No separator rule. It used to be strings.Repeat("─", 60) — and U+2500 is 3
	// bytes in UTF-8, so each rule cost 180 bytes. On a three-file read that was
	// 543 bytes, 17% of the entire response, spent on decoration.
	//
	// The "### " heading is the boundary marker, and it is sufficient precisely
	// because of the line-number gutter below it: file CONTENT containing a
	// markdown heading renders as "  1\t### Subsection", indented and numbered,
	// while a real boundary starts at column 0. That is what the rule was
	// disambiguating, and the gutter already does it for free.
	//
	// The byte count is gone too, and its removal is a correctness fix rather than
	// a saving: it printed len(r.content), the length of read_file's RENDERED
	// output — header and gutters included — not the size of the file. A 677-byte
	// file was announced as "933 bytes", one line above its own header stating
	// chars=675 baseline=677. Three numbers, and the prominent one meant nothing.
	// The provenance line carries the real figures.
	//
	// Commit 3 (header dedup): the per-file header from each inner read_file
	// call still carries its own workspace-independent chars/baseline pair (a
	// ranged read reflects the returned slice vs the whole file — that stays
	// useful per file) but the indent CONVENTION, when every successfully-read
	// file agrees on one, is a shared fact repeated N times for no reason. Pull
	// it into ONE preamble line (see rmfPreamble for why workspace didn't make
	// the cut), and shrink each per-file header to what's left: mtime +
	// sha256 + lines, indent only when a file disagrees with the consensus.
	parsed, consensusIndent := rmfParseHeaders(results)

	var sb strings.Builder
	sb.WriteString(rmfPreamble(consensusIndent))
	editable := false
	for i, p := range paths {
		if i > 0 {
			sb.WriteString("\n")
		}
		r := results[i]
		if r.err != nil {
			fmt.Fprintf(&sb, "### %s\n### ERROR: %s\n", p, r.err.Error())
			continue
		}
		fmt.Fprintf(&sb, "### %s\n", p)
		pf := parsed[i]
		if pf.ok {
			sb.WriteString(rmfCompactHeader(pf, consensusIndent == "" || pf.indent != consensusIndent))
		}
		sb.WriteString(pf.rest)
		sb.WriteString("\n")
		if outsideFn == nil || outsideFn(p) == "" {
			editable = true
		}
	}
	// One hint, not N — see the type doc and readMultipleFilesEditHint above.
	if editable && clientHasNativeEditConflict(clientNameFn) {
		sb.WriteString("\n" + readMultipleFilesEditHint)
	}
	return sb.String()
}

// rmfPreambleMinFiles is the fewest successfully-read, indent-agreeing files
// worth a preamble line for. The line itself costs bytes ("# plumb-read-batch
// indent=X\n\n" is ~28 B); the review round that added chars=/baseline= back
// to every per-file header priced the payoff per file: dropping one file's
// " indent=X" (~8-13 B) doesn't cover a 28 B line until at least three files
// share it. Below that, showing indent on each file (the DivergentIndent
// path) is smaller AND simpler — no preamble to explain.
const rmfPreambleMinFiles = 3

// rmfParseHeaders parses every successful result's provenance header (see
// rmfParsed) and reports the batch's consensus indent — the single value
// every successfully-read file agrees on, or "" when there is no reading, no
// agreement, or fewer than rmfPreambleMinFiles files agreeing (the preamble
// line doesn't pay for itself below that — see rmfPreambleMinFiles).
func rmfParseHeaders(results []rmfResult) (parsed []rmfParsed, consensusIndent string) {
	parsed = make([]rmfParsed, len(results))
	indentCounts := make(map[string]int, 2)
	for i, r := range results {
		if r.err != nil {
			continue
		}
		p := parseReadFileHeader(r.content)
		parsed[i] = p
		if p.ok {
			indentCounts[p.indent]++
		}
	}
	if len(indentCounts) == 1 {
		for indent, n := range indentCounts {
			if n >= rmfPreambleMinFiles {
				consensusIndent = indent
			}
		}
	}
	return parsed, consensusIndent
}
