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
//  4. Crash-durable (fsync-before-ack): the staged temp file is fsynced before
//     the rename, and the parent directory is fsynced after it, so a successful
//     call means the data AND the directory entry survive a hard crash. The
//     directory fsync is best-effort (some filesystems refuse it); the temp
//     fsync is fatal. [edits] fsync = false restores the old no-fsync behaviour.
//
//  5. Permissions preserved: if the target already exists, its mode bits are
//     copied to the temp file so the final file keeps the same permissions.
//
//  6. Concurrent-write detection (edit_file): before writing, we record the
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

	"github.com/plumbkit/plumb/internal/cache"
	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/paths"
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
// write so the next workspace_symbols / get_definition / file_outline sees fresh
// data without waiting for gopls to re-publish diagnostics (and without
// relying on the TTL expiring).
func invalidateCache(c *cache.Cache, uri string) {
	if c == nil {
		return
	}
	_ = c.InvalidateByPath(uri)
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
// canonicalised through paths.Canonical, so link paths and their real targets
// serialise through the same mutex — including for files that do not exist
// yet, which is the case the lock exists for (see lockPathKey).
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

// lockPathKey is the key every per-path bookkeeping agrees on: the write lock
// (pathLocks), the concurrent-change write tracker, the undo store, and
// edit_apply's lock-ordering dedup. It routes through paths.Canonical — the
// tree's one "same place?" answer (issue #273) — rather than open-coding a
// resolution. Two properties matter:
//
//   - A not-yet-existing path resolves by its nearest LIVE ancestor (the
//     missing-ancestor walk Canonical adds on top of EvalSymlinks). The
//     creation case is exactly where a path does not exist yet, so two
//     writers naming one file about to be created under a symlinked parent
//     by two different spellings take ONE mutex, not two.
//   - A relative path is cleaned, never anchored to the daemon's working
//     directory. Callers anchor relative arguments at the WORKSPACE before
//     the boundary check (resolvePath / WriteDeps.resolvePath), and the
//     daemon's cwd belongs to whichever client happened to spawn the
//     singleton (issue #181) — anchoring here would be a second, silent
//     answer to a question the boundary already decided.
func lockPathKey(path string) string {
	return paths.Canonical(paths.URIToPath(path))
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
// file (which os.Rename would otherwise do). A link that resolves nowhere has
// no target to write through: it keeps its spelling and the rename replaces it,
// exactly as before the resolution was delegated.
//
// perm is the file mode to use if the target does not yet exist. If the target
// already exists, its mode is preserved and perm is ignored.
func safeWrite(path string, data []byte, perm os.FileMode) (writeResult, error) {
	var res writeResult

	// Write THROUGH a symlinked target rather than replacing the link. The
	// resolution delegates to paths.Canonical — the tree's one "same place?"
	// answer — which also matches lockPathKey, so the write lands where the
	// write lock was taken. A not-yet-existing file under an aliased ancestor
	// resolves that ancestor (the spelling changes; the on-disk result is what
	// the kernel would have created anyway). A broken link degrades to its
	// cleaned spelling and the rename replaces it, exactly as the old
	// Lstat+EvalSymlinks pair did.
	path = paths.Canonical(path)

	// Snapshot the mtime before we touch anything.
	if info, err := os.Stat(path); err == nil {
		res.modTimeBeforeWrite = info.ModTime()
		perm = info.Mode().Perm() // preserve existing permissions
	}

	// Ensure parent directories exist — and are themselves durable, so a crash
	// cannot lose a freshly created tree the acknowledged write lives in.
	dir := filepath.Dir(path)
	if err := mkdirAllSynced("write", dir); err != nil {
		return res, fmt.Errorf("creating parent directories: %w", err)
	}

	// Known cross-device target: skip straight to the sibling path. Staging in
	// os.TempDir() first would write the data, fsync it, fail the rename with
	// EXDEV and throw it all away — per write, on the very common Linux setup
	// where /tmp is tmpfs.
	if crossDeviceKnown(dir) {
		return safeWriteSibling(path, data, perm, res.modTimeBeforeWrite)
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
	if err := syncTempFile(tmp); err != nil {
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
			// Cross-device: fall back to a sibling .plumb.tmp next to the target,
			// and remember the verdict so the next write to this directory skips
			// the doomed staging entirely.
			rememberCrossDevice(dir)
			_ = os.Remove(tmpPath)
			return safeWriteSibling(path, data, perm, res.modTimeBeforeWrite)
		}
		_ = os.Remove(tmpPath)
		return res, fmt.Errorf("renaming temp to target: %w", err)
	}

	// Fsync the parent directory so the rename's directory entry is durable
	// before we acknowledge the write (see the safety model at the top).
	syncDirBestEffort("write", filepath.Dir(path))

	return res, nil
}

// safeWriteSibling is the cross-device fallback: write a .plumb.tmp sibling
// of the target (guaranteed same filesystem), fsync it, then rename. This is
// the path taken whenever os.TempDir() and the target are on different
// filesystems — e.g. /tmp on tmpfs — so it must carry the full
// fsync-before-ack contract, not skip it.
func safeWriteSibling(path string, data []byte, perm os.FileMode, modTimeBefore time.Time) (writeResult, error) {
	res := writeResult{modTimeBeforeWrite: modTimeBefore}

	sibling := path + ".plumb.tmp"
	f, err := os.OpenFile(sibling, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm) //nolint:gosec // G703: path is validated and locked by the safeWrite contract before reaching this function
	if err != nil {
		return res, fmt.Errorf("creating sibling temp file: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(sibling)
		return res, fmt.Errorf("writing sibling temp file: %w", err)
	}
	if err := syncTempFile(f); err != nil {
		_ = f.Close()
		_ = os.Remove(sibling)
		return res, fmt.Errorf("syncing sibling temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(sibling)
		return res, fmt.Errorf("closing sibling temp file: %w", err)
	}
	res.tempWrittenAt = time.Now()

	if err := os.Rename(sibling, path); err != nil {
		_ = os.Remove(sibling)
		return res, fmt.Errorf("renaming sibling temp to target: %w", err)
	}
	syncDirBestEffort("write", filepath.Dir(path))
	return res, nil
}

// crossDeviceDirs remembers target directories that live on a different
// filesystem from os.TempDir(), so safeWrite stops paying for a staging write it
// knows will fail with EXDEV.
//
// BOUNDED, unlike the per-path lock table (which has an LRU sweep): on the
// common Linux setup where /tmp is tmpfs, EVERY directory the daemon writes is
// cross-device, so an unbounded map would be a slow leak for the life of the
// process. On overflow the whole set is dropped rather than evicted one by one
// — the only cost of forgetting is one wasted staging write per directory
// afterwards, and the entries are pure optimisation. A stale entry is equally
// harmless: the sibling path is always correct, just marginally less isolated
// than a tmpdir staging.
const crossDeviceCacheCap = 4096

var crossDeviceDirs = struct {
	mu   sync.Mutex
	dirs map[string]struct{}
}{dirs: make(map[string]struct{})}

// crossDeviceKnown reports whether a previous write to dir hit EXDEV.
func crossDeviceKnown(dir string) bool {
	crossDeviceDirs.mu.Lock()
	defer crossDeviceDirs.mu.Unlock()
	_, ok := crossDeviceDirs.dirs[dir]
	return ok
}

// rememberCrossDevice records that dir is on another filesystem from the tmpdir.
func rememberCrossDevice(dir string) {
	crossDeviceDirs.mu.Lock()
	defer crossDeviceDirs.mu.Unlock()
	if len(crossDeviceDirs.dirs) >= crossDeviceCacheCap {
		crossDeviceDirs.dirs = make(map[string]struct{}, crossDeviceCacheCap)
	}
	crossDeviceDirs.dirs[dir] = struct{}{}
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
	args := make([]string, 0, 4+len(files))
	args = append(args, "status", "--porcelain", "--")
	args = append(args, files...)
	// Read-only: never take .git/index.lock (see gitReadArgv). This runs on
	// every destructive write, so it is the likeliest source of a stranded lock.
	cmd := exec.CommandContext(ctx, "git", gitReadArgv(args)...) //nolint:gosec // G204: argv is package literals plus `files`, which are basenames within dir supplied by plumb's own write path and passed after the "--" separator, so they cannot be read as flags
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
// delete) must be refused for dirtiness: the guard is enabled ([edits]
// block_dirty_writes) AND the file is dirty (untracked files included —
// overwriting or deleting one is unrecoverable) AND plumb did not write it
// earlier this session. A file plumb wrote this session is its own uncommitted
// work, so re-editing it is never blocked; pre-existing uncommitted work still
// is. The caller gates on dirty_ok, which overrides this entirely.
func dirtyBlocksWrite(ctx context.Context, d WriteDeps, path string) bool {
	if !d.blockDirty() {
		return false
	}
	if d.writes(ctx).Wrote(path) {
		return false
	}
	return pathIsDirty(ctx, path)
}

// dirtyBlocksMove is the move/copy (content-preserving) variant of
// dirtyBlocksWrite: untracked sources don't count, and a source plumb wrote
// this session is never blocked. It, too, is a no-op when the guard is disabled.
func dirtyBlocksMove(ctx context.Context, d WriteDeps, path string) bool {
	if !d.blockDirty() {
		return false
	}
	if d.writes(ctx).Wrote(path) {
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
