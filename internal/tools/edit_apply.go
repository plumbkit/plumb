package tools

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/golimpio/plumb/internal/lsp/protocol"
)

// applyWorkspaceEdit applies a LSP WorkspaceEdit to disk. Handles both the
// `changes` (map[uri][]TextEdit) and `documentChanges` (TextDocumentEdit[])
// forms. Returns the list of files modified.
//
// Edits within each file are applied in reverse-order so earlier edits do not
// shift the positions of later ones. Each file write is atomic (tmp + rename).
//
// Note on character offsets: LSP positions are UTF-16 code units per the spec.
// We treat them as UTF-8 byte offsets, which is correct for ASCII source and
// off-by-some for files containing wide characters in code positions. Most
// refactoring happens on ASCII identifiers, so this is acceptable for now.
func applyWorkspaceEdit(we *protocol.WorkspaceEdit) ([]string, error) {
	if we == nil {
		return nil, nil
	}

	editsByURI := make(map[string][]protocol.TextEdit)
	for uri, edits := range we.Changes {
		editsByURI[uri] = append(editsByURI[uri], edits...)
	}
	for _, dce := range we.DocumentChanges {
		editsByURI[dce.TextDocument.URI] = append(editsByURI[dce.TextDocument.URI], dce.Edits...)
	}

	var modified []string
	for uri, edits := range editsByURI {
		path := strings.TrimPrefix(uri, "file://")
		if err := applyTextEditsToFile(path, edits); err != nil {
			return modified, fmt.Errorf("applying edits to %s: %w", path, err)
		}
		modified = append(modified, path)
	}
	sort.Strings(modified)
	return modified, nil
}

// applyTextEditsToFile applies a list of TextEdits to a single file atomically.
func applyTextEditsToFile(path string, edits []protocol.TextEdit) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	// Sort edits by start position descending so we can apply in order without
	// later edits invalidating earlier offsets.
	sort.Slice(edits, func(i, j int) bool {
		a, b := edits[i].Range.Start, edits[j].Range.Start
		if a.Line != b.Line {
			return a.Line > b.Line
		}
		return a.Character > b.Character
	})

	for _, e := range edits {
		startOff, ok := offsetForPosition(data, e.Range.Start)
		if !ok {
			return fmt.Errorf("edit start position out of range: line %d char %d", e.Range.Start.Line, e.Range.Start.Character)
		}
		endOff, ok := offsetForPosition(data, e.Range.End)
		if !ok {
			return fmt.Errorf("edit end position out of range: line %d char %d", e.Range.End.Line, e.Range.End.Character)
		}
		if startOff > endOff {
			return fmt.Errorf("edit start after end")
		}
		buf := make([]byte, 0, startOff+len(e.NewText)+(len(data)-endOff))
		buf = append(buf, data[:startOff]...)
		buf = append(buf, []byte(e.NewText)...)
		buf = append(buf, data[endOff:]...)
		data = buf
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// offsetForPosition returns the byte offset of pos in data, or false if pos is
// past the end of the file. Treats LSP UTF-16 code-unit characters as UTF-8
// bytes; correct for ASCII.
func offsetForPosition(data []byte, pos protocol.Position) (int, bool) {
	if pos.Line == 0 && pos.Character == 0 {
		return 0, true
	}
	line, col := uint32(0), uint32(0)
	for i, b := range data {
		if line == pos.Line && col == pos.Character {
			return i, true
		}
		if b == '\n' {
			line++
			col = 0
			continue
		}
		col++
	}
	if line == pos.Line && col == pos.Character {
		return len(data), true
	}
	return 0, false
}

// findSymbolByPath walks a hierarchical DocumentSymbol tree following a
// slash-separated name path (e.g. "ClassName/methodName"). Returns the
// matching symbol, or nil if not found.
func findSymbolByPath(syms []protocol.DocumentSymbol, namePath string) *protocol.DocumentSymbol {
	parts := strings.Split(namePath, "/")
	if len(parts) == 0 || parts[0] == "" {
		return nil
	}
	return findSymbolRecursive(syms, parts)
}

func findSymbolRecursive(syms []protocol.DocumentSymbol, parts []string) *protocol.DocumentSymbol {
	if len(parts) == 0 {
		return nil
	}
	for i := range syms {
		if syms[i].Name == parts[0] {
			if len(parts) == 1 {
				return &syms[i]
			}
			if found := findSymbolRecursive(syms[i].Children, parts[1:]); found != nil {
				return found
			}
		}
	}
	return nil
}
