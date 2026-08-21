// Package setup implements the mechanics of plumb's managed instruction
// block: a small, versioned, idempotent span that plumb owns inside a
// client's instruction file (AGENTS.md, CLAUDE.md, GEMINI.md, ...), bounded
// by a pair of HTML-comment markers. Everything outside the markers belongs
// to the user — plumb never reads it, never reasons about it, and never
// rewrites it.
//
// The mechanism is client-agnostic by design: WHICH file a client reads and
// WHAT the block says are internal/cli's questions (setup wiring, per-client
// templates), not this package's. This package only knows how to find,
// render, compare, and write one block inside an arbitrary text file.
package setup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/plumbkit/plumb/internal/fsync"
	"github.com/plumbkit/plumb/internal/paths"
)

const (
	startMarkerPrefix = "<!-- plumb:managed:start "
	startMarkerSuffix = " -->"
	// EndMarker closes a managed block. It carries no version because only
	// one block is ever open at a time — the start marker is the single
	// source of truth for which template version produced it.
	EndMarker = "<!-- plumb:managed:end -->"
)

// startMarker renders the opening marker line for version.
func startMarker(version string) string {
	return startMarkerPrefix + version + startMarkerSuffix
}

// RenderBlock wraps body between the versioned start marker and EndMarker.
// body's trailing newlines are trimmed first so the rendered block always
// ends the same way regardless of whether the caller's template has one —
// which is what makes Apply's idempotency check a plain string comparison.
func RenderBlock(body, version string) string {
	return startMarker(version) + "\n" + strings.TrimRight(body, "\n") + "\n" + EndMarker
}

// blockSpan describes one well-formed managed block found by scanBlocks: byte
// offsets [start, end) covering its start-marker line through its end-marker
// line (both inclusive of their own text, exclusive of a following newline),
// and the version recorded on its start marker.
type blockSpan struct {
	start, end int
	version    string
}

// scanBlocks walks content line by line and reports every well-formed managed
// block, plus whether the markers in content are malformed in any way: an
// orphan start (no matching end before EOF or before another start), an end
// with no preceding start, or more than one well-formed block.
//
// Malformed content is reported rather than guessed at, on purpose. An
// earlier version of this scanner located just the FIRST textual occurrence
// of the start-marker prefix and searched forward for the next end marker,
// which fails open in two dangerous ways: (1) if the user deletes just the
// end marker, the orphan start survives and the NEXT Apply pairs it with a
// different block's end marker, silently deleting every byte between —
// including user prose that was never inside any block; (2) a file that
// merely quotes the marker text inline (documenting the mechanism, say) grows
// a fresh block on every Apply, since the malformed candidate is invisible to
// the scanner and "no block found" means append. A malformed or duplicated
// block must stop Apply from writing at all, not make its best guess.
//
// A line only counts as a marker if it is EXACTLY the marker text with
// nothing else on the line — this is what keeps a marker quoted mid-sentence
// (“ `<!-- plumb:managed:start v1 -->` “ in a doc, say) from being mistaken
// for a real one: such a line never starts at column zero with the marker
// prefix.
func scanBlocks(content string) (blocks []blockSpan, malformed bool) {
	pos := 0
	pendingStart := -1
	pendingVersion := ""
	for pos <= len(content) {
		lineEnd := strings.IndexByte(content[pos:], '\n')
		var line string
		var nextPos int
		last := lineEnd == -1
		if last {
			line = content[pos:]
		} else {
			line = content[pos : pos+lineEnd]
			nextPos = pos + lineEnd + 1
		}
		trimmed := strings.TrimSuffix(line, "\r") // tolerate CRLF without treating it as content

		switch trimmed {
		case EndMarker:
			if pendingStart == -1 {
				malformed = true // end marker with no matching start
			} else {
				blocks = append(blocks, blockSpan{start: pendingStart, end: pos + len(trimmed), version: pendingVersion})
				pendingStart = -1
				pendingVersion = ""
			}
		default:
			if version, ok := parseStartMarkerLine(trimmed); ok {
				if pendingStart != -1 {
					malformed = true // a second start before the first was closed
				}
				pendingStart = pos
				pendingVersion = version
			}
		}

		if last {
			break
		}
		pos = nextPos
	}
	if pendingStart != -1 {
		malformed = true // unterminated start at EOF
	}
	if len(blocks) > 1 {
		malformed = true // more than one managed block — flagged rather than guessed at
	}
	return blocks, malformed
}

// parseStartMarkerLine reports whether line is EXACTLY a start marker line
// — startMarkerPrefix, a version token, then startMarkerSuffix, and nothing
// else — and returns the version. A version containing anything other than
// letters, digits, '.', '_', or '-' fails the parse rather than being
// accepted: it is either not a marker at all, or a hand-mangled one, and
// either way scanBlocks must not treat it as a well-formed boundary.
func parseStartMarkerLine(line string) (version string, ok bool) {
	if !strings.HasPrefix(line, startMarkerPrefix) || !strings.HasSuffix(line, startMarkerSuffix) {
		return "", false
	}
	version = line[len(startMarkerPrefix) : len(line)-len(startMarkerSuffix)]
	if !isVersionToken(version) {
		return "", false
	}
	return version, true
}

// isVersionToken reports whether v is a plausible version string: non-empty,
// and built only from letters, digits, '.', '_', and '-'. It exists so a
// version field can never smuggle marker-like syntax (a stray "-->", a space,
// a newline) into what scanBlocks treats as a clean line match.
func isVersionToken(v string) bool {
	if v == "" {
		return false
	}
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// resolveTarget follows path if it is a symlink (or a chain of them) and
// returns the real file it names, via paths.Canonical — the tree's one
// "same place?" answer. A path that does not exist yet, or that is not a
// symlink, resolves to itself unchanged — the caller creates it directly.
// This is what keeps Apply from ever replacing a symlink (e.g. this repo's
// CLAUDE.md -> AGENTS.md) with a plain file: every write below targets the
// resolved path, never the link.
func resolveTarget(path string) string {
	return paths.Canonical(path)
}

// Status reports how a file's on-disk managed block compares to the current
// template.
type Status int

const (
	// StatusMissing means the file doesn't exist, or exists without a
	// well-formed managed block.
	StatusMissing Status = iota
	// StatusStale means a block is present whose recorded version differs
	// from the version Check was asked about.
	StatusStale
	// StatusModified means the block's version matches, but its content was
	// hand-edited since plumb last wrote it.
	StatusModified
	// StatusCurrent means the block matches the current template exactly.
	StatusCurrent
	// StatusMalformed means the file's markers cannot be trusted — an orphan
	// start or end marker, or more than one well-formed block. Check reports
	// it rather than guessing at Missing/Stale/Modified; Apply refuses to
	// write at all (see scanBlocks).
	StatusMalformed
)

// String names the status for CLI/log output.
func (s Status) String() string {
	switch s {
	case StatusMissing:
		return "missing"
	case StatusStale:
		return "stale"
	case StatusModified:
		return "modified"
	case StatusCurrent:
		return "current"
	case StatusMalformed:
		return "malformed"
	default:
		return "unknown"
	}
}

// Check inspects path — following a symlink to its real target first — and
// reports how its managed block compares to RenderBlock(body, version). It
// never writes.
func Check(path, body, version string) (Status, error) {
	target := resolveTarget(path)
	data, err := os.ReadFile(target) //nolint:gosec // G304: target is caller-supplied by design (a client's own instruction file), same trust boundary as every other setup writer in this codebase
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return StatusMissing, nil
		}
		return StatusMissing, fmt.Errorf("reading %s: %w", target, err)
	}
	content := string(data)
	blocks, malformed := scanBlocks(content)
	if malformed {
		return StatusMalformed, nil
	}
	if len(blocks) == 0 {
		return StatusMissing, nil
	}
	b := blocks[0]
	if b.version != version {
		return StatusStale, nil
	}
	if content[b.start:b.end] != RenderBlock(body, version) {
		return StatusModified, nil
	}
	return StatusCurrent, nil
}

// Apply writes the managed block into the file at path, following a symlink
// to its real target first (the target is rewritten in place; the symlink
// itself is never replaced with a regular file). Behaviour:
//
//   - File absent: created containing just the rendered block.
//   - File present with a managed block (any version): that span is replaced
//     with the current one; content outside the markers is preserved
//     byte-for-byte, whatever it is.
//   - File present without a managed block: the block is appended, separated
//     from existing content by one blank line.
//
// Apply is how `--sync` and a bare `plumb setup <client>` are the same
// operation: both call Apply with the current template, so a stale or
// hand-edited block is unconditionally restored to it. Returns changed=false
// when the file already matches (a plain no-op, not a rewrite) — verified by
// TestManagedBlock_Idempotent to be byte-identical across repeated calls.
func Apply(path, body, version string) (changed bool, err error) {
	target := resolveTarget(path)
	block := RenderBlock(body, version)

	data, readErr := os.ReadFile(target) //nolint:gosec // G304: target is caller-supplied by design, same trust boundary as every other setup writer in this codebase
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return false, fmt.Errorf("reading %s: %w", target, readErr)
	}
	if errors.Is(readErr, os.ErrNotExist) {
		// A GLOBAL instruction file's parent directory (~/.codex, ~/.claude,
		// ~/.gemini) may not exist yet on a machine that has never run the
		// client — fsync.AtomicWrite deliberately never creates directories,
		// so a fresh managed block does it here.
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return false, fmt.Errorf("creating directory for %s: %w", target, err)
		}
		if err := writeBlock(target, block+"\n"); err != nil {
			return false, err
		}
		return true, nil
	}

	content := string(data)
	blocks, malformed := scanBlocks(content)
	if malformed {
		return false, fmt.Errorf("%s: managed block markers are malformed or duplicated (an orphan start/end marker, or more than one block) — refusing to write; repair the file by hand and re-run", target)
	}

	next := mergeBlock(content, blocks, block)
	if next == content {
		return false, nil
	}
	if err := writeBlock(target, next); err != nil {
		return false, err
	}
	return true, nil
}

// mergeBlock returns content with its single well-formed managed block (if
// any — blocks has at most one entry whenever scanBlocks reports
// malformed=false) replaced by block, or block appended — separated from any
// existing content by one blank line — when content has none.
func mergeBlock(content string, blocks []blockSpan, block string) string {
	if len(blocks) == 1 {
		b := blocks[0]
		return content[:b.start] + block + content[b.end:]
	}
	trimmed := strings.TrimRight(content, "\n")
	if trimmed == "" {
		return block + "\n"
	}
	return trimmed + "\n\n" + block + "\n"
}

func writeBlock(target, content string) error {
	if err := fsync.AtomicWrite(target, []byte(content), fsync.Options{Mode: 0o644, Label: "setup-managed-block"}); err != nil {
		return fmt.Errorf("writing %s: %w", target, err)
	}
	return nil
}
