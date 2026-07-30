package tools

// file_write_fsync.go holds the fsync-before-ack seams shared by every write
// primitive in this package: the [edits] fsync knob installer, the stubbable
// sync hooks, and the two helpers the primitives call (a fatal temp-file sync
// before a rename, a best-effort directory sync after one). The write-safety
// model they serve is documented at the top of file_write_helpers.go.

import (
	"log/slog"
	"os"
	"path/filepath"
	"slices"

	"github.com/plumbkit/plumb/internal/fsync"
)

// SetFsyncFunc installs the function consulted for the [edits] fsync knob
// (default true). safeWrite and the other write primitives are free functions,
// so the WriteDeps Fn-closure pattern cannot reach them — this package-level
// setter is called once from the daemon startup path instead, with a closure
// reading the GLOBAL config store. That makes the knob daemon-global by
// design: installing it per connection would let the last workspace to attach
// set the durability contract for every other live session. When the knob is
// off, BOTH the temp-file fsync and the post-rename directory fsync are
// skipped, restoring the pre-fix behaviour for benchmarks and exotic
// filesystems.
func SetFsyncFunc(fn func() bool) { fsync.SetEnabledFunc(fn) }

// syncFileHook and syncDirHook are the fsync-before-ack seams. Production
// wires them to the fsync package (which applies the [edits] fsync knob);
// tests stub them to assert the write paths actually invoke the syncs.
var (
	syncFileHook = fsync.SyncFile
	syncDirHook  = fsync.SyncDir
)

// syncTempFile fsyncs a staged temp file before its rename. Fatal on error
// (the caller aborts the write); skipped entirely when the fsync knob is off.
func syncTempFile(f *os.File) error {
	if !fsync.Enabled() {
		return nil
	}
	return syncFileHook(f)
}

// syncDirBestEffort fsyncs dir after a rename or remove so the directory
// entry reaches stable storage before the call is acknowledged. Skipped when
// the fsync knob is off. A failure is NON-fatal — some filesystems (FUSE,
// some network mounts) refuse directory fsyncs with EINVAL; refusing the
// write after it already landed would be worse than logging the degraded
// durability.
//
// op names the write PRIMITIVE ("write", "rename_file", "delete_file"), not
// always the calling tool: safeWrite is shared by fifteen tools, and threading a
// tool name through every one of them buys only a log label — the warning
// already carries the path, and the failure is a property of the filesystem, not
// of the tool that happened to hit it.
func syncDirBestEffort(op, dir string) {
	if !fsync.Enabled() {
		return
	}
	if err := syncDirHook(dir); err != nil {
		slog.Warn(op+": directory fsync failed — write acknowledged but not crash-durable", "dir", dir, "err", err)
	}
}

// mkdirAllSynced creates dir like os.MkdirAll, then fsyncs the PARENT of every
// directory it had to create. Without this the fsync-before-ack contract has a
// hole one level up: writing a/b/c.txt into a fresh tree fsyncs c.txt and a/b,
// but the entry for b inside a is only in the page cache, so a crash can lose
// the whole new subtree even though the write was acknowledged. The syncs are
// best-effort, like every other directory fsync.
func mkdirAllSynced(op, dir string) error {
	created := missingAncestors(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// Shallowest first: each new directory's entry lives in the one above it.
	for _, d := range created {
		syncDirBestEffort(op, filepath.Dir(d))
	}
	return nil
}

// missingAncestors returns dir and each ancestor that does not exist yet,
// shallowest first. Empty when dir already exists — the common case, which
// therefore costs one Stat and no syncs.
func missingAncestors(dir string) []string {
	var missing []string
	for d := dir; ; {
		if _, err := os.Stat(d); err == nil {
			break
		}
		missing = append(missing, d)
		parent := filepath.Dir(d)
		if parent == d { // reached the filesystem root
			break
		}
		d = parent
	}
	slices.Reverse(missing)
	return missing
}
