package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/plumbkit/plumb/internal/paths"
)

// Failure handling for rename_symbol: the opt-in structural (text) fallback,
// the actionable LSP-failure guidance, and the identifier resolution they
// share. Split from rename_symbol.go to keep it under the file-size cap.

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
