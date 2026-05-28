package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/golimpio/plumb/internal/lsp"
	"github.com/golimpio/plumb/internal/lsp/protocol"
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
type RenameSymbol struct {
	client  lsp.Client
	timeout time.Duration
	guard   BoundaryGuard
}

func NewRenameSymbol(client lsp.Client, timeout time.Duration) *RenameSymbol {
	return &RenameSymbol{client: client, timeout: timeout}
}

func (t *RenameSymbol) WithBoundary(guard BoundaryGuard) *RenameSymbol {
	t.guard = guard
	return t
}

func (*RenameSymbol) Name() string { return "rename_symbol" }

func (*RenameSymbol) Description() string {
	return `No native Claude Code equivalent. Rename a symbol throughout the workspace using LSP semantic refactoring.

The language server identifies every reference (across all files) and produces a precise edit set. Plumb applies the edits atomically. Safer than text-based find-and-replace because it understands scope, shadowing, and types — won't rename unrelated identifiers that happen to share the name.

Provide the cursor position on the identifier you want to rename. By default, runs in dry_run mode and returns a summary of files that would change. Set dry_run=false to actually apply the rename.`
}

func (*RenameSymbol) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"uri":{"type":"string","description":"Absolute path or file:// URI."},
			"line":{"type":"integer","minimum":0,"description":"Zero-based line of the identifier."},
			"character":{"type":"integer","minimum":0,"description":"Zero-based character offset within the line."},
			"new_name":{"type":"string","description":"Replacement identifier name."},
			"dry_run":{"type":"boolean","default":true,"description":"If true (default), preview changes only."}
		},
		"required":["uri","line","character","new_name"],
  "additionalProperties": false
}`)
}

type renameSymbolArgs struct {
	URI       string
	Line      uint32
	Character uint32
	NewName   string
	DryRun    bool
}

func parseRenameSymbolArgs(raw json.RawMessage) (renameSymbolArgs, error) {
	var input struct {
		URI       string `json:"uri"`
		Line      uint32 `json:"line"`
		Character uint32 `json:"character"`
		NewName   string `json:"new_name"`
		DryRun    *bool  `json:"dry_run,omitempty"`
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return renameSymbolArgs{}, fmt.Errorf("invalid args: %w", err)
	}
	if input.URI == "" || input.NewName == "" {
		return renameSymbolArgs{}, fmt.Errorf("`uri` and `new_name` are required")
	}
	input.URI = toFileURI(input.URI)
	dryRun := true
	if input.DryRun != nil {
		dryRun = *input.DryRun
	}
	return renameSymbolArgs{
		URI:       input.URI,
		Line:      input.Line,
		Character: input.Character,
		NewName:   input.NewName,
		DryRun:    dryRun,
	}, nil
}

func (t *RenameSymbol) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	a, err := parseRenameSymbolArgs(args)
	if err != nil {
		return "", err
	}
	if err := t.guard.check(strings.TrimPrefix(a.URI, "file://")); err != nil {
		return "", fmt.Errorf("rename_symbol: %w", err)
	}

	ctx, cancel := withLSPDeadline(ctx, t.timeout)
	defer cancel()

	we, err := t.client.Rename(ctx, protocol.RenameParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: a.URI},
		Position:     protocol.Position{Line: a.Line, Character: a.Character},
		NewName:      a.NewName,
	})
	if err != nil {
		return "", positionErr("rename_symbol", err)
	}
	if we == nil || (len(we.Changes) == 0 && len(we.DocumentChanges) == 0) {
		return "No changes — rename returned an empty edit set (symbol may not be renameable here).", nil
	}

	totalEdits := 0
	files := []string{}
	for uri, edits := range we.Changes {
		if err := t.guard.check(strings.TrimPrefix(uri, "file://")); err != nil {
			return "", fmt.Errorf("rename_symbol: %w", err)
		}
		totalEdits += len(edits)
		files = append(files, strings.TrimPrefix(uri, "file://"))
	}
	for _, dce := range we.DocumentChanges {
		if err := t.guard.check(strings.TrimPrefix(dce.TextDocument.URI, "file://")); err != nil {
			return "", fmt.Errorf("rename_symbol: %w", err)
		}
		totalEdits += len(dce.Edits)
		files = append(files, strings.TrimPrefix(dce.TextDocument.URI, "file://"))
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
