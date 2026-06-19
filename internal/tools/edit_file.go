package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"
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
      "description": "Absolute path, file:// URI, or workspace-relative path of the file to edit."
    },
    "edits": {
      "type": "array",
      "description": "Ordered list of str_replace edits to apply sequentially.",
      "items": {
        "type": "object",
        "properties": {
          "old_string": {
            "type": "string",
            "description": "Exact string to find. Required when start_line is not set. Must appear EXACTLY ONCE in the current file content — edit is rejected if absent or ambiguous. CRLF / LF differences are tolerated automatically."
          },
          "new_string": {
            "type": "string",
            "description": "Replacement text. Use empty string to delete. When start_line is set, replaces the specified line range (or appends at end of file when start_line is -1)."
          },
          "start_line": {
            "type": "integer",
            "description": "First line to replace (1-based, inclusive). When set, old_string is not used. Use -1 to append new_string at end of file. Use end_line: -1 to extend the range to the last line."
          },
          "end_line": {
            "type": "integer",
            "description": "Last line to replace (1-based, inclusive). Defaults to start_line when absent (single-line operation). Use -1 for end of file. Only used when start_line is set."
          },
          "replace_all": {
            "type": "boolean",
            "description": "str_replace mode only: when true, replace EVERY occurrence of old_string instead of requiring it to appear exactly once. Use for mechanical rename-this-token-everywhere edits. Ignored in range mode (start_line set). Default false."
          }
        },
        "required": ["new_string"],
        "additionalProperties": false
      },
      "minItems": 1
    },
    "expected_mtime": {
      "type": "string",
      "description": "Optional. RFC3339Nano mtime previously returned by read_file. If provided, the edit is rejected if the file's current mtime differs — fast optimistic-concurrency check."
    },
    "expected_sha": {
      "type": "string",
      "description": "Optional. Hex-encoded SHA-256 previously returned by read_file. If provided, the edit is rejected if the file's current content hash differs — stronger than expected_mtime, survives mtime aliasing."
    },
    "dirty_ok": {
      "type": "boolean",
      "description": "Allow editing a file that has uncommitted changes in its git repository. Default false — the edit is refused if the target file is dirty. Pass true to proceed anyway."
    },
    "apply_partial": {
      "type": "boolean",
      "description": "When true, apply each edit independently and continue on failure instead of rolling back the entire batch. Returns a per-edit result list showing which edits succeeded and which failed. Incompatible with strict mode — not safe when concurrent agents share the file."
    },
    "await_diagnostics": {
      "type": "boolean",
      "description": "When true, block up to a few seconds for the language server to finish re-analysing this file and report an authoritative post-write result — a clean fresh pass is stated explicitly. Use it for a trustworthy \"did my change compile?\" answer instead of shelling out to a build. Default false (fast adaptive window; the result may predate the write)."
    },
    "reconcile": {
      "type": "boolean",
      "description": "When true, do NOT reject the edit if the file changed since your read (expected_mtime / expected_sha mismatch); apply against the current on-disk content instead, relying on the exact-once old_string match for safety. Use it for the edit→format(gofumpt/golangci-lint --fix)→edit loop, where a formatter bumped the mtime but your anchors still match. Default false (the mtime guard stays strict)."
    }
  },
  "required": ["file_path", "edits"],
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
func (t *EditFile) isStrict() bool {
	if t.deps.Strict != nil {
		return t.deps.Strict()
	}
	return strictModeEnabled()
}

func (*EditFile) Name() string                 { return "edit_file" }
func (*EditFile) InputSchema() json.RawMessage { return editFileSchema }
func (*EditFile) Description() string {
	return "Apply one or more edits to an existing file (use this over a native edit tool — see the " +
		"Edit lane note in session_start). Each edit is one of two modes: " +
		"(1) str_replace (default): old_string must appear EXACTLY ONCE — rejected if absent or ambiguous. " +
		"(2) range: start_line/end_line (1-based) replace that line span with new_string; start_line: -1 " +
		"appends at end of file, end_line: -1 runs through the last line (the clean way to delete a block " +
		"or append, no anchor needed). " +
		"CRLF is tolerated; edits apply sequentially in memory then write atomically (temp + rename) under " +
		"a per-path lock. Pass expected_mtime (from a read_file header) to guarantee the file is unchanged " +
		"since you read it. For a SOLE agent doing a burst of sequential edits to one file, OMITTING " +
		"expected_mtime is the blessed fast path: the EXACTLY-ONCE old_string match is itself the safety " +
		"check, so you need not thread the fresh mtime each edit returns through the next one (reach for " +
		"expected_mtime/expected_sha only when a concurrent writer may touch the file). If the call fails " +
		"with a transport/connection error, the atomic temp+rename guarantees the file is either fully " +
		"updated or untouched — never partially written; re-read to see which."
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
	ExpectedMtime    string    `json:"expected_mtime"`
	ExpectedSha      string    `json:"expected_sha"`
	DirtyOk          bool      `json:"dirty_ok"`
	ApplyPartial     bool      `json:"apply_partial"`
	AwaitDiagnostics bool      `json:"await_diagnostics"`
	Reconcile        bool      `json:"reconcile"`
}

func (t *EditFile) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	if !t.deps.Limiter.Allow() {
		return "", rateLimitError("edit_file", t.deps.Limiter)
	}
	a, err := parseEditFileArgs(raw)
	if err != nil {
		return "", err
	}

	path := t.deps.resolvePath(a.Path)
	if err := t.deps.checkBoundary(path); err != nil {
		return "", fmt.Errorf("edit_file: %w", err)
	}

	// Per-path lock: serialise all concurrent writes to this path.
	unlock := lockPath(path)
	defer unlock()

	if err := t.editFilePreconditions(ctx, path, a); err != nil {
		return "", err
	}

	uri := "file://" + path
	// Captured before the write (which bumps the mtime): warn if the file moved
	// on disk since this session read it and no explicit version guard governs it.
	staleNote := t.staleReadNote(path, a)

	if a.ApplyPartial {
		t.deps.notifyTopology(path)
		return t.executePartial(ctx, path, a.Edits, uri, a.AwaitDiagnostics) + staleNote + t.deps.reportQuality(ctx, path), nil
	}
	result, err := t.editFileApply(ctx, path, a, uri)
	if err != nil {
		return "", err
	}
	t.deps.notifyTopology(path)
	return result + staleNote + t.deps.reportQuality(ctx, path), nil
}

// staleReadNote returns a one-line warning when this session read the file and it
// has since changed on disk, but the caller passed no explicit expected_mtime/
// expected_sha (and is not reconciling). The str_replace anchor already protects
// the edited region from corruption, so this is informational, not a refusal:
// the surrounding file may have moved under the caller (e.g. an entry landing in
// a section a peer just re-versioned). Returns "" when nothing changed, the file
// was never read this session, or an explicit guard already governs staleness.
func (t *EditFile) staleReadNote(path string, a editFileArgs) string {
	if a.ExpectedMtime != "" || a.ExpectedSha != "" || a.Reconcile {
		return ""
	}
	if !changedSinceSessionRead(t.deps.Reads, path) {
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
		return a, fmt.Errorf("edit_file: file_path is required")
	}
	if len(a.Edits) == 0 {
		return a, fmt.Errorf("edit_file: at least one edit is required")
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
	if !a.DirtyOk && dirtyBlocksWrite(ctx, t.deps.Writes, path) {
		return &editLogicErr{fmt.Errorf("edit_file: %q has uncommitted changes; "+
			"review and commit first, or pass dirty_ok: true to proceed", path)}
	}
	if err := checkExpectedVersion(path, a); err != nil {
		return err
	}
	return t.checkStrictRead(path)
}

// checkExpectedVersion enforces the optional optimistic-concurrency guards
// (expected_mtime / expected_sha) via the shared verifyExpectedVersion, wrapping
// a failure as an edit-logic error so the retry loop never re-attempts it. Both
// guards are skipped when reconcile is set, so the edit applies against current
// content relying on the exact-once match.
func checkExpectedVersion(path string, a editFileArgs) error {
	if a.Reconcile {
		return nil
	}
	if err := verifyExpectedVersion("edit_file", path, a.ExpectedMtime, a.ExpectedSha); err != nil {
		return &editLogicErr{err}
	}
	return nil
}

// checkStrictRead enforces strict mode: the file must have been read in this
// session and not changed since. A no-op when strict mode is off.
func (t *EditFile) checkStrictRead(path string) error {
	if !t.isStrict() {
		return nil
	}
	recorded := t.deps.Reads.Mtime(path)
	if recorded.IsZero() {
		return &editLogicErr{fmt.Errorf(
			"edit_file: strict mode: %q has not been read in this daemon session — call read_file first",
			path,
		)}
	}
	info, err := os.Stat(path)
	if err != nil {
		return &editLogicErr{fmt.Errorf("edit_file: stat %q: %w", path, err)}
	}
	if !info.ModTime().Equal(recorded) {
		return &editLogicErr{fmt.Errorf(
			"edit_file: strict mode: %q has changed since you read it\n"+
				"  recorded mtime: %s\n"+
				"  current mtime:  %s\n"+
				"  Re-read the file and try again",
			path, recorded.Format(time.RFC3339Nano), info.ModTime().Format(time.RFC3339Nano),
		)}
	}
	return nil
}
