// Package txlog implements a durable write-ahead log for transaction_apply.
//
// When transaction_apply enters phase 2 (the actual writes), it calls Begin to
// create a per-transaction snapshot directory under <workspace>/.plumb/tx-log/.
// Before each file write it calls Record to save the pre-write content.
// On success it calls Commit to remove the directory.
// On failure (partial write) it calls Rollback to restore snapshotted files
// and remove the directory.
//
// If the daemon crashes between writes, the snapshot directory is left behind.
// The next time the workspace attaches, Scan finds orphaned directories and
// rolls them back automatically.
//
// Concurrency: Log is not safe for concurrent use — transaction_apply holds
// per-path locks for the duration of phase 2, so no concurrent access occurs.
// Scan is safe to call concurrently from multiple goroutines because it
// operates on distinct txID sub-directories.
package txlog

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"time"
)

const (
	txLogSubDir = ".plumb/tx-log"
	// maxSnapSize is the per-file snapshot size cap. Files larger than this
	// are recorded in the manifest but their content is not snapshotted — a
	// rollback cannot restore them and will log a warning. 10 MiB balances
	// durability against disk amplification for large source files.
	maxSnapSize = 10 << 20 // 10 MiB
)

var txCounter atomic.Int64

// newID returns a unique transaction ID combining a nanosecond timestamp with
// a monotone counter. The timestamp component makes IDs from distinct daemon
// runs distinguishable; the counter guarantees uniqueness within a run.
func newID() string {
	n := txCounter.Add(1)
	return fmt.Sprintf("%d-%d", time.Now().UnixNano(), n)
}

// opMeta describes one operation recorded in the manifest.
type opMeta struct {
	N           int         `json:"n"`
	Path        string      `json:"path"`
	Perm        os.FileMode `json:"perm"`
	Snapshotted bool        `json:"snapshotted"`
}

type txManifest struct {
	TxID      string    `json:"tx_id"`
	StartedAt time.Time `json:"started_at"`
	Workspace string    `json:"workspace"`
	Ops       []opMeta  `json:"ops"`
}

// Log represents one in-flight transaction's write-ahead log.
// A zero-value Log is a no-op (returned when the workspace has no .plumb/).
type Log struct {
	dir      string
	manifest txManifest
	n        int
}

// Begin creates the tx-log directory for a new transaction and writes an
// initial (empty) manifest. Returns a no-op Log if workspace is empty or
// <workspace>/.plumb/ does not exist — the transaction proceeds without
// durability rather than failing.
func Begin(workspace string) (*Log, error) {
	if workspace == "" {
		return &Log{}, nil
	}
	plumbDir := filepath.Join(workspace, ".plumb")
	if _, err := os.Stat(plumbDir); err != nil {
		return &Log{}, nil // no .plumb/ marker — no-op
	}
	txID := newID()
	dir := filepath.Join(plumbDir, "tx-log", txID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("txlog: creating log dir: %w", err)
	}
	l := &Log{
		dir: dir,
		manifest: txManifest{
			TxID:      txID,
			StartedAt: time.Now(),
			Workspace: workspace,
		},
	}
	if err := l.writeManifest(); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	return l, nil
}

// Record saves the pre-write content of path as snapshot <n>-before and
// updates the manifest. Must be called before each safeWrite in phase 2.
//
// Files larger than maxSnapSize are listed in the manifest with
// snapshotted=false; their content is not saved. Rollback will skip them and
// log a warning. Record errors are non-fatal — the transaction continues
// without durability for that file.
func (l *Log) Record(path string, content []byte, perm os.FileMode) error {
	if l.dir == "" {
		return nil
	}
	n := l.n
	l.n++
	meta := opMeta{N: n, Path: path, Perm: perm}
	if len(content) <= maxSnapSize {
		snapPath := filepath.Join(l.dir, strconv.Itoa(n)+"-before")
		if err := os.WriteFile(snapPath, content, 0o600); err != nil {
			return fmt.Errorf("txlog: writing snapshot for %s: %w", path, err)
		}
		meta.Snapshotted = true
	} else {
		slog.Warn("txlog: file exceeds snapshot size cap — cannot be rolled back",
			"path", path, "size", len(content), "cap", maxSnapSize)
	}
	l.manifest.Ops = append(l.manifest.Ops, meta)
	return l.writeManifest()
}

// Commit removes the tx-log directory. Call this after all writes succeed.
// A Commit failure is logged but does not affect the committed data.
func (l *Log) Commit() {
	if l.dir == "" {
		return
	}
	if err := os.RemoveAll(l.dir); err != nil {
		slog.Error("txlog: commit cleanup failed — orphaned log may trigger phantom rollback on restart",
			"dir", l.dir, "err", err)
	}
}

// Rollback restores each snapshotted file to its pre-transaction content and
// removes the tx-log directory. Best-effort: failures are logged and rollback
// continues with remaining files.
func (l *Log) Rollback() {
	if l.dir == "" {
		return
	}
	rollbackDir(l.dir)
	if err := os.RemoveAll(l.dir); err != nil {
		slog.Error("txlog: failed to remove log dir after rollback", "dir", l.dir, "err", err)
	}
}

// Scan finds orphaned .plumb/tx-log/* directories left by a daemon that crashed
// mid-transaction and rolls each one back. A directory whose manifest StartedAt
// is at or after liveCutoff belongs to the CURRENT daemon run — a possibly
// in-flight transaction owned by some connection — and is left untouched; only
// the owning transaction may roll it back. Pass the daemon's start time as
// liveCutoff so a second connection attaching to a workspace can never roll back
// a live transaction another connection is running on it (which would silently
// revert that transaction's already-written files).
//
// A directory whose StartedAt cannot be read (a crash before the first manifest
// write, or a corrupt previous-run orphan) is treated as a recoverable orphan —
// a live transaction always has a valid manifest by the time Begin returns.
//
// Scan is a no-op when workspace is empty or no tx-log directory exists.
func Scan(workspace string, liveCutoff time.Time) {
	if workspace == "" {
		return
	}
	logDir := filepath.Join(workspace, txLogSubDir)
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
		rollbackDir(dir)
		if err := os.RemoveAll(dir); err != nil {
			slog.Error("txlog: failed to remove orphaned log after rollback", "dir", dir, "err", err)
		}
	}
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

// rollbackDir reads the manifest from dir and restores each snapshotted file.
func rollbackDir(dir string) {
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
		if !op.Snapshotted {
			slog.Warn("txlog: rollback: no snapshot for large file — cannot restore",
				"path", op.Path)
			continue
		}
		snapPath := filepath.Join(dir, strconv.Itoa(op.N)+"-before")
		content, err := os.ReadFile(snapPath)
		if err != nil {
			slog.Error("txlog: rollback: cannot read snapshot", "snap", snapPath, "err", err)
			continue
		}
		if err := os.WriteFile(op.Path, content, op.Perm); err != nil { //nolint:gosec // G703: op.Path is a workspace path validated by the transaction machinery before being stored in the manifest
			slog.Error("txlog: rollback: cannot restore file", "path", op.Path, "err", err)
			continue
		}
		slog.Info("txlog: rollback: restored", "path", op.Path)
	}
}

func (l *Log) writeManifest() error {
	data, err := json.MarshalIndent(l.manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("txlog: marshalling manifest: %w", err)
	}
	return atomicWriteManifest(filepath.Join(l.dir, "manifest.json"), data)
}

// atomicWriteManifest writes the manifest via a uniquely-named temp file in the
// same directory, fsync'd then renamed into place. The manifest is rewritten on
// every Record of a live multi-file transaction, and a cross-connection Scan can
// os.ReadFile it concurrently for orphan recovery: a non-atomic truncate-in-place
// write would let Scan observe a half-written manifest, fail to unmarshal it, miss
// the StartedAt-cutoff guard, and roll back the *live* transaction's already-
// written files (silent corruption). The POSIX-atomic rename guarantees a reader
// always sees a complete manifest — the old one or the new one, never a torn one.
func atomicWriteManifest(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".manifest-*.tmp")
	if err != nil {
		return fmt.Errorf("txlog: creating temp manifest: %w", err)
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }() // no-op once the rename below succeeds
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("txlog: writing temp manifest: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("txlog: syncing temp manifest: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("txlog: closing temp manifest: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("txlog: renaming manifest into place: %w", err)
	}
	return nil
}
