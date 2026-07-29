// Package fsync provides the crash-durability primitives behind plumb's
// "fsync-before-ack" write contract: a write tool call must not report success
// until the data is on stable storage. Temp-file-then-rename alone is not
// enough — the rename only updates a directory entry, and that entry can sit
// in the page cache across a hard crash or power cut, so a "successful" write
// vanishes on reboot (this happened: an edit_file acked, the machine
// hard-rebooted seconds later, the edit was gone).
//
// The contract, per write:
//
//  1. SyncFile the staged temp file BEFORE the rename (fatal on error).
//  2. SyncDir the parent directory AFTER the rename (non-fatal on error —
//     some filesystems, e.g. FUSE, return EINVAL for a directory fsync).
//
// Both steps are gated on the [edits] fsync knob (PLUMB_FSYNC env override),
// installed via SetEnabledFunc. Disabling the knob restores the pre-fix
// behaviour — no fsyncs at all — for benchmarks and exotic filesystems.
package fsync

import "os"

// enabledFn returns whether fsync-before-ack is active. The daemon installs a
// closure reading the resolved [edits] fsync config; nil (tests, headless
// tools, one-shot CLI commands) means the safe default: on.
var enabledFn func() bool

// SetEnabledFunc installs the function consulted by Enabled. Called once from
// the daemon's write-deps assembly. Passing nil restores the default (on).
func SetEnabledFunc(fn func() bool) { enabledFn = fn }

// Enabled reports whether fsync-before-ack is active. Defaults to true when
// no function has been installed.
func Enabled() bool {
	if enabledFn == nil {
		return true
	}
	return enabledFn()
}

// SyncFile fsyncs f (flushing its data and metadata to stable storage) when
// the knob is on; it is a no-op returning nil when the knob is off.
func SyncFile(f *os.File) error {
	if !Enabled() {
		return nil
	}
	return f.Sync()
}

// SyncDir fsyncs the directory at path so the directory entries it holds
// (the product of a recent rename, create, or remove) reach stable storage.
// No-op returning nil when the knob is off.
func SyncDir(path string) error {
	if !Enabled() {
		return nil
	}
	return syncDir(path)
}
