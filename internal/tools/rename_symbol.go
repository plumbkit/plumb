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

	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
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
	ws       WorkspaceFn // may be nil; anchors a workspace-relative input uri to the pinned root
	fallback *findReplaceTool
}

func NewRenameSymbol(client lsp.Client, timeout time.Duration) *RenameSymbol {
	return &RenameSymbol{client: client, timeout: timeout}
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

func (*RenameSymbol) Name() string { return "rename_symbol" }

func (*RenameSymbol) Description() string {
	return `No native Claude Code equivalent. Rename a symbol throughout the workspace using LSP semantic refactoring.

The language server identifies every reference (across all files) and produces a precise edit set. Plumb applies the edits atomically. Safer than text-based find-and-replace because it understands scope, shadowing, and types — won't rename unrelated identifiers that happen to share the name.

Provide the cursor position on the identifier you want to rename. By default, runs in dry_run mode and returns a summary of files that would change. Set dry_run=false to actually apply the rename.

If the language server cannot compute the rename (an error, or an empty edit set — common with sourcekit-lsp before the project's build graph resolves), the tool returns actionable guidance. Pass structural_fallback=true to additionally attempt a best-effort, identifier-boundary text rename via find_replace (still dry_run by default). The fallback is NOT scope-aware — it renames every whole-word occurrence of the identifier in same-extension files — so review the preview before applying.`
}

func (*RenameSymbol) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"uri":{"type":"string","description":"Absolute path, file:// URI, or workspace-relative path."},
			"line":{"type":"integer","minimum":0,"description":"Zero-based line of the identifier."},
			"character":{"type":"integer","minimum":0,"description":"Zero-based character offset within the line."},
			"new_name":{"type":"string","description":"Replacement identifier name."},
			"dry_run":{"type":"boolean","default":true,"description":"If true (default), preview changes only."},
			"structural_fallback":{"type":"boolean","default":false,"description":"If true, and the language server cannot compute the rename, attempt a best-effort, identifier-boundary text rename via find_replace (NOT scope-aware; honours dry_run). Default false."}
		},
		"required":["uri","line","character","new_name"],
  "additionalProperties": false
}`)
}

type renameSymbolArgs struct {
	URI                string
	Line               uint32
	Character          uint32
	NewName            string
	DryRun             bool
	StructuralFallback bool
}

func parseRenameSymbolArgs(raw json.RawMessage) (renameSymbolArgs, error) {
	var input struct {
		URI                string `json:"uri"`
		Line               uint32 `json:"line"`
		Character          uint32 `json:"character"`
		NewName            string `json:"new_name"`
		DryRun             *bool  `json:"dry_run,omitempty"`
		StructuralFallback bool   `json:"structural_fallback"`
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
		NewName:            input.NewName,
		DryRun:             dryRun,
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
		if err := t.guard.check(strings.TrimPrefix(uri, "file://")); err != nil {
			return nil, 0, fmt.Errorf("rename_symbol: %w", err)
		}
		totalEdits += len(edits)
		files = append(files, strings.TrimPrefix(uri, "file://"))
	}
	for _, dce := range we.DocumentChanges {
		if err := t.guard.check(strings.TrimPrefix(dce.TextDocument.URI, "file://")); err != nil {
			return nil, 0, fmt.Errorf("rename_symbol: %w", err)
		}
		totalEdits += len(dce.Edits)
		files = append(files, strings.TrimPrefix(dce.TextDocument.URI, "file://"))
	}
	return files, totalEdits, nil
}

func (t *RenameSymbol) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	a, err := parseRenameSymbolArgs(args)
	if err != nil {
		return "", err
	}
	a.URI = toFileURIAnchored(a.URI, t.ws)
	if err := t.guard.check(strings.TrimPrefix(a.URI, "file://")); err != nil {
		return "", fmt.Errorf("rename_symbol: %w", err)
	}

	dctx, cancel := withLSPDeadline(ctx, t.timeout)
	defer cancel()

	we, err := t.client.Rename(dctx, protocol.RenameParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: a.URI},
		Position:     protocol.Position{Line: a.Line, Character: a.Character},
		NewName:      a.NewName,
	})
	if err != nil {
		return t.onRenameUnavailable(ctx, a, "the language server returned an error", positionErr("rename_symbol", err))
	}
	if we == nil || (len(we.Changes) == 0 && len(we.DocumentChanges) == 0) {
		return t.onRenameEmpty(ctx, a)
	}
	return t.applyOrPreview(a, we)
}

// onRenameUnavailable handles a failed LSP rename: it runs the structural
// fallback when the caller opted in and it is wired, otherwise returns the
// original error enriched with actionable guidance.
func (t *RenameSymbol) onRenameUnavailable(ctx context.Context, a renameSymbolArgs, reason string, baseErr error) (string, error) {
	if a.StructuralFallback && t.fallback != nil {
		return t.structuralFallback(ctx, a, reason)
	}
	oldName, _ := identifierAtFile(strings.TrimPrefix(a.URI, "file://"), a.Line, a.Character)
	return "", fmt.Errorf("%w%s", baseErr, renameLSPFailureHint(oldName, a.NewName, t.fallback != nil))
}

// onRenameEmpty handles an empty edit set: an opt-in structural fallback, or the
// informational message plus guidance.
func (t *RenameSymbol) onRenameEmpty(ctx context.Context, a renameSymbolArgs) (string, error) {
	if a.StructuralFallback && t.fallback != nil {
		return t.structuralFallback(ctx, a, "the language server returned an empty edit set")
	}
	oldName, _ := identifierAtFile(strings.TrimPrefix(a.URI, "file://"), a.Line, a.Character)
	return "No changes — rename returned an empty edit set (symbol may not be renameable here)." +
		renameLSPFailureHint(oldName, a.NewName, t.fallback != nil), nil
}

// applyOrPreview applies (or previews, in dry-run) a server-computed edit set.
func (t *RenameSymbol) applyOrPreview(a renameSymbolArgs, we *protocol.WorkspaceEdit) (string, error) {
	files, totalEdits, err := t.collectRenameTargets(we)
	if err != nil {
		return "", err
	}
	sort.Strings(files)

	var sb strings.Builder
	verb := "would change"
	if !a.DryRun {
		modified, applyErr := applyWorkspaceEdit(we)
		if applyErr != nil {
			if strings.Contains(applyErr.Error(), "out of range") {
				return "", fmt.Errorf("applying rename: %w%s", applyErr, renameStaleIndexHint)
			}
			return "", fmt.Errorf("applying rename: %w", applyErr)
		}
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
	if a.DryRun {
		sb.WriteString("\nTo apply, re-run with dry_run=false.")
	}
	return sb.String(), nil
}

// structuralFallback performs a best-effort, identifier-boundary text rename via
// the find_replace engine when the LSP could not. It resolves the old name from
// the position, then runs a word-boundary regex replace across same-extension
// files under the workspace, honouring the caller's dry_run.
func (t *RenameSymbol) structuralFallback(ctx context.Context, a renameSymbolArgs, reason string) (string, error) {
	path := strings.TrimPrefix(a.URI, "file://")
	oldName, err := identifierAtFile(path, a.Line, a.Character)
	if err != nil {
		return "", fmt.Errorf("rename_symbol: structural fallback could not resolve the symbol name at the position: %w", err)
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
