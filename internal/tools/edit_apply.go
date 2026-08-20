package tools

import (
	"errors"
	"fmt"
	"os"
	"slices"
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
	modified, _, err := applyWorkspaceEditDetailed(we, nil)
	return modified, err
}

// applyWorkspaceEditDetailed applies every edit in we, holding all target path
// locks (acquired in canonical sorted order) across prepare and write. Every
// file is prepared in memory before any byte lands; a mid-sequence write
// failure rolls already-written files back to their pre-edit bytes. onApplied,
// when non-nil, runs after all writes succeed but before the locks release —
// the place for bookkeeping (write tracker, undo snapshots) whose contract
// requires the per-path lock to still be held.
//
// Targets are prepared, and then written, in sorted path order rather than in
// the WorkspaceEdit's map order. Nothing about correctness depends on which file
// is prepared first — no byte is written until all of them have been — but a
// deterministic order means a broken "validate as you write" refactor fails the
// same way on every run instead of one time in N, and a failure names the same
// file every time.
func applyWorkspaceEditDetailed(we *protocol.WorkspaceEdit, onApplied func([]workspaceEditPlan)) ([]string, []workspaceEditPlan, error) {
	if we == nil {
		return nil, nil, nil
	}

	targets, err := workspaceEditTargets(we)
	if err != nil {
		return nil, nil, err
	}
	targetPaths := make([]string, 0, len(targets))
	for _, tgt := range targets {
		targetPaths = append(targetPaths, tgt.path)
	}

	unlocks := lockPaths(targetPaths)
	defer unlockAll(unlocks)

	plans := make([]workspaceEditPlan, 0, len(targets))
	for _, tgt := range targets {
		before, after, mode, err := prepareTextEditsLocked(tgt.path, tgt.edits)
		if err != nil {
			return nil, nil, fmt.Errorf("applying edits to %s: %w", tgt.path, err)
		}
		plans = append(plans, workspaceEditPlan{
			path:   tgt.path,
			before: before,
			after:  after,
			mode:   mode,
		})
	}

	var modified []string
	for _, p := range plans {
		if _, err := safeWrite(p.path, p.after, p.mode); err != nil {
			if rbErr := rollbackWorkspaceEdit(plans, modified); rbErr != nil {
				return modified, plans, fmt.Errorf("writing %s: %w; rollback failed: %w", p.path, err, rbErr)
			}
			return modified, plans, fmt.Errorf("writing %s: %w", p.path, err)
		}
		modified = append(modified, p.path)
	}
	if onApplied != nil {
		onApplied(plans)
	}
	return modified, plans, nil
}

type workspaceEditPlan struct {
	path   string
	before []byte
	after  []byte
	mode   os.FileMode
}

// workspaceEditTarget is one file's share of a WorkspaceEdit, resolved to a
// filesystem path.
type workspaceEditTarget struct {
	path  string
	edits []protocol.TextEdit
}

// workspaceEditTargets resolves we into one target per file, in sorted path
// order, so preparation, writing, and any resulting error are deterministic
// regardless of Go's map iteration.
//
// A file named by TWO entries that both carry edits is refused (issue #314).
// An "entry" is one key of Changes or one element of DocumentChanges, and the
// two dangerous shapes are the same defect wearing different clothes:
//
//   - Two SPELLINGS of one file (through a symlinked parent, say) used to
//     become two targets. Both were prepared from the same pre-edit bytes and
//     both were written in turn, so the second write silently discarded the
//     first — a lost update inside an apply whose contract is that it lands
//     atomically. lockPaths already collapses the pair to one mutex (issue
//     #290), so nothing deadlocked and nothing failed; that is exactly what
//     made it silent.
//   - The SAME spelling in both Changes and DocumentChanges used to be merged
//     into one edit list. A server that emits its edits under both forms for
//     capability compatibility would then have each edit applied TWICE.
//     applyTextEdits threads the buffer through its loop (each edit sees the
//     previous edit's output, not the pre-edit bytes), so a repeated
//     length-changing edit does not land idempotently: it corrupts the file.
//
// Refusing both mirrors the decision transaction_apply made for the same shape
// (txCanonicalPaths). It is the safe direction: a refusal cannot corrupt a
// file, and merging — the alternative #314 offered — is precisely what produces
// the second case above.
//
// An entry carrying NO edits is exempt, because it cannot lose or duplicate
// anything: servers emit a bare documentChanges entry alongside changes, and
// TestRenameSymbol_DeduplicatesTargetsAcrossEditForms pins that such a file is
// still counted once rather than refused.
//
// "Two spellings" includes two that differ only in CASE on a filesystem that
// folds case: lockPathKey routes through paths.CanonicalKey, which probes the
// volume and lowercases the key where it folds (issue #346), so such a pair
// arrives here as one key and is refused like any other. It used to arrive as
// two, which made this guard miss precisely the lost update it exists to stop.
func workspaceEditTargets(we *protocol.WorkspaceEdit) ([]workspaceEditTarget, error) {
	targets := make([]workspaceEditTarget, 0, len(we.Changes)+len(we.DocumentChanges))
	byKey := make(map[string]int, cap(targets))
	for _, e := range workspaceEditEntries(we) {
		p := paths.URIToPath(e.uri)
		key := lockPathKey(p)
		idx, dup := byKey[key]
		if !dup {
			byKey[key] = len(targets)
			// Clone: applyTextEdits sorts its argument IN PLACE, and these slices are
			// the caller's WorkspaceEdit. The merge this replaced always allocated,
			// so aliasing here would quietly break applyTextEdits' documented
			// "callers pass a freshly-built slice" contract.
			targets = append(targets, workspaceEditTarget{path: p, edits: slices.Clone(e.edits)})
			continue
		}
		// The bare-mention carve-out is deliberately narrow: it applies only when
		// both mentions use the SAME spelling. Two SPELLINGS of one file are the
		// defect this guard exists for, and a bare second mention does not make the
		// pair benign — it just makes it harmless today, while leaving the target
		// carrying one spelling and rename_symbol's file list counting both, which
		// breaks the invariant collectRenameTargets documents.
		if targets[idx].path == p {
			if len(e.edits) == 0 {
				continue // a bare mention changes nothing
			}
			if len(targets[idx].edits) == 0 {
				targets[idx].edits = slices.Clone(e.edits) // the earlier mention was bare
				continue
			}
		}
		named := fmt.Sprintf("under two paths (%q and %q)", targets[idx].path, p)
		if targets[idx].path == p {
			named = fmt.Sprintf("twice (%q)", p)
		}
		return nil, &editLogicErr{fmt.Errorf(
			"workspace edit names the same file %s, so one set of edits would silently overwrite the other or the same edit would be applied twice — plumb cannot apply that atomically. This is a property of what the language server sent, so retrying will not change it: apply the edits to that file with edit_file instead",
			named,
		)}
	}
	// NOT redundant with the sorted URIs in workspaceEditEntries: that sorts the
	// Changes map only, and DocumentChanges entries follow it in the order the
	// server sent them. An edit set using BOTH forms therefore arrives unsorted on
	// every platform, not just where URIToPath rewrites separators (Windows).
	sort.Slice(targets, func(i, j int) bool { return targets[i].path < targets[j].path })
	return targets, nil
}

// workspaceEditEntry is one (URI, edits) pair exactly as the server sent it:
// one key of Changes, or one element of DocumentChanges. Entries are kept
// distinct rather than merged by URI so that a file named by two of them can be
// recognised as such instead of being silently concatenated.
type workspaceEditEntry struct {
	uri   string
	edits []protocol.TextEdit
}

// workspaceEditEntries flattens we in a deterministic order — Changes sorted by
// URI, then DocumentChanges as the server ordered them. Determinism matters
// because a duplicate must be reported against the same pair of entries on
// every run; iterating the Changes map directly would pick a different "first"
// mention each time and make the refusal look unreproducible.
func workspaceEditEntries(we *protocol.WorkspaceEdit) []workspaceEditEntry {
	uris := make([]string, 0, len(we.Changes))
	for uri := range we.Changes {
		uris = append(uris, uri)
	}
	sort.Strings(uris)

	entries := make([]workspaceEditEntry, 0, len(uris)+len(we.DocumentChanges))
	for _, uri := range uris {
		entries = append(entries, workspaceEditEntry{uri: uri, edits: we.Changes[uri]})
	}
	for _, dce := range we.DocumentChanges {
		entries = append(entries, workspaceEditEntry{uri: dce.TextDocument.URI, edits: dce.Edits})
	}
	return entries
}

// lockPaths locks every distinct path and returns the unlock funcs. Paths are
// deduplicated and ordered by their canonical lock key (lockPathKey —
// paths.CanonicalKey), not their raw spelling: two spellings of the same file
// (/tmp/x vs /private/tmp/x on macOS, or x.txt vs X.txt wherever the volume
// folds case) map to one non-reentrant mutex, so a raw-string dedup would
// self-deadlock on the second acquisition, and canonical ordering keeps the
// acquisition order consistent across callers regardless of spelling.
func lockPaths(paths []string) []func() {
	if len(paths) == 0 {
		return nil
	}
	spelling := make(map[string]string, len(paths))
	keys := make([]string, 0, len(paths))
	for _, p := range paths {
		k := lockPathKey(p)
		if _, dup := spelling[k]; dup {
			continue
		}
		spelling[k] = p
		keys = append(keys, k)
	}
	sort.Strings(keys)
	unlocks := make([]func(), 0, len(keys))
	for _, k := range keys {
		unlocks = append(unlocks, lockPath(spelling[k]))
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
// fixed "<path>.tmp" that two concurrent writers would collide on. The
// production multi-file path is applyWorkspaceEditDetailed, which holds ALL
// target locks (canonically sorted) across prepare and write; this single-file
// helper remains for tests and takes one lock only.
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
			return nil, errors.New("edit start after end")
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
