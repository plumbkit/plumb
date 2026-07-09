package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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
func (t *RenameSymbol) collectRenameTargets(we *protocol.WorkspaceEdit) ([]string, int, error) {
	totalEdits := 0
	files := []string{}
	for uri, edits := range we.Changes {
		if err := t.guard.check(paths.URIToPath(uri)); err != nil {
			return nil, 0, fmt.Errorf("rename_symbol: %w", err)
		}
		totalEdits += len(edits)
		files = append(files, paths.URIToPath(uri))
	}
	for _, dce := range we.DocumentChanges {
		if err := t.guard.check(paths.URIToPath(dce.TextDocument.URI)); err != nil {
			return nil, 0, fmt.Errorf("rename_symbol: %w", err)
		}
		totalEdits += len(dce.Edits)
		files = append(files, paths.URIToPath(dce.TextDocument.URI))
	}
	return files, totalEdits, nil
}

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
		return nil, "", fmt.Errorf("rename_symbol: either symbol_name or both line and character are required")
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
		return nil, "", fmt.Errorf("rename_symbol: no symbol named %q in %s", a.SymbolName, a.URI)
	}
	if len(matches) > 1 {
		return nil, "", fmt.Errorf("rename_symbol: %d symbols named %q in %s; use line/character to disambiguate", len(matches), a.SymbolName, a.URI)
	}
	sym := matches[0]
	return t.renameByPosition(ctx, a, sym.SelectionRange.Start.Line, sym.SelectionRange.Start.Character, false)
}

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
	return nil, "", positionErr("rename_symbol", err)
}

// onRenameUnavailable handles a failed LSP rename: it runs the structural
// fallback when the caller opted in and it is wired, otherwise returns the
// original error enriched with actionable guidance.
func (t *RenameSymbol) onRenameUnavailable(ctx context.Context, a renameSymbolArgs, reason string, baseErr error) (string, error) {
	if a.StructuralFallback && t.fallback != nil {
		return t.structuralFallback(ctx, a, reason)
	}
	oldName := t.oldNameForFailure(a)
	return "", fmt.Errorf("%w%s", baseErr, renameLSPFailureHint(oldName, a.NewName, t.fallback != nil))
}

// onRenameEmpty handles an empty edit set: an opt-in structural fallback, or the
// informational message plus guidance.
func (t *RenameSymbol) onRenameEmpty(ctx context.Context, a renameSymbolArgs) (string, error) {
	if a.StructuralFallback && t.fallback != nil {
		return t.structuralFallback(ctx, a, "the language server returned an empty edit set")
	}
	oldName := t.oldNameForFailure(a)
	return "No changes — rename returned an empty edit set (symbol may not be renameable here)." +
		renameLSPFailureHint(oldName, a.NewName, t.fallback != nil), nil
}

func (t *RenameSymbol) oldNameForFailure(a renameSymbolArgs) string {
	if a.SymbolName != "" {
		return a.SymbolName
	}
	if a.Line == nil || a.Character == nil {
		return ""
	}
	oldName, _ := identifierAtFile(paths.URIToPath(a.URI), *a.Line, *a.Character)
	return oldName
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
		modified, plans, applyErr := applyWorkspaceEditDetailed(we)
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

func (t *RenameSymbol) captureRenameBaselines(files []string) map[string]*diagBaseline {
	deps := writeDepsPtr(t.hasDeps, &t.deps)
	if deps == nil {
		return nil
	}
	out := make(map[string]*diagBaseline, len(files))
	for _, f := range files {
		uri := protocol.FileURI(f)
		out[uri] = deps.capturePreWriteBaseline(uri)
	}
	return out
}

func (t *RenameSymbol) postWriteRename(ctx context.Context, plans []workspaceEditPlan, baselines map[string]*diagBaseline, diagOut *strings.Builder) {
	deps := writeDepsPtr(t.hasDeps, &t.deps)
	for _, p := range plans {
		uri := protocol.FileURI(p.path)
		if deps == nil {
			notifySymbolEditWritten(ctx, t.client, t.cache, p.path, uri)
			continue
		}
		diagOut.WriteString(semanticPostWrite(ctx, deps, t.client, t.cache, p.path, uri, string(p.before), string(p.after), "rename_symbol", baselines[uri]))
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

// structuralFallback performs a best-effort, identifier-boundary text rename via
// the find_replace engine when the LSP could not. It resolves the old name from
// the position, then runs a word-boundary regex replace across same-extension
// files under the workspace, honouring the caller's dry_run.
func (t *RenameSymbol) structuralFallback(ctx context.Context, a renameSymbolArgs, reason string) (string, error) {
	path := paths.URIToPath(a.URI)
	oldName := a.SymbolName
	if oldName == "" {
		if a.Line == nil || a.Character == nil {
			return "", fmt.Errorf("rename_symbol: structural fallback requires symbol_name or both line and character")
		}
		var err error
		oldName, err = identifierAtFile(path, *a.Line, *a.Character)
		if err != nil {
			return "", fmt.Errorf("rename_symbol: structural fallback could not resolve the symbol name at the position: %w", err)
		}
	}

	root := ""
	if t.ws != nil {
		root = t.ws()
	}
	if root == "" {
		root = filepath.Dir(path)
	}
	glob := ""
	if ext := filepath.Ext(path); ext != "" {
		glob = "*" + ext
	}

	frArgs, err := json.Marshal(map[string]any{
		"path":           root,
		"pattern":        `\b` + regexp.QuoteMeta(oldName) + `\b`,
		"replacement":    a.NewName,
		"use_regex":      true,
		"case_sensitive": true,
		"glob":           glob,
		"dry_run":        a.DryRun,
	})
	if err != nil {
		return "", fmt.Errorf("rename_symbol: structural fallback: %w", err)
	}
	out, err := t.fallback.Execute(ctx, frArgs)
	if err != nil {
		return "", fmt.Errorf("rename_symbol: structural fallback failed: %w", err)
	}
	return structuralFallbackBanner(oldName, a.NewName, reason, glob) + out, nil
}

// structuralFallbackBanner prefixes the find_replace output with a loud,
// honest explanation that this is a non-scope-aware text rename.
func structuralFallbackBanner(oldName, newName, reason, glob string) string {
	scope := "all text files"
	if glob != "" {
		scope = glob + " files"
	}
	return fmt.Sprintf(
		"STRUCTURAL FALLBACK — rename_symbol could not use the language server (%s).\n"+
			"This is a best-effort, identifier-boundary text rename of %q → %q across %s under the workspace.\n"+
			"It is NOT scope-aware: a same-named identifier in another scope is also matched, so review every change.\n\n",
		reason, oldName, newName, scope)
}

// renameLSPFailureHint returns actionable recovery guidance for a rename the
// language server could not compute. hasFallback gates the structural_fallback
// suggestion so it is only offered when the fallback is actually wired.
func renameLSPFailureHint(oldName, newName string, hasFallback bool) string {
	var b strings.Builder
	b.WriteString("\n\nThe language server could not compute this rename. This is common when the project's ")
	b.WriteString("build graph is not fully resolved (e.g. sourcekit-lsp before a successful build, or mid-edit).\n")
	b.WriteString("Recovery options:\n")
	b.WriteString("  - Ensure the project builds (resolve dependencies / run a build), then retry rename_symbol.\n")
	if hasFallback {
		if oldName != "" {
			fmt.Fprintf(&b, "  - Re-run with structural_fallback:true for a best-effort identifier-boundary rename of %q → %q (dry-run first; review, then apply with dry_run:false).\n", oldName, newName)
		} else {
			b.WriteString("  - Re-run with structural_fallback:true for a best-effort identifier-boundary rename (dry-run first).\n")
		}
	}
	if oldName != "" {
		fmt.Fprintf(&b, "  - Or use find_references + edit_file, or find_replace on %q with a word boundary, fixing any scope collisions manually.\n", oldName)
	} else {
		b.WriteString("  - Or use find_references + edit_file to update each call site.\n")
	}
	return b.String()
}

// identifierAtFile reads path and returns the identifier token spanning the
// (line, character) position. Character is treated as a byte offset, matching
// plumb's LSP-position convention (correct for ASCII identifiers).
func identifierAtFile(path string, line, character uint32) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading %q: %w", path, err)
	}
	lines := strings.Split(string(data), "\n")
	if int(line) >= len(lines) {
		return "", fmt.Errorf("line %d is past the end of %q", line, path)
	}
	name := identifierAt(lines[line], int(character))
	if name == "" {
		return "", fmt.Errorf("no identifier at line %d, character %d", line, character)
	}
	return name, nil
}

// isIdentifierByte reports whether b is a [A-Za-z0-9_] identifier byte.
func isIdentifierByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// identifierAt returns the maximal [A-Za-z0-9_] run containing the byte index
// char (or the one immediately before it, since a cursor often sits just after
// a token), or "" when the position is not on an identifier.
func identifierAt(line string, char int) string {
	if char < 0 {
		return ""
	}
	pos := char
	if pos >= len(line) || !isIdentifierByte(line[pos]) {
		pos-- // the position may sit just after the identifier
	}
	if pos < 0 || pos >= len(line) || !isIdentifierByte(line[pos]) {
		return ""
	}
	start, end := pos, pos
	for start > 0 && isIdentifierByte(line[start-1]) {
		start--
	}
	for end < len(line) && isIdentifierByte(line[end]) {
		end++
	}
	return line[start:end]
}
