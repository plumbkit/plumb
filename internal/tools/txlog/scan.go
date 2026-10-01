package txlog

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// PathGuard reports whether a path named in an on-disk manifest may be written
// during replay. It is the caller's boundary policy, injected rather than
// imported: txlog sits below internal/tools, so it cannot reach the PathPolicy
// that knows a session's allowed roots.
//
// Scan REQUIRES one and fails closed without it. Note that a plain
// workspace-containment test would be the WRONG check here: transaction_apply
// legitimately writes to configured extra roots and --allow-dir grants, so crash
// recovery must be able to restore those too. Only the session's own policy
// knows the difference between "outside the workspace" and "outside every root
// this session may write".
type PathGuard func(path string) error

// Scan finds orphaned .plumb/tx-log/* directories left by a daemon that crashed
// mid-transaction and rolls each one back. Delegates to ScanRecording with a nil sink.
func Scan(workspace string, liveCutoff time.Time, guard PathGuard) {
	ScanRecording(workspace, liveCutoff, guard, nil)
}

// ScanRecording finds orphaned .plumb/tx-log/* directories left by a daemon that crashed
// mid-transaction and rolls each one back, reporting restores through sink.
func ScanRecording(workspace string, liveCutoff time.Time, guard PathGuard, sink RestoreSink) {
	if workspace == "" {
		return
	}
	logDir := filepath.Join(workspace, txLogSubDir)
	if _, err := os.Lstat(logDir); err != nil {
		// No tx-log at all is the ordinary case for a workspace that has never run
		// a transaction. Returning before the containment verdict keeps the security
		// error below meaning "something is wrong", rather than firing on every
		// attach of every clean workspace — alarm fatigue on the one message that
		// signals a live attack.
		return
	}
	if !logDirIsTheRealTxLogDir(workspace, logDir) {
		slog.Error("txlog: refusing to scan — the tx-log directory does not resolve to <workspace>/"+txLogSubDir,
			"dir", logDir, "workspace", workspace)
		return
	}
	entries, err := os.ReadDir(logDir)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("txlog: scan failed", "dir", logDir, "err", err)
		}
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(logDir, e.Name())
		if startedAt, ok := manifestStartedAt(dir); ok && !startedAt.Before(liveCutoff) {
			// Created by the current daemon run: a live or just-committed
			// transaction owns it. Never roll it back from here.
			continue
		}
		slog.Warn("txlog: orphaned transaction log found — rolling back", "txid", e.Name(), "workspace", workspace)
		replayOrphan(dir, guard, sink)
		if err := os.RemoveAll(dir); err != nil {
			slog.Error("txlog: failed to remove orphaned log after rollback", "dir", dir, "err", err)
		}
	}
}

// logDirIsTheRealTxLogDir reports whether <workspace>/.plumb/tx-log really is
// that directory once symlinks are resolved — an identity check, not a
// containment one. Containment is not enough here: the workspace root admits
// itself, so a tx-log resolving to the workspace would pass a containment test
// and still let Scan delete .git.
func logDirIsTheRealTxLogDir(workspace, logDir string) bool {
	root, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return false
	}
	resolved, err := filepath.EvalSymlinks(logDir)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil {
		return false
	}
	return rel == filepath.FromSlash(txLogSubDir)
}

// manifestStartedAt reads the StartedAt timestamp from a tx-log directory's
// manifest. ok is false when the manifest is missing, unparseable, or carries no
// timestamp — callers then treat the directory as a recoverable orphan.
func manifestStartedAt(dir string) (time.Time, bool) {
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return time.Time{}, false
	}
	var m txManifest
	if err := json.Unmarshal(data, &m); err != nil || m.StartedAt.IsZero() {
		return time.Time{}, false
	}
	return m.StartedAt, true
}

// replayPerm is the mode an untrusted replay creates a file with. os.WriteFile
// applies perm ONLY when it creates the file, so a manifest's own Perm can
// matter in exactly one case — the replay creating a file that does not already
// exist — which is precisely the case worth attacking (perm 0o777 plus a shell
// script). An existing file keeps its own mode regardless, so declining the
// manifest's value costs a legitimate recovery nothing.
const replayPerm os.FileMode = 0o600

// replayOrphan restores the snapshotted files named by the manifest left in dir,
// admitting only paths guard allows.
func replayOrphan(dir string, guard PathGuard, sink RestoreSink) {
	manifestPath := filepath.Join(dir, "manifest.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		slog.Error("txlog: cannot read manifest", "path", manifestPath, "err", err)
		return
	}
	var m txManifest
	if err := json.Unmarshal(data, &m); err != nil {
		slog.Error("txlog: cannot parse manifest", "path", manifestPath, "err", err)
		return
	}
	for _, op := range m.Ops {
		if err := admitReplayPath(op.Path, guard); err != nil {
			slog.Error("txlog: replay REFUSED — manifest names a path this session may not write",
				"path", op.Path, "manifest", manifestPath, "err", err)
			continue
		}
		target, err := replayTarget(op.Path)
		if err != nil {
			slog.Error("txlog: replay REFUSED — cannot determine which file this op would write",
				"path", op.Path, "manifest", manifestPath, "err", err)
			continue
		}
		if target != op.Path {
			if err := admitReplayPath(target, guard); err != nil {
				slog.Error("txlog: replay REFUSED — manifest path is a symlink resolving outside the session's roots",
					"path", op.Path, "target", target, "manifest", manifestPath, "err", err)
				continue
			}
		}
		if err := refuseSymlink(snapshotPath(dir, op.N)); err != nil {
			slog.Error("txlog: replay REFUSED — snapshot file is a symlink",
				"snap", snapshotPath(dir, op.N), "manifest", manifestPath, "err", err)
			continue
		}
		if err := refuseMultiplyLinked(snapshotPath(dir, op.N)); err != nil {
			slog.Error("txlog: replay REFUSED — snapshot file has more than one hard link",
				"snap", snapshotPath(dir, op.N), "manifest", manifestPath, "err", err)
			continue
		}
		restoreOp(dir, op, replayPerm, target, sink, m.CallID)
	}
}

// replayTarget returns the file a write to path would actually modify, so the
// caller can re-check THAT against the boundary guard.
func replayTarget(path string) (string, error) {
	if _, err := os.Lstat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			parent, perr := filepath.EvalSymlinks(filepath.Dir(path))
			if perr != nil {
				return "", fmt.Errorf("cannot resolve the parent directory: %w", perr)
			}
			return filepath.Join(parent, filepath.Base(path)), nil
		}
		return "", fmt.Errorf("cannot stat path: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("path is a dangling or unresolvable symlink: %w", err)
	}
	return resolved, nil
}

// refuseSymlink reports an error when path exists and is a symbolic link.
func refuseSymlink(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("cannot determine whether path is a symlink: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("path is a symlink")
	}
	return nil
}

// admitReplayPath decides whether one path out of an untrusted manifest may be
// written. A nil guard FAILS CLOSED: a caller with no policy to consult must
// refuse the replay rather than perform it unchecked.
func admitReplayPath(path string, guard PathGuard) error {
	if path == "" {
		return errors.New("manifest op has no path")
	}
	if !filepath.IsAbs(path) {
		return errors.New("manifest op path is relative")
	}
	if filepath.Clean(path) != path {
		return errors.New("manifest op path is not in canonical form")
	}
	if guard == nil {
		return errors.New("no boundary guard supplied")
	}
	return guard(path)
}
