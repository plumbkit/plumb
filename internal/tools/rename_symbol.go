package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/cache"
	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/paths"
)

const renameStaleIndexHint = `

This usually means the LSP position index is stale after recent in-session edits.
The language server computed edit positions against an older file version.

Recovery options:
  1. Call diagnostics to confirm the language server has re-indexed, then retry rename_symbol.
  2. Fall back to find_replace for the fully-qualified name (e.g. "pkg.OldName"), then fix
     bare-name references in comments and doc strings manually.
  3. Restart the daemon with "plumb stop" if re-indexing does not resolve the issue.`

// RenameSymbol performs a workspace-wide rename via LSP. The language server
// computes all call-site updates safely (across files, respecting scope and
// types). Plumb applies the resulting WorkspaceEdit atomically to disk.
//
// When the language server cannot compute the rename (an error, or an empty
// edit set — common with sourcekit-lsp before the build graph resolves), the
// tool returns actionable guidance, and — only when the caller opts in with
// structural_fallback:true — a best-effort, identifier-boundary text rename via
// the find_replace engine (dry-run by default; not scope-aware).
type RenameSymbol struct {
	client   lsp.Client
	timeout  time.Duration
	guard    BoundaryGuard
	ws       WorkspaceFn  // may be nil; anchors a workspace-relative input uri to the pinned root
	cache    *cache.Cache // may be nil; evicted per modified file after a successful apply
	fallback *findReplaceTool
	showDiff func() bool // may be nil; resolves the show_write_diff toggle (defaults on)
	deps     WriteDeps
	hasDeps  bool
}

func NewRenameSymbol(client lsp.Client, timeout time.Duration) *RenameSymbol {
	return &RenameSymbol{client: client, timeout: timeout}
}

// WithCache wires the session symbol cache so a successful rename evicts every
// modified file's entries (parity with edit_file/write_file). Nil-safe.
func (t *RenameSymbol) WithCache(c *cache.Cache) *RenameSymbol {
	t.cache = c
	return t
}

func (t *RenameSymbol) WithBoundary(guard BoundaryGuard) *RenameSymbol {
	t.guard = guard
	return t
}

// WithWorkspace wires the pinned-workspace accessor so a relative input uri is
// resolved against the workspace root rather than the daemon's working
// directory. Nil-safe. Only the input uri is anchored; the server-emitted
// WorkspaceEdit URIs are already absolute and are left untouched.
func (t *RenameSymbol) WithWorkspace(ws WorkspaceFn) *RenameSymbol {
	t.ws = ws
	return t
}

// WithStructuralFallback enables the opt-in structural rename fallback by wiring
// a find_replace engine with the write-capable deps. Nil-safe to omit (the
// fallback is then simply unavailable and the tool says so). The fallback only
// ever runs when the caller passes structural_fallback:true AND the LSP rename
// could not be computed.
func (t *RenameSymbol) WithStructuralFallback(deps WriteDeps) *RenameSymbol {
	t.fallback = NewFindReplace(deps)
	return t
}

// WithShowWriteDiff wires the per-session show_write_diff resolver. Nil-safe;
// when unset (e.g. in tests) the diff defaults on, matching the config default.
func (t *RenameSymbol) WithShowWriteDiff(fn func() bool) *RenameSymbol {
	t.showDiff = fn
	return t
}

func (t *RenameSymbol) WithWriteDeps(deps WriteDeps) *RenameSymbol {
	t.deps = deps
	t.hasDeps = true
	if deps.Cache != nil {
		t.cache = deps.Cache
	}
	return t
}

func (*RenameSymbol) Name() string { return "rename_symbol" }

func (*RenameSymbol) Description() string {
	return `No native Claude Code equivalent. Rename a symbol throughout the workspace using LSP semantic refactoring.

The language server identifies every reference across all files and applies a precise edit set atomically. Safer than text find-and-replace: it understands scope, shadowing, and types, so it won't rename unrelated identifiers that share the name.

Prefer symbol_name to identify the symbol; plumb resolves it through the document-symbol tree and queries the language server at the exact identifier position. Raw line/character remains supported and recovers from narrow "no identifier" misses by snapping once to the enclosing symbol. Runs in dry_run mode by default; set dry_run=false to apply. The response appends a per-file unified diff (a preview in dry-run, the applied change otherwise), capped at 20 files, unless show_write_diff is disabled.

If the language server cannot compute the rename (an error, or an empty edit set — common with sourcekit-lsp before the build graph resolves), pass structural_fallback=true to attempt a best-effort identifier-boundary text rename via find_replace (still dry_run by default). The fallback is NOT scope-aware — it renames every whole-word occurrence in same-extension files — so review the preview before applying.`
}

func (*RenameSymbol) InputSchema() json.RawMessage {
	return json.RawMessage(`{
			"type":"object",
			"properties":{
				"uri":{"type":"string","description":"Absolute path, file:// URI, or workspace-relative path."},
				"line":{"type":"integer","minimum":0,"description":"Zero-based line of the identifier. Required when symbol_name is not provided."},
				"character":{"type":"integer","minimum":0,"description":"Zero-based character offset within the line. Required when symbol_name is not provided."},
				"symbol_name":{"type":"string","description":"Symbol name to rename instead of a raw position — PREFERRED over line/character. Accepts plain name or ReceiverType.MethodName form. When provided, line and character are not needed."},
				"new_name":{"type":"string","description":"Replacement identifier name."},
				"dirty_ok":{"type":"boolean","default":false,"description":"Allow editing target files with uncommitted changes. Default false — review/commit first, or pass true to proceed."},
				"dry_run":{"type":"boolean","default":true,"description":"If true (default), preview changes only."},
				"structural_fallback":{"type":"boolean","default":false,"description":"If true, and the language server cannot compute the rename, attempt a best-effort, identifier-boundary text rename via find_replace (NOT scope-aware; honours dry_run). Default false."}
			},
			"required":["uri","new_name"],
	  "additionalProperties": false
	}`)
}

type renameSymbolArgs struct {
	URI                string
	Line               *uint32
	Character          *uint32
	SymbolName         string
	NewName            string
	DryRun             bool
	DirtyOK            bool
	StructuralFallback bool
}

func parseRenameSymbolArgs(raw json.RawMessage) (renameSymbolArgs, error) {
	var input struct {
		URI                string  `json:"uri"`
		Line               *uint32 `json:"line"`
		Character          *uint32 `json:"character"`
		SymbolName         string  `json:"symbol_name"`
		NewName            string  `json:"new_name"`
		DryRun             *bool   `json:"dry_run,omitempty"`
		DirtyOK            bool    `json:"dirty_ok"`
		StructuralFallback bool    `json:"structural_fallback"`
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return renameSymbolArgs{}, fmt.Errorf("invalid args: %w", err)
	}
	if input.URI == "" || input.NewName == "" {
		return renameSymbolArgs{}, fmt.Errorf("`uri` and `new_name` are required")
	}
	dryRun := true
	if input.DryRun != nil {
		dryRun = *input.DryRun
	}
	return renameSymbolArgs{
		URI:                input.URI,
		Line:               input.Line,
		Character:          input.Character,
		SymbolName:         input.SymbolName,
		NewName:            input.NewName,
		DryRun:             dryRun,
		DirtyOK:            input.DirtyOK,
		StructuralFallback: input.StructuralFallback,
	}, nil
}

// collectRenameTargets walks both Changes and DocumentChanges in we, boundary-
// checks every output URI before any edit is applied, and returns the unique
// file list plus the total edit count for the response.
//
// The list must be deduplicated: applyWorkspaceEditDetailed groups its plans by
// URI, so a server that names one file in both Changes and DocumentChanges (or
// twice in DocumentChanges — nothing in the protocol forbids it) yields one plan
// but would otherwise yield two entries here. captureRenameBaselines caps this
// list and postWriteRename caps the plans, and the two prefixes must name the
// same files or a reported file silently loses its pre-write baseline.
func (t *RenameSymbol) collectRenameTargets(we *protocol.WorkspaceEdit) ([]string, int, error) {
	totalEdits := 0
	files := []string{}
	seen := make(map[string]bool)
	add := func(uri string, edits int) error {
		path := paths.URIToPath(uri)
		if err := t.guard.check(path); err != nil {
			return fmt.Errorf("rename_symbol: %w", err)
		}
		totalEdits += edits
		if !seen[path] {
			seen[path] = true
			files = append(files, path)
		}
		return nil
	}
	for uri, edits := range we.Changes {
		if err := add(uri, len(edits)); err != nil {
			return nil, 0, err
		}
	}
	for _, dce := range we.DocumentChanges {
		if err := add(dce.TextDocument.URI, len(dce.Edits)); err != nil {
			return nil, 0, err
		}
	}
	return files, totalEdits, nil
}

// Execute resolves the rename with the language server, then applies the edit
// set it returns.
//
// Note the ordering: unlike the single-file symbol-edit tools — which take the
// target's path lock and only THEN resolve the symbol's range, so the range and
// the bytes it addresses cannot drift apart — a rename must ask the server for
// the whole WorkspaceEdit BEFORE it knows which files to lock. That is inherent
// to an LSP rename, and it leaves a window in which a concurrent plumb-side write
// can move the text under a range the server has already computed. Drift that
// pushes a range past end-of-file is caught by the overshoot guard in
// endOffsetForPosition and surfaces as the stale-index error below; drift that
// stays in range is NOT detected, and the rename applies to whatever now sits at
// those coordinates. Do not assume this path has the single-file tools'
// under-lock re-resolve.
func (t *RenameSymbol) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	a, err := parseRenameSymbolArgs(args)
	if err != nil {
		return "", err
	}
	a.URI = toFileURIAnchored(a.URI, t.ws)
	if err := t.guard.check(paths.URIToPath(a.URI)); err != nil {
		return "", fmt.Errorf("rename_symbol: %w", err)
	}

	dctx, cancel := withLSPDeadline(ctx, t.timeout)
	defer cancel()

	we, note, err := t.renameWorkspaceEdit(dctx, a)
	if err != nil {
		var pre preLSPErr
		if errors.As(err, &pre) {
			return "", pre.err
		}
		return t.onRenameUnavailable(ctx, a, "the language server returned an error", err)
	}
	if we == nil || (len(we.Changes) == 0 && len(we.DocumentChanges) == 0) {
		return t.onRenameEmpty(ctx, a)
	}
	return t.applyOrPreview(ctx, a, we, note)
}

func (t *RenameSymbol) renameWorkspaceEdit(ctx context.Context, a renameSymbolArgs) (*protocol.WorkspaceEdit, string, error) {
	if a.SymbolName != "" {
		return t.renameByName(ctx, a)
	}
	if a.Line == nil || a.Character == nil {
		return nil, "", preLSPErr{fmt.Errorf("rename_symbol: either symbol_name or both line and character are required")}
	}
	return t.renameByPosition(ctx, a, *a.Line, *a.Character, true)
}

func (t *RenameSymbol) renameByName(ctx context.Context, a renameSymbolArgs) (*protocol.WorkspaceEdit, string, error) {
	syms, err := t.client.DocumentSymbols(ctx, protocol.DocumentSymbolParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: a.URI},
	})
	if err != nil {
		return nil, "", lspTimeoutErr("rename_symbol", t.timeout, fmt.Errorf("resolving symbol %q: %w", a.SymbolName, err))
	}
	matches := resolveSymbolsByName(syms, a.SymbolName)
	if len(matches) == 0 {
		return nil, "", preLSPErr{fmt.Errorf("rename_symbol: no symbol named %q in %s", a.SymbolName, a.URI)}
	}
	if len(matches) > 1 {
		return nil, "", preLSPErr{fmt.Errorf("rename_symbol: %d symbols named %q in %s; use line/character to disambiguate", len(matches), a.SymbolName, a.URI)}
	}
	sym := matches[0]
	return t.renameByPosition(ctx, a, sym.SelectionRange.Start.Line, sym.SelectionRange.Start.Character, false)
}

// renameLSPErr renders a language-server rename failure for the caller that
// actually made the call. A caller who passed symbol_name never supplied
// coordinates, so positionErr's "line and character are 0-based" hint would send
// it to inspect an argument it did not pass; the position came from plumb's own
// resolution of the document-symbol tree, and the server rejecting it points at a
// stale index instead.
func renameLSPErr(a renameSymbolArgs, err error) error {
	return queryErr("rename_symbol", a.SymbolName, err)
}

// preLSPErr wraps an error raised before any language-server rename attempt —
// argument validation or plumb-side symbol resolution (no match, ambiguous
// match). Execute returns it verbatim: the structural fallback and the
// LSP-failure hint exist for language-server failures and would mislead here
// (an ambiguous symbol_name must be disambiguated, not text-renamed
// workspace-wide).
type preLSPErr struct{ err error }

func (e preLSPErr) Error() string { return e.err.Error() }
func (e preLSPErr) Unwrap() error { return e.err }

func (t *RenameSymbol) renameByPosition(ctx context.Context, a renameSymbolArgs, line, character uint32, allowSnap bool) (*protocol.WorkspaceEdit, string, error) {
	we, err := t.client.Rename(ctx, protocol.RenameParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: a.URI},
		Position:     protocol.Position{Line: line, Character: character},
		NewName:      a.NewName,
	})
	if err == nil {
		return we, "", nil
	}
	if allowSnap && isPositionMissErr(err) {
		snapped, syms, ok := snapPosition(ctx, t.client, a.URI, line)
		if !ok {
			return nil, "", positionMissErr("rename_symbol", a.URI, line, syms)
		}
		we, _, retryErr := t.renameByPosition(ctx, a, snapped.Line, snapped.Character, false)
		if retryErr != nil {
			return nil, "", retryErr
		}
		return we, snapNotice(a.URI, line, character, snapped.Line), nil
	}
	return nil, "", renameLSPErr(a, err)
}

// applyOrPreview applies (or previews, in dry-run) a server-computed edit set.
func (t *RenameSymbol) applyOrPreview(ctx context.Context, a renameSymbolArgs, we *protocol.WorkspaceEdit, note string) (string, error) {
	files, totalEdits, err := t.collectRenameTargets(we)
	if err != nil {
		return "", err
	}
	sort.Strings(files)
	if err := t.preflightTargets(ctx, files, a); err != nil {
		return "", err
	}

	// Reconstruct the diff against the current on-disk content BEFORE applying,
	// so the read bytes are the true "before" in both the dry-run and apply paths.
	diff := ""
	if resolveShowDiff(t.showDiff) {
		diff = renameFileDiffs(we, files)
	}

	var sb strings.Builder
	if note != "" {
		sb.WriteString(note)
	}
	verb := "would change"
	var diagOut strings.Builder
	if !a.DryRun {
		baselines := t.captureRenameBaselines(files)
		modified, plans, applyErr := applyWorkspaceEditDetailed(we, t.recordRenameWrites)
		if applyErr != nil {
			if strings.Contains(applyErr.Error(), "out of range") {
				return "", fmt.Errorf("applying rename: %w%s", applyErr, renameStaleIndexHint)
			}
			return "", fmt.Errorf("applying rename: %w", applyErr)
		}
		t.postWriteRename(ctx, plans, baselines, &diagOut)
		files = modified
		verb = "changed"
	} else {
		sb.WriteString("DRY RUN — no files modified.\n\n")
	}
	fmt.Fprintf(&sb, "Renamed to %q across %d file(s), %d edit(s) %s:\n\n",
		a.NewName, len(files), totalEdits, verb)
	for _, f := range files {
		fmt.Fprintf(&sb, "  %s\n", f)
	}
	if diff != "" {
		sb.WriteString("\n")
		sb.WriteString(diff)
		sb.WriteString("\n")
	}
	if diagOut.Len() > 0 {
		sb.WriteString(diagOut.String())
	}
	if a.DryRun {
		sb.WriteString("\nTo apply, re-run with dry_run=false.")
	}
	return sb.String(), nil
}

// preflightTargets gates an about-to-be-applied rename: the write-rate budget
// once, then the workspace boundary and the dirty guard for every file the
// server's edit set touches.
//
// Strict mode ([edits] strict = true) is deliberately NOT enforced here, unlike
// on the symbol-edit tools (semanticWritePreflight) and edit_file. Strict mode
// asks "has this session read the file it is about to author content into?" —
// and a rename authors no content: the new text is one identifier, and the file
// set is chosen by the language server, not the agent. Demanding a prior
// read_file of every file in a 40-file rename would make the tool unusable in
// strict mode; demanding it of only the anchor file would be a half-guarantee
// that protects nothing (the anchor's range is re-resolved server-side anyway).
// The dirty guard above is what stops a rename from clobbering unreviewed work.
func (t *RenameSymbol) preflightTargets(ctx context.Context, files []string, a renameSymbolArgs) error {
	if a.DryRun {
		return nil
	}
	deps := writeDepsPtr(t.hasDeps, &t.deps)
	if deps != nil && deps.Limiter != nil && !deps.Limiter.Allow() {
		return rateLimitError("rename_symbol", deps.Limiter)
	}
	for _, f := range files {
		if err := t.guard.check(f); err != nil {
			return fmt.Errorf("rename_symbol: %w", err)
		}
		if deps != nil && !a.DirtyOK && dirtyBlocksWrite(ctx, *deps, f) {
			return fmt.Errorf("rename_symbol: %q has uncommitted changes; review and commit first, or pass dirty_ok: true to proceed", f)
		}
	}
	return nil
}

// maxRenameReportFiles bounds how many of a multi-file rename's modified files
// get the expensive half of the post-write pipeline: the blocking diagnostics
// wait (up to post_write_diagnostics_ms each) and the quality analysers (one
// lint invocation each). Every modified file is still notified to the language
// server, cache-invalidated, and re-indexed — only the reporting is capped, so a
// 50-file rename costs one response's worth of waiting rather than fifty. The
// remainder is summarised with a pointer at diagnostics().
const maxRenameReportFiles = 5

// capReportFiles returns the leading maxRenameReportFiles entries of a sorted
// slice (or all of it, when shorter).
func capReportFiles[T any](s []T) []T {
	if len(s) > maxRenameReportFiles {
		return s[:maxRenameReportFiles]
	}
	return s
}

// captureRenameBaselines snapshots the pre-write language-server state for the
// files that will actually be reported on. A baseline is consumed only by the
// differential diagnostics block, and — with the cross-file sweep enabled — each
// one costs a whole-workspace diagnostics snapshot, so capturing one per
// modified file would pay for reports that are never rendered. files must be
// sorted: postWriteRename reports the same prefix.
func (t *RenameSymbol) captureRenameBaselines(files []string) map[string]*diagBaseline {
	deps := writeDepsPtr(t.hasDeps, &t.deps)
	if deps == nil {
		return nil
	}
	out := make(map[string]*diagBaseline, len(files))
	for _, f := range capReportFiles(files) {
		uri := protocol.FileURI(f)
		out[uri] = deps.capturePreWriteBaseline(uri)
	}
	return out
}

// recordRenameWrites runs inside applyWorkspaceEditDetailed after every file
// has been written but before the path locks release, honouring recordWritten
// and recordUndo's held-lock contract — recording after the unlock would let a
// concurrent session's write slip in between and have its undo snapshot and
// read-tracker state clobbered by ours.
func (t *RenameSymbol) recordRenameWrites(plans []workspaceEditPlan) {
	deps := writeDepsPtr(t.hasDeps, &t.deps)
	if deps == nil {
		return
	}
	for _, p := range plans {
		deps.recordWritten(p.path)
		deps.recordUndo(p.path, string(p.before), string(p.after), true, "rename_symbol")
	}
}

// postWriteRename runs the post-write pipeline over a rename's modified files in
// two passes. Every file is notified first — LSP, adapter hook, cache eviction,
// topology — and none of that blocks, so by the time the reporting pass waits on
// the server it has already heard about the whole edit set and the per-file
// waits overlap. Reporting is then capped at maxRenameReportFiles: it is the
// only blocking part, and it is what made a wide rename serialise a diagnostics
// wait plus a lint run for every file it touched.
func (t *RenameSymbol) postWriteRename(ctx context.Context, plans []workspaceEditPlan, baselines map[string]*diagBaseline, diagOut *strings.Builder) {
	deps := writeDepsPtr(t.hasDeps, &t.deps)
	for _, p := range plans {
		uri := protocol.FileURI(p.path)
		if deps == nil {
			notifySymbolEditWritten(ctx, t.client, t.cache, p.path, uri)
			continue
		}
		semanticNotifyWritten(ctx, deps, t.client, t.cache, p.path, uri, "rename_symbol")
	}
	if deps == nil {
		return
	}
	reported := capReportFiles(plans)
	for _, p := range reported {
		uri := protocol.FileURI(p.path)
		diagOut.WriteString(semanticPostWriteReport(ctx, deps, p.path, uri, string(p.before), string(p.after), baselines[uri]))
	}
	if len(plans) > len(reported) {
		fmt.Fprintf(diagOut, "\ndiagnostics and code quality reported for the first %d of %d modified file(s); call diagnostics() for the rest",
			len(reported), len(plans))
	}
}

// maxRenameDiffFiles caps the number of per-file diffs rendered in a
// rename_symbol response; files beyond it are summarised, not diffed.
const maxRenameDiffFiles = 20

// renameFileDiffs renders a per-file unified diff for the WorkspaceEdit by
// applying each file's TextEdits to a copy of its current on-disk bytes. It must
// run BEFORE the edits land on disk so the read content is the true "before".
// Files are rendered in the given (sorted) order, capped at maxRenameDiffFiles
// with an "and N more file(s)" summary. Best-effort: a file that can't be read
// or reconstructed is skipped (the rename itself is unaffected).
func renameFileDiffs(we *protocol.WorkspaceEdit, files []string) string {
	byPath := groupEditsByPath(we)
	limit := len(files)
	if limit > maxRenameDiffFiles {
		limit = maxRenameDiffFiles
	}
	var sb strings.Builder
	for _, path := range files[:limit] {
		d := symbolEditsDiff(path, byPath[path])
		if d == "" {
			continue
		}
		sb.WriteString(d)
		sb.WriteString("\n\n")
	}
	if len(files) > maxRenameDiffFiles {
		fmt.Fprintf(&sb, "… and %d more file(s) (diff omitted; use file_diff)", len(files)-maxRenameDiffFiles)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// groupEditsByPath collects every TextEdit in we keyed by filesystem path,
// merging the Changes and DocumentChanges forms (matching applyWorkspaceEdit).
func groupEditsByPath(we *protocol.WorkspaceEdit) map[string][]protocol.TextEdit {
	byPath := make(map[string][]protocol.TextEdit)
	for uri, edits := range we.Changes {
		p := paths.URIToPath(uri)
		byPath[p] = append(byPath[p], edits...)
	}
	for _, dce := range we.DocumentChanges {
		p := paths.URIToPath(dce.TextDocument.URI)
		byPath[p] = append(byPath[p], dce.Edits...)
	}
	return byPath
}
