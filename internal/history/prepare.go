package history

import (
	"bytes"
	"path/filepath"

	"github.com/plumbkit/plumb/internal/paths"
	"github.com/plumbkit/plumb/internal/textdiff"
)

// Policy is the per-call slice of [history] config Prepare applies.
type Policy struct {
	SensitiveGlobs  []string
	MaxContentBytes int64
}

// binarySniffBytes matches internal/tools/walk.go (the ripgrep/git heuristic).
const binarySniffBytes = 8000

// Prepare classifies it inline, on the caller's goroutine, so withheld content
// never enters the queue (spec §5.2 steps 1–2): directories and unchanged
// renames need no diff; sensitive paths keep counts and shas only; oversized
// sides are stripped. Everything else is left for the writer.
func Prepare(it Item, p Policy) Item {
	switch {
	case it.Kind == KindDir:
		it.Content = ContentNone
	case it.Op == OpRename && bytes.Equal(it.Before.SHA, it.After.SHA):
		it.Content = ContentNone
	case isSensitive(p.SensitiveGlobs, it):
		if carried(it.Before) && carried(it.After) {
			it.Added, it.Removed = textdiff.Counts(textdiff.ComputeExact(string(it.Before.Content), string(it.After.Content)))
		}
		it.Content = ContentSensitive
	case tooBig(it.Before, limit(p)) || tooBig(it.After, limit(p)):
		it.Content = ContentTooLarge
	default:
		return it
	}
	it.Before.Content, it.After.Content = nil, nil
	return it
}

func limit(p Policy) int64 {
	if p.MaxContentBytes <= 0 || p.MaxContentBytes > HardMaxContentBytes {
		return HardMaxContentBytes
	}
	return p.MaxContentBytes
}

func carried(s Side) bool                { return !s.Exists || s.Content != nil }
func tooBig(s Side, maxBytes int64) bool { return s.Exists && (s.Content == nil || s.Size > maxBytes) }

// isSensitive reports whether the change touches a sensitive file under ANY
// spelling the row could end up filed under or the content could come from:
//
//   - the path as the tool resolved it AND as the writer will store it
//     (paths.Canonical follows symlinks, so a write through `notes.txt -> .env`
//     is filed under .env and must be classified as .env);
//   - the copy/rename SOURCE (From): a copy of .env carries .env's content to
//     a destination whose own name matches nothing;
//   - against the root as given and as canonicalised, so a workspace-relative
//     glob still matches when the root and the path disagree on an alias
//     (/var vs /private/var, a symlinked checkout).
//
// Classification runs before the queue, so a miss here stores plaintext.
func isSensitive(globs []string, it Item) bool {
	if len(globs) == 0 {
		return false
	}
	roots := []string{it.Workspace, paths.Canonical(it.Workspace)}
	cands := []string{it.Path, paths.Canonical(it.Path)}
	if it.From != "" {
		cands = append(cands, it.From, paths.Canonical(it.From))
	}
	for _, c := range cands {
		for _, r := range roots {
			if MatchSensitive(globs, r, c) {
				return true
			}
		}
	}
	return false
}

// MatchSensitive reports whether path matches any glob by base name or by its
// path relative to root ("/"-separated). It matches the one spelling it is
// given; isSensitive is what tries every spelling a change can carry.
func MatchSensitive(globs []string, root, path string) bool {
	base := filepath.Base(path)
	rel, err := filepath.Rel(root, path)
	if err != nil {
		rel = ""
	}
	rel = filepath.ToSlash(rel)
	for _, g := range globs {
		if ok, _ := filepath.Match(g, base); ok {
			return true
		}
		if ok, _ := filepath.Match(g, rel); ok && rel != "" {
			return true
		}
	}
	return false
}

func isBinary(b []byte) bool {
	return bytes.IndexByte(b[:min(len(b), binarySniffBytes)], 0) >= 0
}
