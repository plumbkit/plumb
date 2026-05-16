package tools

// file_write_helpers.go — shared primitives for all file-write operations.
//
// Safety model, in layers:
//
//  1. Atomic rename: every write stages content in a temp file and renames it
//     into place. os.Rename is a single POSIX syscall — the target is always
//     either the old file or the new one, never partially written.
//
//  2. Symlink-aware: if the target is a symlink, the link is resolved and the
//     write goes to the underlying file. Without this, os.Rename would replace
//     the symlink with a regular file, silently breaking the link.
//
//  3. Temp file location: temp files go to os.TempDir() to avoid polluting the
//     project tree. If the target is on a different filesystem (os.Rename returns
//     EXDEV), we fall back to a sibling .plumb.tmp next to the target, which is
//     guaranteed same-filesystem. The temp file is always cleaned up on failure.
//
//  4. Permissions preserved: if the target already exists, its mode bits are
//     copied to the temp file so the final file keeps the same permissions.
//
//  5. Concurrent-write detection (edit_file): before writing, we record the
//     target's mtime. After the rename, we re-stat the file and compare mtimes.
//     Because we just wrote the file, the mtime should be >= our pre-write
//     snapshot. If the file is newer than our temp (i.e. a third party wrote it
//     during our operation), we know we've overwritten a concurrent change.
//     edit_file uses this to trigger a retry loop (max 3 attempts).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/golimpio/plumb/internal/cache"
	"github.com/golimpio/plumb/internal/lsp"
	"github.com/golimpio/plumb/internal/lsp/protocol"
)

// fileSHA256 computes the hex-encoded SHA-256 of the named file's full
// content. Used by read_file (header output) and edit_file / transaction_apply
// (optional expected_sha concurrency check).
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// invalidateCache evicts every cache entry whose key references uri. Safe
// when c is nil. Called by all write tools immediately after a successful
// write so the next find_symbol / get_definition / list_symbols sees fresh
// data without waiting for gopls to re-publish diagnostics (and without
// relying on the TTL expiring).
func invalidateCache(c *cache.Cache, uri string) {
	if c == nil {
		return
	}
	_ = c.InvalidateByPath(uri)
}

// postWriteDiagWindow is how long write/edit tools wait for the LSP server
// to re-publish diagnostics for the URI they just wrote. Short enough that
// fast servers (gopls on a small package) usually deliver within the window,
// long enough that the agent doesn't have to round-trip again to find out
// it broke the build. Empirically ~150-250ms for gopls on incremental edits.
const postWriteDiagWindow = 300 * time.Millisecond

// awaitDiagnosticsRefresh waits up to postWriteDiagWindow for the diagnostics
// for uri to change from the supplied baseline. Returns the post-write
// diagnostics slice (which may equal the baseline if the server didn't
// republish in time). nil-safe on the diagnosticsSource argument.
func awaitDiagnosticsRefresh(diag postWriteDiagSource, uri string, baseline []protocol.Diagnostic) []protocol.Diagnostic {
	if diag == nil {
		return nil
	}
	deadline := time.Now().Add(postWriteDiagWindow)
	for {
		current := diag.Diagnostics(uri)
		if !diagnosticsEqual(current, baseline) {
			return current
		}
		if time.Now().After(deadline) {
			return current
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// postWriteDiagSource is the narrow interface write/edit tools need to
// observe post-write diagnostic changes. Satisfied by *cache.Invalidator
// and the daemon's invProxy / routingInvProxy.
type postWriteDiagSource interface {
	Diagnostics(uri string) []protocol.Diagnostic
}

// diagnosticsEqual returns true if a and b have the same number of entries
// in the same order with equal Severity, Message, and Range.Start.Line.
// Used to detect "did anything change" not "are these the same diagnostic
// objects."
func diagnosticsEqual(a, b []protocol.Diagnostic) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Severity != b[i].Severity || a[i].Message != b[i].Message ||
			a[i].Range.Start.Line != b[i].Range.Start.Line {
			return false
		}
	}
	return true
}

// formatPostWriteDiagnostics renders up to N error/warning diagnostics as a
// compact suffix appended to write/edit_file output. Returns "" if none.
func formatPostWriteDiagnostics(d []protocol.Diagnostic) string {
	if len(d) == 0 {
		return ""
	}
	var errs, warns []protocol.Diagnostic
	for _, x := range d {
		switch x.Severity {
		case protocol.SevError:
			errs = append(errs, x)
		case protocol.SevWarning:
			warns = append(warns, x)
		}
	}
	if len(errs) == 0 && len(warns) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\ndiagnostics after write:")
	render := func(label string, diags []protocol.Diagnostic) {
		const maxPerCategory = 3
		for i, x := range diags {
			if i >= maxPerCategory {
				fmt.Fprintf(&sb, "\n  %s: …(+%d more)", label, len(diags)-maxPerCategory)
				return
			}
			fmt.Fprintf(&sb, "\n  %s L%d: %s", label, x.Range.Start.Line+1, x.Message)
		}
	}
	render("error", errs)
	render("warn", warns)
	return sb.String()
}

// pathLocks serialises write operations to the same on-disk path across all
// concurrent tool calls in this process. The map is consulted by lockPath /
// unlockPath. Without it, two simultaneous edit_file calls to the same file
// could each read the pre-edit content, each apply their edits independently,
// and the second writer would silently overwrite the first.
var pathLocks sync.Map // map[string]*sync.Mutex

// StrictModeFn returns the current strict-mode setting. The daemon
// installs a closure that reads from the resolved per-workspace config
// (with global + env-var override fallbacks); tests pass nil for "off".
type StrictModeFn func() bool

// strictModeEnabled is the env-only fallback, used when no StrictModeFn is
// wired on the tool (test setups, headless dev). Production flows route
// through the tool's configured StrictModeFn closure.
func strictModeEnabled() bool {
	v := os.Getenv("PLUMB_STRICT_EDITS")
	return v == "1" || v == "true" || v == "yes"
}

// lockPath returns a release function that unlocks the path. The path is
// canonicalised via filepath.Clean so file:// URIs and absolute paths share
// the same lock.
func lockPath(path string) func() {
	key := filepath.Clean(path)
	v, _ := pathLocks.LoadOrStore(key, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// writeResult is returned by safeWrite and carries metadata about the write
// for the concurrent-change detection logic in edit_file.
type writeResult struct {
	// modTimeBeforeWrite is the mtime of the target file snapshotted before
	// the write began. Zero for new files.
	modTimeBeforeWrite time.Time
	// tempWrittenAt is the time at which os.WriteFile completed writing to
	// the temp file. Used as a reference to detect whether the target was
	// modified by a third party after we started but before our rename landed.
	tempWrittenAt time.Time
}

// safeWrite writes data to path using temp-file-then-atomic-rename.
//
// The temp file is created in os.TempDir(). If the rename fails with EXDEV
// (cross-device), we retry using a sibling .plumb.tmp in the same directory
// as the target — guaranteed same filesystem. The sibling is removed on any
// failure.
//
// If path is a symlink, the link is resolved and the write goes to the target
// of the link. This preserves the link rather than replacing it with a regular
// file (which os.Rename would otherwise do).
//
// perm is the file mode to use if the target does not yet exist. If the target
// already exists, its mode is preserved and perm is ignored.
func safeWrite(path string, data []byte, perm os.FileMode) (writeResult, error) {
	var res writeResult

	// If path is a symlink, resolve to the real target so we write through the
	// link instead of replacing it. os.Lstat does not follow the link; we use
	// it to detect the symlink, then resolve with EvalSymlinks.
	if linfo, err := os.Lstat(path); err == nil && linfo.Mode()&os.ModeSymlink != 0 {
		resolved, rerr := filepath.EvalSymlinks(path)
		if rerr == nil {
			path = resolved
		}
	}

	// Snapshot the mtime before we touch anything.
	if info, err := os.Stat(path); err == nil {
		res.modTimeBeforeWrite = info.ModTime()
		perm = info.Mode().Perm() // preserve existing permissions
	}

	// Ensure parent directories exist.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return res, fmt.Errorf("creating parent directories: %w", err)
	}

	// Write to a temp file in os.TempDir() first.
	tmp, err := os.CreateTemp("", "plumb-write-*")
	if err != nil {
		return res, fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmp.Name()

	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return res, fmt.Errorf("setting temp file permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return res, fmt.Errorf("writing temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return res, fmt.Errorf("syncing temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return res, fmt.Errorf("closing temp file: %w", err)
	}

	res.tempWrittenAt = time.Now()

	// Attempt rename from tmpdir → target.
	if err := os.Rename(tmpPath, path); err != nil {
		if isCrossDevice(err) {
			// Cross-device: fall back to a sibling .plumb.tmp next to the target.
			_ = os.Remove(tmpPath)
			return safeWriteSibling(path, data, perm, res.modTimeBeforeWrite)
		}
		_ = os.Remove(tmpPath)
		return res, fmt.Errorf("renaming temp to target: %w", err)
	}

	return res, nil
}

// safeWriteSibling is the cross-device fallback: write a .plumb.tmp sibling
// of the target (guaranteed same filesystem), then rename.
func safeWriteSibling(path string, data []byte, perm os.FileMode, modTimeBefore time.Time) (writeResult, error) {
	res := writeResult{modTimeBeforeWrite: modTimeBefore}

	sibling := path + ".plumb.tmp"
	if err := os.WriteFile(sibling, data, perm); err != nil {
		return res, fmt.Errorf("writing sibling temp file: %w", err)
	}
	res.tempWrittenAt = time.Now()

	if err := os.Rename(sibling, path); err != nil {
		_ = os.Remove(sibling)
		return res, fmt.Errorf("renaming sibling temp to target: %w", err)
	}
	return res, nil
}

// isCrossDevice reports whether err is a cross-device rename failure (EXDEV).
func isCrossDevice(err error) bool {
	if le, ok := errors.AsType[*os.LinkError](err); ok {
		if errno, ok := errors.AsType[syscall.Errno](le.Err); ok {
			return errno == syscall.EXDEV
		}
	}
	return false
}

// concurrentWriteDetected reports whether the file at path appears to have
// been modified by a third party during our write operation.
//
// After our rename, the file's mtime should be >= tempWrittenAt (the OS sets
// mtime to now on rename). If the mtime is significantly newer than our write
// time, a concurrent writer snuck in after our rename — this is a false
// negative we cannot detect. But if the mtime equals the pre-write snapshot,
// it means our rename never landed (shouldn't happen) or the file was already
// at that mtime (race: third-party write happened between our stat and rename).
//
// The meaningful case we do detect: if the *current* mtime is newer than our
// tempWrittenAt by more than a small clock-skew allowance (100ms), it strongly
// suggests a third party wrote the file after our rename. We treat this as a
// concurrent write and trigger retry.
func concurrentWriteDetected(path string, res writeResult) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	mtime := info.ModTime()
	// If mtime predates when we started writing the temp, the file hasn't
	// been touched by anyone — our rename set it to approximately tempWrittenAt.
	// If mtime is much newer than our write, a third party wrote after us.
	const skew = 100 * time.Millisecond
	return mtime.After(res.tempWrittenAt.Add(skew))
}

// notifyLSP tells the server "this file on disk just changed" via
// workspace/didChangeWatchedFiles — the LSP-correct primitive for external
// file changes. A single notification, no language-ID guessing, no version
// counters, no implicit buffer ownership.
//
// changeType should be FileCreated for new files and FileChanged for
// overwrites/edits. FileDeleted is for callers that delete on disk.
//
// Best-effort: a notification failure must never roll back a successful file
// write. Callers log and continue.
func notifyLSP(ctx context.Context, client lsp.LSPClient, path string, changeType protocol.FileChangeType) error {
	if client == nil {
		return nil
	}
	return client.DidChangeWatchedFiles(ctx, protocol.DidChangeWatchedFilesParams{
		Changes: []protocol.FileEvent{{
			URI:  "file://" + path,
			Type: changeType,
		}},
	})
}
