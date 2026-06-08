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
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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

// defaultPostWriteDiagWindow is the fallback window used when WriteDeps.PostWriteDiagWindow
// is zero (i.e. not explicitly configured). Empirically ~150-250ms for gopls on incremental edits.
const defaultPostWriteDiagWindow = 300 * time.Millisecond

// awaitDiagnosticsRefresh waits for the language server to re-publish
// diagnostics for uri after a write, then returns the result. It subscribes to
// the next publishDiagnostics notification and returns the instant the server
// responds — not after a fixed sleep cycle. If the server does not respond in
// time, the most-recent diagnostics for uri are returned (which may predate the
// write).
//
// ceiling semantics: 0 → use defaultPostWriteDiagWindow; negative → disabled,
// return current diagnostics immediately without waiting.
//
// est (nil-safe) adapts the effective wait to how quickly this server actually
// re-publishes: the configured ceiling is an upper bound, and once a typical
// latency is known the wait shrinks toward it so a clean write — one the server
// never re-publishes for — stops paying the full ceiling. Observed publish
// latencies are fed back into est.
//
// The second return value, fresh, reports whether a publish arrived during the
// wait — i.e. the returned diagnostics reflect this write. When false (timeout,
// or the wait was disabled) the diagnostics may predate the write, and callers
// annotate their output accordingly.
func awaitDiagnosticsRefresh(diag postWriteDiagSource, uri string, ceiling time.Duration, est *DiagWaitEstimator) (diags []protocol.Diagnostic, fresh bool) {
	if diag == nil {
		return nil, false
	}
	if ceiling < 0 {
		// Disabled: return the last-known snapshot without waiting — it may
		// predate this write, so it is never fresh.
		return diag.Diagnostics(uri), false
	}
	if ceiling == 0 {
		ceiling = defaultPostWriteDiagWindow
	}
	ctx, cancel := context.WithTimeout(context.Background(), est.window(ceiling))
	defer cancel()
	start := time.Now()
	d, err := diag.WaitNextDiagnostics(ctx, uri)
	if err == nil {
		// A publish landed during the wait, so it reflects this write.
		est.record(time.Since(start))
		return d, true
	}
	return d, false
}

// postWriteDiagSource is the narrow interface write/edit tools need to
// observe post-write diagnostic changes. Satisfied by *cache.Invalidator
// and the daemon's invProxy / routingInvProxy.
type postWriteDiagSource interface {
	Diagnostics(uri string) []protocol.Diagnostic
	WaitNextDiagnostics(ctx context.Context, uri string) ([]protocol.Diagnostic, error)
}

// lineCount reports the number of source lines in s, generously (a trailing
// newline counts as an extra empty line). Used to decide whether a post-write
// diagnostic points beyond the file's current end. Being generous biases the
// out-of-range check toward NOT down-ranking a borderline last-line diagnostic.
func lineCount(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

// renderDiagGroup appends up to maxPerCategory diagnostics under label, then a
// "…(+N more)" overflow line.
func renderDiagGroup(sb *strings.Builder, label string, diags []protocol.Diagnostic) {
	const maxPerCategory = 3
	for i, x := range diags {
		if i >= maxPerCategory {
			fmt.Fprintf(sb, "\n  %s: …(+%d more)", label, len(diags)-maxPerCategory)
			return
		}
		fmt.Fprintf(sb, "\n  %s L%d: %s", label, x.Range.Start.Line+1, x.Message)
	}
}

// formatPostWriteDiagnostics renders up to N error/warning diagnostics as a
// compact suffix appended to write/edit_file output. Returns "" if none.
//
// Two staleness guards reduce phantom breakage after a write — the single
// most-reported friction in internal/feedbacks.md:
//
//   - fresh=false: the language server had not re-published within the wait
//     window, so the snapshot may predate this write; a hedge note is appended.
//   - newLineCount>0: any error/warning whose line lies beyond the just-written
//     file's current end is provably stale (it points past EOF — the classic
//     case after a structural edit that shrank the file, where gopls still
//     reports old line numbers). These are split into a "stale?" group and never
//     rendered as a hard "error", so an agent does not chase phantom breakage.
func formatPostWriteDiagnostics(d []protocol.Diagnostic, fresh bool, newLineCount int) string {
	if len(d) == 0 {
		return ""
	}
	var errs, warns, stale []protocol.Diagnostic
	for _, x := range d {
		if x.Severity != protocol.SevError && x.Severity != protocol.SevWarning {
			continue
		}
		if newLineCount > 0 && int(x.Range.Start.Line) >= newLineCount {
			stale = append(stale, x)
			continue
		}
		if x.Severity == protocol.SevError {
			errs = append(errs, x)
		} else {
			warns = append(warns, x)
		}
	}
	if len(errs) == 0 && len(warns) == 0 && len(stale) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\ndiagnostics after write:")
	renderDiagGroup(&sb, "error", errs)
	renderDiagGroup(&sb, "warn", warns)
	renderDiagGroup(&sb, "stale?", stale)
	if len(stale) > 0 {
		sb.WriteString("\n  (stale? = past the file's current end — almost certainly a pre-edit diagnostic the language server has not yet cleared; rebuild to confirm)")
	}
	if !fresh {
		sb.WriteString("\n  (may predate this write — the language server had not re-analysed within the wait window; re-check with diagnostics)")
	}
	return sb.String()
}

// pathLockEntry is the value stored in pathLocks. It pairs a per-path mutex
// with an atomic timestamp recording when the path was last accessed. The
// timestamp is updated on every lockPath call and on every unlock, so the
// background LRU sweep can safely evict entries that have been idle for
// longer than pathLockIdleExpiry.
type pathLockEntry struct {
	mu         sync.Mutex
	lastUsedNs atomic.Int64 // Unix nanoseconds; read/written via sync/atomic
}

// pathLocks serialises write operations to the same on-disk path across all
// concurrent tool calls in this process. The map is consulted by lockPath.
// Without it, two simultaneous edit_file calls to the same file could each
// read the pre-edit content, apply their edits independently, and the second
// writer would silently overwrite the first.
//
// Entries are evicted by StartPathLockSweep after they have been idle for
// pathLockIdleExpiry. The sweep is started once per daemon lifetime.
var pathLocks sync.Map // map[string]*pathLockEntry

const (
	pathLockSweepInterval = 5 * time.Minute
	pathLockIdleExpiry    = 1 * time.Hour
)

// StartPathLockSweep launches a background goroutine that evicts idle entries
// from pathLocks every pathLockSweepInterval. It should be called once from
// the daemon's run loop, passing the daemon's lifetime context.
func StartPathLockSweep(ctx context.Context) {
	go func() {
		t := time.NewTicker(pathLockSweepInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				sweepPathLocks(time.Now())
			}
		}
	}()
}

// sweepPathLocks removes entries from pathLocks that have been idle for longer
// than pathLockIdleExpiry as of now. It uses TryLock to skip entries that are
// currently held, and re-checks idleness after acquiring to guard against a
// lock that became active between the Range iteration and the TryLock call.
func sweepPathLocks(now time.Time) {
	pathLocks.Range(func(key, value any) bool {
		e := value.(*pathLockEntry)
		lastUsed := time.Unix(0, e.lastUsedNs.Load())
		if now.Sub(lastUsed) < pathLockIdleExpiry {
			return true // recently used — keep
		}
		if !e.mu.TryLock() {
			return true // currently held — skip
		}
		// Re-check: the entry might have been claimed between Range and TryLock.
		lastUsed = time.Unix(0, e.lastUsedNs.Load())
		if now.Sub(lastUsed) < pathLockIdleExpiry {
			e.mu.Unlock()
			return true
		}
		pathLocks.Delete(key)
		e.mu.Unlock()
		return true
	})
}

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

// lockPath returns a release function that unlocks the path. The lock key is
// canonicalised through symlinks when possible so link paths and their real
// targets serialise through the same mutex.
//
// The entry's lastUsedNs is stamped on every call (before blocking on Lock)
// and again when the caller releases, so the LRU sweep never evicts an entry
// that is either about to be locked or was recently released.
func lockPath(path string) func() {
	key := lockPathKey(path)
	now := time.Now().UnixNano()
	newEntry := &pathLockEntry{}
	newEntry.lastUsedNs.Store(now)
	v, _ := pathLocks.LoadOrStore(key, newEntry)
	e := v.(*pathLockEntry)
	// Mark the entry as wanted even if we got back an existing one — this
	// prevents the sweep from evicting it while we are waiting for mu.Lock.
	e.lastUsedNs.Store(now)
	e.mu.Lock()
	return func() {
		e.lastUsedNs.Store(time.Now().UnixNano())
		e.mu.Unlock()
	}
}

func lockPathKey(path string) string {
	path = strings.TrimPrefix(path, "file://")
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return filepath.Clean(path)
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
	if err := os.WriteFile(sibling, data, perm); err != nil { //nolint:gosec // G703: path is validated and locked by the safeWrite contract before reaching this function
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
const defaultConcurrentWriteSkew = 100 * time.Millisecond

func concurrentWriteDetected(path string, res writeResult, skew time.Duration) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	if skew <= 0 {
		skew = defaultConcurrentWriteSkew
	}
	mtime := info.ModTime()
	// If mtime predates when we started writing the temp, the file hasn't
	// been touched by anyone — our rename set it to approximately tempWrittenAt.
	// If mtime is much newer than our write, a third party wrote after us.
	return mtime.After(res.tempWrittenAt.Add(skew))
}

// dirtyBasenamesInDir runs one git status --porcelain call for a set of files
// within dir, returning a set of dirty basenames. Returns nil (no dirty files)
// when git is not on PATH or dir is not inside a git repository.
//
// Batching files from the same directory avoids spawning one git process per
// file in transaction_apply. Git errors (not a repo, unreachable, etc.) are
// silently treated as "not dirty" to avoid false positives.
//
// skipUntracked controls how untracked files (porcelain "??") are reported. A
// destructive write (overwrite, edit, delete) that lands on an untracked file
// destroys content git cannot recover, so those callers pass false and an
// untracked file counts as dirty. A move/copy (rename_file, copy_file)
// preserves the source content, so those callers pass true to skip untracked
// files and avoid blocking on a brand-new file that has nothing at HEAD to lose.
func dirtyBasenamesInDir(ctx context.Context, dir string, files []string, skipUntracked bool) map[string]bool {
	if _, err := exec.LookPath("git"); err != nil {
		return nil
	}
	args := make([]string, 0, 3+len(files))
	args = append(args, "status", "--porcelain", "--")
	args = append(args, files...)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil || len(out) == 0 {
		return nil
	}
	dirty := make(map[string]bool)
	for line := range strings.SplitSeq(strings.TrimRight(string(out), "\n"), "\n") {
		if len(line) < 4 {
			continue
		}
		// Porcelain v1: "XY filename" where XY are two status characters.
		// Untracked entries are "??"; skip them only for callers that preserve
		// content (move/copy), since an untracked file has no committed state.
		if skipUntracked && line[0] == '?' {
			continue
		}
		// Rename format: "R  old -> new" — take the new name after " -> ".
		name := line[3:]
		if i := strings.Index(name, " -> "); i >= 0 {
			name = name[i+4:]
		}
		dirty[strings.TrimSpace(name)] = true
	}
	return dirty
}

// pathIsDirty reports whether path has uncommitted changes that a destructive
// write (overwrite, edit, delete) would lose. Untracked files count as dirty:
// their entire content is uncommitted, so overwriting or deleting one is
// unrecoverable. Returns false when git is not on PATH or path is not inside a
// git repository. Git errors are silently treated as not dirty to avoid
// blocking writes.
func pathIsDirty(ctx context.Context, path string) bool {
	return dirtyBasenamesInDir(ctx, filepath.Dir(path), []string{filepath.Base(path)}, false)[filepath.Base(path)]
}

// pathIsDirtyIgnoringUntracked is the move/copy variant of pathIsDirty: it
// reports uncommitted changes to content already in git history but does not
// count untracked files as dirty. rename_file and copy_file preserve the
// source content, so a brand-new (untracked) source need not be committed first.
func pathIsDirtyIgnoringUntracked(ctx context.Context, path string) bool {
	return dirtyBasenamesInDir(ctx, filepath.Dir(path), []string{filepath.Base(path)}, true)[filepath.Base(path)]
}

// dirtyBlocksWrite reports whether a destructive write to path (overwrite, edit,
// delete) must be refused for dirtiness: the file is dirty (untracked files
// included — overwriting or deleting one is unrecoverable) AND plumb did not
// write it earlier this session. A file plumb wrote this session is its own
// uncommitted work, so re-editing it is never blocked; pre-existing uncommitted
// work still is. The caller gates on dirty_ok, which overrides this entirely.
func dirtyBlocksWrite(ctx context.Context, writes *WriteTracker, path string) bool {
	if writes.Wrote(path) {
		return false
	}
	return pathIsDirty(ctx, path)
}

// dirtyBlocksMove is the move/copy (content-preserving) variant of
// dirtyBlocksWrite: untracked sources don't count, and a source plumb wrote
// this session is never blocked.
func dirtyBlocksMove(ctx context.Context, writes *WriteTracker, path string) bool {
	if writes.Wrote(path) {
		return false
	}
	return pathIsDirtyIgnoringUntracked(ctx, path)
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
func notifyLSP(ctx context.Context, client lsp.Client, path string, changeType protocol.FileChangeType) error {
	if client == nil {
		return nil
	}
	return client.DidChangeWatchedFiles(ctx, protocol.DidChangeWatchedFilesParams{
		Changes: []protocol.FileEvent{{
			URI:  protocol.FileURI(path),
			Type: changeType,
		}},
	})
}
