package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/golimpio/plumb/internal/lsp"
	"github.com/golimpio/plumb/internal/lsp/protocol"
)

// RenameSymbol performs a workspace-wide rename via LSP. The language server
// computes all call-site updates safely (across files, respecting scope and
// types). Plumb applies the resulting WorkspaceEdit atomically to disk.
type RenameSymbol struct {
	client lsp.Client
}

func NewRenameSymbol(client lsp.Client) *RenameSymbol { return &RenameSymbol{client: client} }

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
			"uri":{"type":"string","description":"Document URI (file://...)."},
			"line":{"type":"integer","minimum":0,"description":"Zero-based line of the identifier."},
			"character":{"type":"integer","minimum":0,"description":"Zero-based character offset within the line."},
			"new_name":{"type":"string","description":"Replacement identifier name."},
			"dry_run":{"type":"boolean","default":true,"description":"If true (default), preview changes only."}
		},
		"required":["uri","line","character","new_name"]
	}`)
}

func (t *RenameSymbol) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		URI       string `json:"uri"`
		Line      uint32 `json:"line"`
		Character uint32 `json:"character"`
		NewName   string `json:"new_name"`
		DryRun    *bool  `json:"dry_run,omitempty"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	if a.URI == "" || a.NewName == "" {
		return "", fmt.Errorf("`uri` and `new_name` are required")
	}
	dryRun := true
	if a.DryRun != nil {
		dryRun = *a.DryRun
	}

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
		totalEdits += len(edits)
		files = append(files, strings.TrimPrefix(uri, "file://"))
	}
	for _, dce := range we.DocumentChanges {
		totalEdits += len(dce.Edits)
		files = append(files, strings.TrimPrefix(dce.TextDocument.URI, "file://"))
	}
	sort.Strings(files)

	var sb strings.Builder
	verb := "would change"
	if !dryRun {
		modified, applyErr := applyWorkspaceEdit(we)
		if applyErr != nil {
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
	if dryRun {
		sb.WriteString("\nTo apply, re-run with dry_run=false.")
	}
	return sb.String(), nil
}
