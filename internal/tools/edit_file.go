package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// edit_file is split across files by concern: the all-or-nothing apply path
// lives in edit_file_apply.go; the apply_partial path in edit_file_partial.go;
// error construction and the line-ending / line-change helpers in
// edit_file_errors.go. This file holds the Tool surface and the precondition
// gates (dirty / optimistic-concurrency / strict mode).

var editFileSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "file_path": {
      "type": "string",
      "description": "File: absolute path, file:// URI, or workspace-relative."
    },
    "start_anchor": {
      "type": "string",
      "description": "Anchor mode (instead of edits): unique substring, exactly once, starting the span. Tolerates CRLF and a pasted \"<n>\\t\" gutter."
    },
    "end_anchor": {
      "type": "string",
      "description": "Anchor mode: unique substring, exactly once, after start_anchor, ending the span."
    },
    "new_string": {
      "type": "string",
      "description": "Anchor mode: replacement for the span; empty deletes it."
    },
    "include_anchors": {
      "type": "boolean",
      "description": "Anchor mode: false (default) KEEPS both anchors and replaces only the text between them; true replaces the anchors too."
    },
    "edits": {
      "type": "array",
      "description": "Edits applied in order, all or nothing unless apply_partial (instead of anchor mode).",
      "items": {
        "type": "object",
        "properties": {
          "old_string": {
            "type": "string",
            "description": "Exact text, matched EXACTLY ONCE or the edit is refused (CRLF/LF tolerated). Not used with start_line."
          },
          "new_string": {
            "type": "string",
            "description": "Replacement; empty deletes. With start_line it replaces whole lines (adding a missing trailing newline)."
          },
          "start_line": {
            "type": "integer",
            "description": "Range mode: first line, 1-based; -1 appends at EOF. Prefer range for big multi-line replacements."
          },
          "end_line": {
            "type": "integer",
            "description": "Range mode: last line, inclusive (default start_line; -1 = EOF)."
          },
          "replace_all": {
            "type": "boolean",
            "description": "Replace every occurrence instead of requiring exactly one (default false)."
          }
        },
        "required": ["new_string"],
        "additionalProperties": false
      },
      "minItems": 1
    },
    "expected_mtime": {
      "type": "string",
      "description": "mtime from read_file; refuse if the file changed since."
    },
    "expected_sha": {
      "type": "string",
      "description": "sha256 from read_file; refuse if the content changed since (stronger than mtime)."
    },
    "dirty_ok": {
      "type": "boolean",
      "description": "Allow a file with uncommitted git changes (default false: refused)."
    },
    "apply_partial": {
      "type": "boolean",
      "description": "Apply each edit independently and report per-edit results instead of rolling back the batch. Not with strict mode; unsafe when agents share the file."
    },
    "await_diagnostics": {
      "type": "boolean",
      "description": "Wait a few seconds for the language server and append a labelled 'diagnostics delta' line (new_errors, resolved, pre_existing). Default false: a fast window whose result may predate the write."
    },
    "fail_on_new_errors": {
      "type": "boolean",
      "description": "Roll the edit back (file unchanged) if the language server CONFIRMS new errors in this file; unconfirmed checks, warnings and errors elsewhere never roll back. Implies await_diagnostics; not with apply_partial or over 1 MiB."
    },
    "reconcile": {
      "type": "boolean",
      "description": "Apply despite an expected_mtime/sha mismatch, relying on the exact-once match, e.g. after a formatter touched the file. Default false."
    }
  },
  "required": ["file_path"],
  "additionalProperties": false
}`)

// maxEditRetries is the maximum number of times edit_file will retry when it
// detects a concurrent write between its read and rename.
const maxEditRetries = 3

// EditFile applies one or more str_replace edits to a file.
//
// Safety model (five layers):
//
//  1. Per-path lock: a process-global lock serialises concurrent edit_file /
//     write_file calls to the same path. Two parallel sessions cannot interleave
//     read/write operations on the same file.
//
//  2. Uniqueness lock: each old_string must appear EXACTLY ONCE. If the file was
//     modified concurrently (old_string absent or context changed), the edit is
//     rejected with a clear error — no silent corruption possible.
//
//  3. Optional expected_mtime: when supplied, the file's current mtime must
//     match. Rejects edits to a file that changed since the agent's read.
//
//  4. In-memory application: all edits are applied in memory to produce the
//     final content before any write occurs. If any edit fails, the file is
//     not touched.
//
//  5. Atomic write + retry: content is staged in os.TempDir() and renamed.
//     A pre-rename mtime check rejects writes if the file changed between
//     our read and the rename. A post-rename mtime check triggers a retry
//     (up to maxEditRetries=3) if a third party wrote after our rename.
//
// CRLF/LF handling: line endings in old_string are normalised against the file
// before matching, so an old_string with LF can match a file with CRLF.
//
// Concurrency: Execute is safe for concurrent use.
type EditFile struct{ deps WriteDeps }

func NewEditFile(deps WriteDeps) *EditFile { return &EditFile{deps: deps} }

// isStrict reports whether strict mode applies to this call. Prefers the
// configured StrictModeFn (per-workspace + env merged by daemon); falls
// back to env-only check when no closure is wired.
func (t *EditFile) isStrict() bool { return strictEnabled(t.deps.Strict) }

func (*EditFile) Name() string                 { return "edit_file" }
func (*EditFile) InputSchema() json.RawMessage { return editFileSchema }
func (*EditFile) Description() string {
	return "Edit an existing file (use this over a native edit tool). Two exclusive shapes: an edits " +
		"array, or start_anchor + end_anchor + new_string.\n\n" +
		"An edits entry is str_replace (old_string must appear EXACTLY ONCE) or a line range " +
		"(start_line/end_line). Prefer a range for a big multi-line change: escaping inside a JSON " +
		"string often defeats an exact match. Anchor mode replaces the text BETWEEN two unique " +
		"anchors, keeping them unless include_anchors=true; an anchor quoted without its newline " +
		"joins that line onto new_string (flagged).\n\n" +
		"Atomic, crash-durable, per-path locked. Pass expected_mtime from read_file when another " +
		"writer may touch the file. For a whole declaration prefer replace_symbol_body and its " +
		"siblings; mode choice in depth: the plumb-refactor skill."
}

type strEdit struct {
	OldStr     string `json:"old_string"`
	NewStr     string `json:"new_string"`
	StartLine  int    `json:"start_line"`
	EndLine    int    `json:"end_line"`
	ReplaceAll bool   `json:"replace_all"`
}

type editFileArgs struct {
	Path             string    `json:"file_path"`
	Edits            []strEdit `json:"edits"`
	StartAnchor      string    `json:"start_anchor"`
	EndAnchor        string    `json:"end_anchor"`
	NewStr           string    `json:"new_string"`
	IncludeAnchors   bool      `json:"include_anchors"`
	ExpectedMtime    string    `json:"expected_mtime"`
	ExpectedSha      string    `json:"expected_sha"`
	DirtyOk          bool      `json:"dirty_ok"`
	ApplyPartial     bool      `json:"apply_partial"`
	AwaitDiagnostics bool      `json:"await_diagnostics"`
	FailOnNewErrors  bool      `json:"fail_on_new_errors"`
	Reconcile        bool      `json:"reconcile"`
}

// diagOpts derives the post-write diagnostics request from the call's flags.
// fail_on_new_errors implies await_diagnostics: a rollback decision may only
// ever rest on a confirmed pass, so it must pay for one.
func (a editFileArgs) diagOpts(lspNotifyFailed bool) postWriteDiagOpts {
	confirm := a.AwaitDiagnostics || a.FailOnNewErrors
	return postWriteDiagOpts{
		awaitFresh:      confirm,
		structured:      confirm,
		lspNotifyFailed: lspNotifyFailed,
	}
}

// anchorMode reports whether this request uses the anchor-bounded edit shape
// (start_anchor / end_anchor) rather than the str_replace / range edits array.
// The two shapes are mutually exclusive; validateMode enforces it.
func (a editFileArgs) anchorMode() bool {
	return a.StartAnchor != "" || a.EndAnchor != ""
}

// validateMode enforces that exactly one edit shape is used: either the edits
// array, or the start_anchor/end_anchor pair — never both, never neither.
func (a editFileArgs) validateMode() error {
	if a.anchorMode() {
		if len(a.Edits) > 0 {
			return errors.New("edit_file: provide either edits or start_anchor/end_anchor, not both")
		}
		if a.StartAnchor == "" || a.EndAnchor == "" {
			return errors.New("edit_file: anchor mode requires both start_anchor and end_anchor")
		}
		return nil
	}
	if len(a.Edits) == 0 {
		return errors.New("edit_file: at least one edit is required (or pass start_anchor/end_anchor)")
	}
	return nil
}

func (t *EditFile) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	if !t.deps.limiter(ctx).Allow() {
		return "", rateLimitError("edit_file", t.deps.limiter(ctx))
	}
	a, err := parseEditFileArgs(raw)
	if err != nil {
		return "", err
	}

	path, err := t.deps.resolvePath(ctx, a.Path)
	if err != nil {
		return "", fmt.Errorf("edit_file: %w", err)
	}
	if err := t.deps.checkBoundary(ctx, path); err != nil {
		return "", fmt.Errorf("edit_file: %w", err)
	}

	// Per-path lock: serialise all concurrent writes to this path.
	unlock := lockPath(path)
	defer unlock()

	if err := t.editFilePreconditions(ctx, path, a); err != nil {
		return "", err
	}

	// Anchor mode resolves its start_anchor/end_anchor pair into a single
	// synthetic str_replace edit against the current bytes, then flows through
	// the exact same write path as the edits array (locks, retry, LSP notify,
	// cache invalidation, diff). Only the span computation is new.
	var anchorNote string
	if a.anchorMode() {
		edit, note, err := t.resolveAnchorEdit(path, a)
		if err != nil {
			return "", err
		}
		a.Edits = []strEdit{edit}
		if note != "" {
			anchorNote = "\n" + note
		}
	}

	uri := "file://" + path
	// Captured before the write (which bumps the mtime): warn if the file moved
	// on disk since this session read it and no explicit version guard governs it.
	staleNote := t.staleReadNote(ctx, path, a)

	if a.ApplyPartial {
		t.deps.notifyTopology(path)
		return t.executePartial(ctx, path, a.Edits, uri, a.AwaitDiagnostics) + anchorNote + staleNote + t.deps.reportQuality(ctx, path), nil
	}
	result, err := t.editFileApply(ctx, path, a, uri)
	if err != nil {
		return "", err
	}
	t.deps.notifyTopology(path)
	return result + anchorNote + staleNote + t.deps.reportQuality(ctx, path), nil
}

// staleReadNote returns a one-line warning when this session read the file and it
// has since changed on disk, but the caller passed no explicit expected_mtime/
// expected_sha (and is not reconciling). The str_replace anchor already protects
// the edited region from corruption, so this is informational, not a refusal:
// the surrounding file may have moved under the caller (e.g. an entry landing in
// a section a peer just re-versioned). Returns "" when nothing changed, the file
// was never read this session, or an explicit guard already governs staleness.
func (t *EditFile) staleReadNote(ctx context.Context, path string, a editFileArgs) string {
	if a.ExpectedMtime != "" || a.ExpectedSha != "" || a.Reconcile {
		return ""
	}
	if !changedSinceSessionRead(t.deps.reads(ctx), path) {
		return ""
	}
	return "\n# plumb-warn: this file changed on disk since your session last read it — " +
		"your edit applied against the newer content (the old_string match protected the edited " +
		"region, but surrounding context may have moved); re-read before further edits"
}

func parseEditFileArgs(raw json.RawMessage) (editFileArgs, error) {
	var a editFileArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		// Some MCP clients intermittently double-encode the typed `edits` array
		// as a JSON string ("[{...}]") rather than a JSON array, which fails the
		// normal decode with an opaque "cannot unmarshal string into ... edits".
		// Recover that one shape instead of forcing the agent to retry blindly.
		if recovered, ok := recoverStringEncodedEdits(raw); ok {
			a = recovered
		} else {
			return a, fmt.Errorf("edit_file: invalid arguments: %w", err)
		}
	}
	if a.Path == "" {
		return a, errors.New("edit_file: file_path is required")
	}
	if err := a.validateMode(); err != nil {
		return a, err
	}
	return a, nil
}

// recoverStringEncodedEdits handles the client-side bug where `edits` arrives as
// a JSON string holding the array, rather than the array itself. It re-decodes
// the file_path/etc. fields normally and unwraps the stringified edits once.
// Returns ok=false if the input is malformed for any other reason.
func recoverStringEncodedEdits(raw json.RawMessage) (editFileArgs, bool) {
	var shadow struct {
		editFileArgs
		Edits json.RawMessage `json:"edits"`
	}
	if err := json.Unmarshal(raw, &shadow); err != nil {
		return editFileArgs{}, false
	}
	var encoded string
	if err := json.Unmarshal(shadow.Edits, &encoded); err != nil {
		return editFileArgs{}, false
	}
	var edits []strEdit
	if err := json.Unmarshal([]byte(encoded), &edits); err != nil {
		return editFileArgs{}, false
	}
	a := shadow.editFileArgs
	a.Edits = edits
	return a, true
}

// editFilePreconditions runs the dirty-check, optimistic-concurrency, and
// strict-mode gates before any read or write.
func (t *EditFile) editFilePreconditions(ctx context.Context, path string, a editFileArgs) error {
	if a.FailOnNewErrors {
		if a.ApplyPartial {
			return badArgument(&editLogicErr{errors.New(
				"edit_file: fail_on_new_errors cannot be combined with apply_partial — " +
					"apply_partial deliberately lets some edits land while others fail, which is the opposite of the " +
					"all-or-nothing guarantee fail_on_new_errors makes. Pick one")})
		}
		if err := failOnNewErrorsPrecheck("edit_file", path); err != nil {
			return &editLogicErr{err}
		}
	}
	if !a.DirtyOk && dirtyBlocksWrite(ctx, t.deps, path) {
		return dirtyWrite(&editLogicErr{fmt.Errorf("edit_file: %q has uncommitted changes; "+
			"review and commit first, or pass dirty_ok: true to proceed", path)})
	}
	if err := checkExpectedVersion(path, a, t.isStrict(), t.deps.reads(ctx)); err != nil {
		return err
	}
	return t.checkStrictRead(ctx, path)
}

// checkExpectedVersion enforces the optional optimistic-concurrency guards
// (expected_mtime / expected_sha) via the shared verifyExpectedVersion, wrapping
// a failure as an edit-logic error so the retry loop never re-attempts it. Both
// guards are skipped when reconcile is set, so the edit applies against current
// content relying on the exact-once match.
//
// A mismatch is NOT auto-reconciled: an explicit version guard is deliberate
// optimistic-concurrency control, and a stale guard whose anchor still matches a
// peer's concurrent change is exactly the clobber the guard exists to prevent
// (see TestMultiSession_StaleExpectedMtime_Rejected). Instead, for an all-anchor-based
// batch the rejection is made actionable: it names reconcile: true as the
// one-call escape hatch for the single-agent edit→format→edit loop, so the agent
// recovers without a blind retry while the safety contract stays intact. The
// hint is suppressed in strict mode, where reconcile alone is NOT enough —
// checkStrictRead would still demand a fresh read — so pointing at reconcile there
// would just cost an extra failed round-trip.
func checkExpectedVersion(path string, a editFileArgs, strict bool, reads *ReadTracker) error {
	if a.Reconcile {
		return nil
	}
	err := verifyExpectedVersion("edit_file", path, a.ExpectedMtime, a.ExpectedSha, reads)
	if err == nil {
		return nil
	}
	if !strict && allAnchorBased(a.Edits) {
		err = fmt.Errorf("%w\n"+
			"  If only a formatter changed the file since your read (the edit→format→edit loop), "+
			"pass reconcile: true to apply against the current content — the exact-once old_string "+
			"match keeps the edit safe; if a peer may have changed it, re-read instead", err)
	}
	return &editLogicErr{err}
}

// allAnchorBased reports whether every edit uses an exact-once old_string anchor
// (str_replace mode) rather than a line range. Only such a batch is safe to
// reconcile — the unique match proves the region is intact — so the reconcile
// hint is offered only then.
func allAnchorBased(edits []strEdit) bool {
	for _, e := range edits {
		if e.StartLine != 0 || e.OldStr == "" {
			return false
		}
	}
	return len(edits) > 0
}

// checkStrictRead enforces strict mode: the file must have been read in this
// session and not changed since. A no-op when strict mode is off. The failure is
// wrapped as an edit-logic error so the retry loop never re-attempts it.
func (t *EditFile) checkStrictRead(ctx context.Context, path string) error {
	if !t.isStrict() {
		return nil
	}
	if err := requireStrictRead(t.deps.reads(ctx), "edit_file", path); err != nil {
		return &editLogicErr{err}
	}
	return nil
}
