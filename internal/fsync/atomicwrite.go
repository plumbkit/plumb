package fsync

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
)

// defaultTempPattern stages the write as a hidden sibling of the target. The
// dot prefix keeps the in-flight file out of casual directory listings, and the
// .tmp suffix keeps it out of extension-filtered scans.
const defaultTempPattern = ".plumb-*.tmp"

// defaultMode is the permission for a file that does not yet exist. Deliberately
// private (0600) rather than 0644: these are a daemon's own state files, and a
// caller that genuinely wants a world-readable file says so via Options.Mode.
const defaultMode os.FileMode = 0o600

// Options tunes AtomicWrite. The zero value is valid and correct for a
// plumb-owned state file.
type Options struct {
	// TempPattern is the os.CreateTemp pattern for the staging file, created in
	// the same directory as the target so the rename cannot cross a filesystem.
	// Empty means defaultTempPattern.
	//
	// This is a knob because two callers scan the directories they write into
	// and the pattern is load-bearing: internal/session lists sessions by a
	// ".json" suffix, so its staging files must keep a trailing ".tmp" to stay
	// invisible to that scan.
	TempPattern string

	// Mode is the permission for a file that does not yet exist. Zero means
	// defaultMode. When the target already exists its current permissions are
	// preserved and Mode is ignored — see AtomicWrite.
	Mode os.FileMode

	// Label names the calling subsystem in the best-effort directory-fsync
	// warning ("memory", "session", "txlog"). Empty is allowed; the warning
	// still carries the path.
	Label string
}

// AtomicWrite writes data to path so that a concurrent reader sees either the
// old contents or the new ones, never a partial file, and so that an
// acknowledged write survives a hard crash.
//
// This is the executable form of the contract in this package's doc comment:
// stage into a temp file in the target's own directory, fsync it, rename over
// the target, then fsync the parent directory. The staging file is removed on
// every failure path.
//
// Permissions: if path already exists, its current mode is preserved and
// opts.Mode is ignored; otherwise opts.Mode (or 0600) applies. os.CreateTemp
// always creates 0600, so without the explicit chmod every rewrite of an
// existing 0644 file would silently downgrade it — which is exactly what the
// `plumb setup` writers were doing to third-party config files.
//
// It does not create parent directories: a caller that needs them should say so
// explicitly, with the permissions it wants.
func AtomicWrite(path string, data []byte, opts Options) error {
	return AtomicWriteFunc(path, opts, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
}

// AtomicWriteFunc is AtomicWrite for a caller that streams its content rather
// than marshalling it to a byte slice first — an encoder writing straight into
// the staging file, say. write is called exactly once; returning an error from
// it aborts the write and leaves the target untouched.
func AtomicWriteFunc(path string, opts Options, write func(io.Writer) error) error {
	dir := filepath.Dir(path)

	pattern := opts.TempPattern
	if pattern == "" {
		pattern = defaultTempPattern
	}
	mode := opts.Mode
	if mode == 0 {
		mode = defaultMode
	}
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}

	tmp, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpName := tmp.Name()
	// Cleans up every failure path below; a no-op once the rename succeeds,
	// because by then tmpName no longer exists.
	defer func() { _ = os.Remove(tmpName) }()

	if err := write(tmp); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing temp file: %w", err)
	}
	if err := SyncFile(tmp); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("syncing temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp file: %w", err)
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return fmt.Errorf("setting temp file permissions: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("renaming temp file into place: %w", err)
	}
	// Best-effort: some filesystems (FUSE, notably) return EINVAL for a
	// directory fsync. Without it a crash can resurrect the pre-rename
	// directory entry even though the file data above was synced.
	if err := SyncDir(dir); err != nil {
		slog.Warn("directory fsync failed after atomic write",
			"subsystem", opts.Label, "path", path, "err", err)
	}
	return nil
}
