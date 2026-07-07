package tools

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/paths"
)

// applyWorkspaceEdit applies a LSP WorkspaceEdit to disk. Handles both the
// `changes` (map[uri][]TextEdit) and `documentChanges` (TextDocumentEdit[])
// forms. Returns the list of files modified.
//
// All target files are locked in stable order, read, and validated in memory
// before any bytes are written. If a write fails after earlier files were
// updated, those files are restored to their pre-edit content before returning.
// That keeps semantic renames all-or-nothing at the filesystem level instead of
// leaving a partially-applied WorkspaceEdit behind.
//
// Edits within each file are applied in reverse-order so earlier edits do not
// shift the positions of later ones. Each file write is atomic (tmp + rename).
//
// Note on character offsets: LSP positions are UTF-16 code units per the spec.
// We treat them as UTF-8 byte offsets, which is correct for ASCII source and
// off-by-some for files containing wide characters in code positions. Most
// refactoring happens on ASCII identifiers, so this is acceptable for now.
func applyWorkspaceEdit(we *protocol.WorkspaceEdit) ([]string, error) {
	modified, _, err := applyWorkspaceEditDetailed(we)
	return modified, err
}

func applyWorkspaceEditDetailed(we *protocol.WorkspaceEdit) ([]string, []workspaceEditPlan, error) {
	if we == nil {
		return nil, nil, nil
	}

	editsByURI := workspaceEditGroups(we)
	pathsByURI := make(map[string]string, len(editsByURI))
	var targetPaths []string
	for uri := range editsByURI {
		path := paths.URIToPath(uri)
		pathsByURI[uri] = path
		targetPaths = append(targetPaths, path)
	}
	sort.Strings(targetPaths)

	unlocks := lockPaths(targetPaths)
	defer unlockAll(unlocks)

	plans := make([]workspaceEditPlan, 0, len(editsByURI))
	for uri, edits := range editsByURI {
		path := pathsByURI[uri]
		before, after, mode, err := prepareTextEditsLocked(path, edits)
		if err != nil {
			return nil, nil, fmt.Errorf("applying edits to %s: %w", path, err)
		}
		plans = append(plans, workspaceEditPlan{
			path:   path,
			before: before,
			after:  after,
			mode:   mode,
		})
	}
	sort.Slice(plans, func(i, j int) bool { return plans[i].path < plans[j].path })

	var modified []string
	for _, p := range plans {
		if _, err := safeWrite(p.path, p.after, p.mode); err != nil {
			if rbErr := rollbackWorkspaceEdit(plans, modified); rbErr != nil {
				return modified, plans, fmt.Errorf("writing %s: %w; rollback failed: %v", p.path, err, rbErr)
			}
			return modified, plans, fmt.Errorf("writing %s: %w", p.path, err)
		}
		modified = append(modified, p.path)
	}
	return modified, plans, nil
}

type workspaceEditPlan struct {
	path   string
	before []byte
	after  []byte
	mode   os.FileMode
}

func workspaceEditGroups(we *protocol.WorkspaceEdit) map[string][]protocol.TextEdit {
	editsByURI := make(map[string][]protocol.TextEdit)
	for uri, edits := range we.Changes {
		editsByURI[uri] = append(editsByURI[uri], edits...)
	}
	for _, dce := range we.DocumentChanges {
		editsByURI[dce.TextDocument.URI] = append(editsByURI[dce.TextDocument.URI], dce.Edits...)
	}
	return editsByURI
}

func lockPaths(paths []string) []func() {
	if len(paths) == 0 {
		return nil
	}
	uniq := paths[:0]
	var last string
	for i, p := range paths {
		if i == 0 || p != last {
			uniq = append(uniq, p)
			last = p
		}
	}
	unlocks := make([]func(), 0, len(uniq))
	for _, p := range uniq {
		unlocks = append(unlocks, lockPath(p))
	}
	return unlocks
}

func unlockAll(unlocks []func()) {
	for i := len(unlocks) - 1; i >= 0; i-- {
		unlocks[i]()
	}
}

func rollbackWorkspaceEdit(plans []workspaceEditPlan, modified []string) error {
	byPath := make(map[string]workspaceEditPlan, len(plans))
	for _, p := range plans {
		byPath[p.path] = p
	}
	var errs []string
	for i := len(modified) - 1; i >= 0; i-- {
		p := byPath[modified[i]]
		if _, err := safeWrite(p.path, p.before, p.mode); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", p.path, err))
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// applyTextEditsToFile applies a list of TextEdits to a single file atomically,
// under the per-path write lock every other write tool holds. Without it, a
// concurrent edit_file / symbol-edit / rename on the same file (the daemon
// dispatches tool calls concurrently across connections) could read the same
// pre-edit content and lost-update each other. It writes through safeWrite,
// which stages a UNIQUELY-named temp file and renames it into place — never a
// fixed "<path>.tmp" that two concurrent writers would collide on. The lock is
// taken and released per file, so applyWorkspaceEdit's multi-file rename never
// holds two path locks at once (no lock-ordering deadlock).
func applyTextEditsToFile(path string, edits []protocol.TextEdit) error {
	unlock := lockPath(path)
	defer unlock()

	_, out, mode, err := prepareTextEditsLocked(path, edits)
	if err != nil {
		return err
	}
	if _, err := safeWrite(path, out, mode); err != nil {
		return err
	}
	return nil
}

func prepareTextEditsLocked(path string, edits []protocol.TextEdit) (before, after []byte, mode os.FileMode, err error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, 0, err
	}
	mode = info.Mode().Perm()
	if mode == 0 {
		mode = 0o644
	}
	before, err = os.ReadFile(path)
	if err != nil {
		return nil, nil, 0, err
	}
	after, err = applyTextEdits(before, edits)
	if err != nil {
		return nil, nil, 0, err
	}
	return before, after, mode, nil
}

// applyTextEdits applies edits to data and returns the resulting content. Pure;
// performs no I/O. Edits are applied start-position descending so an earlier
// edit never shifts the offsets of a later one. The input slice is sorted in
// place (callers pass a freshly-built slice, as the file-writing path does).
func applyTextEdits(data []byte, edits []protocol.TextEdit) ([]byte, error) {
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
			return nil, fmt.Errorf("edit start position out of range: line %d char %d", e.Range.Start.Line, e.Range.Start.Character)
		}
		endOff, ok := endOffsetForPosition(data, e.Range.End)
		if !ok {
			return nil, fmt.Errorf("edit end position out of range: line %d char %d", e.Range.End.Line, e.Range.End.Character)
		}
		if startOff > endOff {
			return nil, fmt.Errorf("edit start after end")
		}
		buf := make([]byte, 0, startOff+len(e.NewText)+(len(data)-endOff))
		buf = append(buf, data[:startOff]...)
		buf = append(buf, []byte(e.NewText)...)
		buf = append(buf, data[endOff:]...)
		data = buf
	}
	return data, nil
}

// maxEndOvershootLines bounds how far past the last line of a file an edit's
// END position may point before we treat the range as stale rather than clamp
// it. An LSP symbol-range end legitimately addresses one line past the last
// line (line == lineCount, character 0), or one character past a file with no
// trailing newline — a small, expected overshoot that means "to end of file".
// A larger overshoot means the range was computed against an older, longer
// version of the file (RC2 staleness); clamping there would silently swallow
// live content, so we refuse and let the caller surface the error / re-resolve.
// Two lines is deliberately tight — enough to absorb the legitimate off-by-one,
// nowhere near enough to eat a function body.
const maxEndOvershootLines = 2

// endOffsetForPosition resolves an edit's END position to a byte offset,
// clamping a small overshoot past the end of the file to len(data). It exists
// so a fresh symbol-range end that points one past true EOF applies cleanly
// instead of detonating the whole edit; a wild overshoot (a stale range) still
// returns false. START positions keep the stricter offsetForPosition — a start
// past EOF is always an error.
func endOffsetForPosition(data []byte, pos protocol.Position) (int, bool) {
	if off, ok := offsetForPosition(data, pos); ok {
		return off, true
	}
	var lineCount uint32
	for _, b := range data {
		if b == '\n' {
			lineCount++
		}
	}
	// Only clamp a position at or past the final line; an intra-line overrun on
	// an earlier line is a broken range, not an end-of-file end, so it must fail.
	if pos.Line < lineCount || pos.Line-lineCount > maxEndOvershootLines {
		return 0, false
	}
	return len(data), true
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
// matching symbol, or nil if not found. Each segment matches via
// symbolNameMatches, so a plain "show" addresses a member a server reports
// with its signature ("show()", sourcekit-lsp) — keeping the semantic-edit
// tools' by-name addressing in step with the read/query tools.
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
		if symbolNameMatches(syms[i].Name, parts[0]) {
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
