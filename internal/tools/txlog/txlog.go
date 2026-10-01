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

	"github.com/plumbkit/plumb/internal/fsync"
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
	CallID    string    `json:"call_id,omitempty"`
	Ops       []opMeta  `json:"ops"`
}

// Restored records the pre- and post-restoration content of one file rolled
// back by crash recovery, along with the call ID of the manifest that owned it.
type Restored struct {
	Path          string
	Before        []byte
	After         []byte
	BeforeExisted bool
	CallID        string
}

// RestoreSink receives crash recovery restores as they happen.
type RestoreSink func(Restored)

func (r RestoreSink) recordHistory(x Restored) {
	if r != nil {
		r(x)
	}
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
func Begin(workspace, callID string) (*Log, error) {
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
			CallID:    callID,
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
//
// This replays the IN-MEMORY manifest, never the copy on disk, and that is the
// whole reason Scan is a separate function rather than a second caller of one
// shared replay. The ops here were recorded by this process during this
// transaction, and every path in them passed the write tools' boundary guard
// before the write it is undoing, so they need no re-check. Reading the file
// back would make this function's safety depend on the file's provenance — and
// it is exactly that conflation which let a cloned repository's manifest be
// replayed with no boundary check at all.
func (l *Log) Rollback() {
	if l.dir == "" {
		return
	}
	for _, op := range l.manifest.Ops {
		// Resolve here too. The forward write went through safeWrite, which follows
		// a symlink to its target, so the file this transaction actually changed is
		// the resolved one; restoring the link's own name would replace the link
		// with a regular file instead of undoing the write. Resolution failure
		// falls back to the recorded path rather than abandoning the rollback.
		target := op.Path
		if resolved, err := replayTarget(op.Path); err == nil {
			target = resolved
		}
		// nil sink: transaction_apply's rollback already recorded this restoration
		// (tx_rollback); recording the txlog replay of the same bytes would duplicate it.
		restoreOp(l.dir, op, op.Perm, target, nil, "")
	}
	if err := os.RemoveAll(l.dir); err != nil {
		slog.Error("txlog: failed to remove log dir after rollback", "dir", l.dir, "err", err)
	}
}

// snapshotPath is where Record stored the pre-write content of op n. op.N is an
// int, so strconv.Itoa can only produce digits and a leading minus — never a
// separator — and the join therefore cannot leave dir.
func snapshotPath(dir string, n int) string {
	return filepath.Join(dir, strconv.Itoa(n)+"-before")
}

// restoreOp restores one op's snapshot to target. perm is used only when the
// target does not already exist — see replayPerm.
//
// target is passed separately from op.Path because the two differ on the
// untrusted path: replayOrphan resolves op.Path and re-checks the resolved file,
// then writes to THAT. Rollback, whose ops this process recorded in memory,
// passes op.Path itself.
func restoreOp(dir string, op opMeta, perm os.FileMode, target string, sink RestoreSink, callID string) {
	if !op.Snapshotted {
		slog.Warn("txlog: rollback: no snapshot for large file — cannot restore",
			"path", op.Path)
		return
	}
	snapPath := snapshotPath(dir, op.N)
	content, err := os.ReadFile(snapPath)
	if err != nil {
		slog.Error("txlog: rollback: cannot read snapshot", "snap", snapPath, "err", err)
		return
	}
	var (
		cur    []byte
		curErr error
	)
	if info, statErr := os.Stat(target); statErr == nil && info.Size() <= maxSnapSize {
		cur, curErr = os.ReadFile(target)
	} else if statErr != nil {
		curErr = statErr
	}
	if err := fsync.AtomicWrite(target, content, fsync.Options{Mode: perm, Label: "txlog"}); err != nil {
		slog.Error("txlog: rollback: cannot restore file", "path", target, "err", err)
		return
	}
	sink.recordHistory(Restored{
		Path:          target,
		Before:        cur,
		BeforeExisted: curErr == nil,
		After:         content,
		CallID:        callID,
	})
	slog.Info("txlog: rollback: restored", "path", target)
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
	if err := fsync.AtomicWrite(path, data, fsync.Options{
		TempPattern: ".manifest-*.tmp",
		Label:       "txlog",
	}); err != nil {
		return fmt.Errorf("txlog: writing manifest: %w", err)
	}
	return nil
}
