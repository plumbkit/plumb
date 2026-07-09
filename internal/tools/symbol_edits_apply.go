package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/plumbkit/plumb/internal/cache"
	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/paths"
)

// resolveShowDiff resolves a per-session show_write_diff toggle. A nil resolver
// means the tool was constructed without WithShowWriteDiff (e.g. in tests), in
// which case it defaults to on — matching the config default.
func resolveShowDiff(fn func() bool) bool {
	if fn == nil {
		return true
	}
	return fn()
}

type symbolEditResolver func(context.Context) (protocol.TextEdit, *protocol.DocumentSymbol, string, error)

type symbolEditRefusal struct{ msg string }

func (e symbolEditRefusal) Error() string { return e.msg }

// treeSitterFallbackLegacyNote prefixes a symbol-edit response whose target was
// located by tree-sitter because the language server is genuinely unavailable.
const treeSitterFallbackLegacyNote = "[topology fallback — LSP unavailable; symbol located by tree-sitter, range is line-granular]\n\n"

// treeSitterFallbackNote picks the symbol-edit fallback banner: the warming
// variant when the server that would own uri is still completing its handshake
// (so the agent retries instead of concluding the LSP is broken), else the
// legacy genuinely-unavailable text, byte-identical to the historical banner.
// fn is nil-safe.
func treeSitterFallbackNote(fn LSPWarmupFn, uri string) string {
	warming, elapsed := lspWarmup(fn, uri)
	if !warming {
		return treeSitterFallbackLegacyNote
	}
	return fmt.Sprintf("[topology fallback — LSP still warming%s; symbol located by tree-sitter, range is line-granular]\n\n",
		warmupElapsedSuffix(elapsed))
}

// symbolEditFallbackNote resolves the response banner for a symbol-edit whose
// target came from the tree-sitter fallback; "" when the language server
// resolved it (no banner).
func symbolEditFallbackNote(viaFallback bool, fn LSPWarmupFn, uri string) string {
	if !viaFallback {
		return ""
	}
	return treeSitterFallbackNote(fn, uri)
}

// applySingleEdit runs the standard apply-or-preview flow used by every
// symbol-edit tool. summary is the human-readable verb ("inserted before",
// "replaced", etc.) used in the dry-run / applied output. The resolver's third
// result is the fallback banner (see symbolEditFallbackNote) — non-empty when
// the symbol was located by tree-sitter rather than the language server; it is
// prepended to the response. When showDiff is true
// the response carries a unified diff of the change — a preview in dry-run, the
// applied change otherwise.
//
// In apply mode the target file is locked BEFORE the resolver runs, so the LSP
// range is resolved against the same on-disk bytes the edit is applied to. The
// successful write then flows through the same write-side bookkeeping as the
// ordinary write tools when WriteDeps is wired: LSP notify, adapter post-write
// hook, cache invalidation, write/read tracker refresh, undo, topology, quality,
// and differential diagnostics. client/cache remain nil-safe for unit tests.
func applySingleEdit(ctx context.Context, client lsp.Client, c *cache.Cache, deps *WriteDeps, uri string, dryRun, showDiff bool, summary, toolName string, dirtyOK bool, resolve symbolEditResolver) (string, error) {
	path := paths.URIToPath(uri)
	if err := semanticWritePreflight(ctx, deps, toolName, path, dryRun, dirtyOK); err != nil {
		return "", err
	}
	var sb strings.Builder
	if dryRun {
		edit, sym, fallbackNote, err := resolve(ctx)
		if err != nil {
			var refusal symbolEditRefusal
			if errors.As(err, &refusal) {
				return refusal.msg, nil
			}
			return "", err
		}
		diff := ""
		if showDiff {
			diff = symbolEditDiff(path, edit)
		}
		sb.WriteString(fallbackNote)
		sb.WriteString("DRY RUN — file not modified.\n\n")
		fmt.Fprintf(&sb, "Would %s symbol %q in %s\n", summary, sym.Name, path)
		fmt.Fprintf(&sb, "  Range: line %d char %d → line %d char %d\n",
			edit.Range.Start.Line, edit.Range.Start.Character,
			edit.Range.End.Line, edit.Range.End.Character)
		if diff != "" {
			sb.WriteString("\n")
			sb.WriteString(diff)
		}
		sb.WriteString("\nTo apply, re-run with dry_run=false.")
		return sb.String(), nil
	}

	unlock := lockPath(path)
	defer unlock()

	edit, sym, fallbackNote, err := resolve(ctx)
	if err != nil {
		var refusal symbolEditRefusal
		if errors.As(err, &refusal) {
			return refusal.msg, nil
		}
		return "", err
	}
	baseline := captureSemanticBaseline(deps, uri)
	before, after, mode, err := prepareTextEditsLocked(path, []protocol.TextEdit{edit})
	if err != nil {
		return "", fmt.Errorf("applying edit: %w", err)
	}
	if _, err := safeWrite(path, after, mode); err != nil {
		return "", fmt.Errorf("applying edit: %w", err)
	}
	diff := ""
	if showDiff {
		diff = unifiedDiff(path, string(before), string(after))
	}
	sb.WriteString(fallbackNote)
	fmt.Fprintf(&sb, "%s symbol %q in %s\n", capitalise(summary), sym.Name, path)
	if diff != "" {
		sb.WriteString("\n")
		sb.WriteString(diff)
	}
	sb.WriteString(semanticPostWrite(ctx, deps, client, c, path, uri, string(before), string(after), toolName, baseline))
	return sb.String(), nil
}

func semanticWritePreflight(ctx context.Context, deps *WriteDeps, toolName, path string, dryRun, dirtyOK bool) error {
	if deps == nil {
		return nil
	}
	if err := deps.checkBoundary(path); err != nil {
		return fmt.Errorf("%s: %w", toolName, err)
	}
	if dryRun {
		return nil
	}
	if deps.Limiter != nil && !deps.Limiter.Allow() {
		return rateLimitError(toolName, deps.Limiter)
	}
	if !dirtyOK && dirtyBlocksWrite(ctx, *deps, path) {
		return fmt.Errorf("%s: %q has uncommitted changes; review and commit first, or pass dirty_ok: true to proceed", toolName, path)
	}
	return nil
}

func captureSemanticBaseline(deps *WriteDeps, uri string) *diagBaseline {
	if deps == nil {
		return nil
	}
	return deps.capturePreWriteBaseline(uri)
}

// semanticPostWrite is the full post-write pipeline for callers still holding
// the target's path lock: write-tracker/undo bookkeeping (which requires the
// held lock) plus the notify/diagnostics/quality half.
func semanticPostWrite(ctx context.Context, deps *WriteDeps, client lsp.Client, c *cache.Cache, path, uri, before, after, toolName string, baseline *diagBaseline) string {
	if deps == nil {
		notifySymbolEditWritten(ctx, client, c, path, uri)
		return ""
	}
	deps.recordWritten(path)
	deps.recordUndo(path, before, after, true, toolName)
	return semanticNotifyPostWrite(ctx, deps, client, c, path, uri, before, after, toolName, baseline)
}

// semanticNotifyPostWrite is the lock-free half of the post-write pipeline:
// LSP notify, adapter hook, cache invalidation, topology refresh, differential
// diagnostics, and quality report. Callers that release the path lock before
// running it (rename_symbol's multi-file path) must have recorded the
// write-tracker/undo bookkeeping under the lock first.
func semanticNotifyPostWrite(ctx context.Context, deps *WriteDeps, client lsp.Client, c *cache.Cache, path, uri, before, after, toolName string, baseline *diagBaseline) string {
	semanticNotifyWritten(ctx, deps, client, c, path, uri, toolName)
	return semanticPostWriteReport(ctx, deps, path, uri, before, after, baseline)
}

// semanticNotifyWritten is the cheap, non-blocking part of the post-write
// pipeline: tell the language server the file changed on disk, run any
// adapter-specific hook, evict the symbol cache, and enqueue a topology
// re-index. A multi-file caller runs this for EVERY modified file — none of it
// waits on the server.
func semanticNotifyWritten(ctx context.Context, deps *WriteDeps, client lsp.Client, c *cache.Cache, path, uri, toolName string) {
	writeClient := deps.Client
	if writeClient == nil {
		writeClient = client
	}
	writeCache := deps.Cache
	if writeCache == nil {
		writeCache = c
	}
	if err := notifyLSP(ctx, writeClient, path, protocol.FileChanged); err != nil {
		slog.Warn(toolName+": LSP notification failed", "path", path, "err", err)
	}
	if deps.PostWriteNotifyFn != nil {
		if err := deps.PostWriteNotifyFn(ctx, path); err != nil {
			slog.Warn(toolName+": post-write adapter notification failed", "path", path, "err", err)
		}
	}
	invalidateCache(writeCache, uri)
	deps.notifyTopology(path)
}

// semanticPostWriteReport is the expensive part: it blocks for up to
// post_write_diagnostics_ms waiting for the server to re-publish, then runs the
// offline quality analysers. Cost is per file and serial, so a caller with many
// modified files must bound how many times it calls this (see
// maxRenameReportFiles).
func semanticPostWriteReport(ctx context.Context, deps *WriteDeps, path, uri, before, after string, baseline *diagBaseline) string {
	return deps.postWriteDiagnostics(uri, before, after, false, baseline) + deps.reportQuality(ctx, path)
}

// notifySymbolEditWritten performs the post-write housekeeping shared by the
// symbol-edit apply paths: it tells the language server the file changed on disk
// (workspace/didChangeWatchedFiles) and evicts the symbol cache for uri. This is
// byte-identical to what edit_file/write_file do after every write; keeping it
// here means a semantic edit no longer leaves the server holding stale content.
// Best-effort — a notification failure is logged, never fatal to a write that
// already landed. client and c are nil-safe.
func notifySymbolEditWritten(ctx context.Context, client lsp.Client, c *cache.Cache, path, uri string) {
	if err := notifyLSP(ctx, client, path, protocol.FileChanged); err != nil {
		slog.Warn("symbol edit: LSP notification failed", "path", path, "err", err)
	}
	invalidateCache(c, uri)
}

// symbolEditDiff renders the unified diff a single TextEdit would produce
// against the current on-disk content. Returns "" if the file can't be read or
// the edit can't be applied in-memory — the diff is best-effort presentation,
// never a hard failure of the edit itself.
func symbolEditDiff(path string, edit protocol.TextEdit) string {
	old, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	out, err := applyTextEdits(old, []protocol.TextEdit{edit})
	if err != nil {
		return ""
	}
	return unifiedDiff(path, string(old), string(out))
}

// symbolEditsDiff renders the unified diff a set of TextEdits would produce
// against path's current on-disk content. Best-effort: returns "" when there are
// no edits, the file can't be read, or the edits can't be reconstructed
// in-memory — the diff is presentation only, never a hard failure of the edit.
func symbolEditsDiff(path string, edits []protocol.TextEdit) string {
	if len(edits) == 0 {
		return ""
	}
	old, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	out, err := applyTextEdits(old, edits)
	if err != nil {
		return ""
	}
	return unifiedDiff(path, string(old), string(out))
}

func capitalise(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
