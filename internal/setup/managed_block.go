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

// findBlock locates the first managed block in content and reports its byte
// span [start, end) — start marker line through end marker line inclusive —
// and the version recorded on its start marker. ok is false when no
// well-formed block is present (no start marker, no matching end marker, or
// a start marker line that doesn't close with startMarkerSuffix on the same
// line — a hand-mangled marker is safer treated as absent than guessed at).
func findBlock(content string) (start, end int, version string, ok bool) {
	startIdx := strings.Index(content, startMarkerPrefix)
	if startIdx == -1 {
		return 0, 0, "", false
	}
	rest := content[startIdx:]
	lineEnd := strings.IndexByte(rest, '\n')
	var markerLine string
	if lineEnd == -1 {
		markerLine = rest
	} else {
		markerLine = rest[:lineEnd]
	}
	if !strings.HasSuffix(markerLine, startMarkerSuffix) {
		return 0, 0, "", false
	}
	version = strings.TrimSuffix(strings.TrimPrefix(markerLine, startMarkerPrefix), startMarkerSuffix)

	endIdx := strings.Index(rest, EndMarker)
	if endIdx == -1 {
		return 0, 0, "", false
	}
	endIdx += startIdx + len(EndMarker)
	return startIdx, endIdx, version, true
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
	start, end, gotVersion, ok := findBlock(content)
	if !ok {
		return StatusMissing, nil
	}
	if gotVersion != version {
		return StatusStale, nil
	}
	if content[start:end] != RenderBlock(body, version) {
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
	next := mergeBlock(content, block)
	if next == content {
		return false, nil
	}
	if err := writeBlock(target, next); err != nil {
		return false, err
	}
	return true, nil
}

// mergeBlock returns content with its managed block (if any) replaced by
// block, or block appended — separated from any existing content by one
// blank line — when content has none.
func mergeBlock(content, block string) string {
	if start, end, _, ok := findBlock(content); ok {
		return content[:start] + block + content[end:]
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
