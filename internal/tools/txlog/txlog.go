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
	"errors"
	"fmt"
	"io/fs"
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
		restoreOp(l.dir, op, op.Perm, target)
	}
	if err := os.RemoveAll(l.dir); err != nil {
		slog.Error("txlog: failed to remove log dir after rollback", "dir", l.dir, "err", err)
	}
}

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
// The manifests Scan replays are UNTRUSTED. <workspace>/.plumb/tx-log/ is an
// ordinary directory inside the workspace, so a cloned repository ships one and
// the daemon replays it on attach, as the user, with no prompt. Every path is
// therefore re-checked against guard; see PathGuard.
//
// Scan is a no-op when workspace is empty or no tx-log directory exists.
func Scan(workspace string, liveCutoff time.Time, guard PathGuard) {
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
		replayOrphan(dir, guard)
		if err := os.RemoveAll(dir); err != nil {
			slog.Error("txlog: failed to remove orphaned log after rollback", "dir", dir, "err", err)
		}
	}
}

// logDirInsideWorkspace reports whether <workspace>/.plumb/tx-log really lives
// inside the workspace once symlinks are resolved.
//
// Without this, Scan is a directory-DELETION primitive, and one that needs no
// manifest at all. `.plumb/tx-log` — or `.plumb` itself — is an ordinary path
// inside the workspace, and git stores a symlink natively, so a repository can
// commit `.plumb/tx-log -> /Users/you/Documents`. os.ReadDir follows it, every
// subdirectory of the target then looks like an orphaned transaction, and the
// RemoveAll at the end of the loop deletes each one. Attaching to a cloned
// repository was enough; nothing had to be replayed.
//
// Checked on the RESOLVED paths rather than by Lstat-ing the final component,
// because an intermediate component is equally attacker-supplied: with
// `.plumb -> /` the final `tx-log` component is not itself a link and an Lstat
// check would pass it.
//
// The predicate is IDENTITY, not containment, and the difference is the whole
// fix. "Inside the workspace" admits the workspace root itself and every
// directory under it, so `.plumb/tx-log -> ..` resolves to the root, and Scan
// then treats each top-level directory as an orphaned transaction and
// RemoveAlls it — `.git` included, taking local branches, stashes and the reflog
// with it. `-> ../.git` does the same to the object store, `-> .` to
// `.plumb/memories`. Each is a payload one character shorter than the `../..`
// the first version's own regression test used.
//
// There is exactly one directory Scan may ever walk, and its name is known, so
// say so: anything that does not resolve to <workspace>/.plumb/tx-log is
// refused, whether it points out of the workspace or back into it.
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
//
// A refused op is REPORTED, not silently skipped. A replay that quietly dropped
// entries would leave a half-restored transaction indistinguishable from a clean
// one, and would let a hostile manifest steer an operator reading the log.
func replayOrphan(dir string, guard PathGuard) {
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
		// The guard is necessary but not sufficient: it rules on the path as
		// WRITTEN, and os.WriteFile follows a symlink to somewhere else. Resolve the
		// path to the file the write will actually reach, and put THAT through the
		// guard too.
		//
		// Resolving rather than refusing a symlink outright is deliberate. The
		// earlier version refused, on the stated premise that the write primitive
		// "replaces a link rather than following it" — the opposite of what safeWrite
		// does ("resolve to the real target so we write through the link", and only a
		// DANGLING link gets replaced). On that false premise the blanket refusal
		// looked free; in fact it silently dropped crash recovery for any legitimately
		// symlinked source file, which is ordinary in a monorepo.
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
		// The snapshot is read with os.ReadFile, which follows symlinks too, and its
		// content is then written to the admitted path. A repository shipping
		// `0-before -> ~/.ssh/id_ed25519` would otherwise copy that file into the
		// workspace, where the agent reads it or a later commit carries it away.
		// Refused outright rather than resolved: a snapshot is a file THIS daemon
		// wrote, so a link in its place is never legitimate.
		if err := refuseSymlink(snapshotPath(dir, op.N)); err != nil {
			slog.Error("txlog: replay REFUSED — snapshot file is a symlink",
				"snap", snapshotPath(dir, op.N), "manifest", manifestPath, "err", err)
			continue
		}
		// A HARDLINK is not a symlink: Lstat reports a perfectly ordinary file, so
		// the check above waves it through while the inode is somewhere else
		// entirely. `0-before` hardlinked to ~/.ssh/id_ed25519 would have its
		// content read and written to an admitted in-workspace path — the same
		// exfiltration the symlink check exists to stop, one indirection lower.
		// A snapshot is a file THIS daemon created moments before crashing, so it
		// has exactly one link; more than one means someone else made it.
		if err := refuseMultiplyLinked(snapshotPath(dir, op.N)); err != nil {
			slog.Error("txlog: replay REFUSED — snapshot file has more than one hard link",
				"snap", snapshotPath(dir, op.N), "manifest", manifestPath, "err", err)
			continue
		}
		// Write to the RESOLVED target, not to op.Path: the guard has just ruled on
		// target, and handing the syscall a different string is exactly the bug this
		// function exists to close.
		restoreOp(dir, op, replayPerm, target)
	}
}

// replayTarget returns the file a write to path would actually modify, so the
// caller can re-check THAT against the boundary guard.
//
// EVERY component is resolved, not just the last one. An earlier version only
// called EvalSymlinks when the final component was itself a link, so a link in
// an ANCESTOR left the returned target equal to the input and the caller's
// re-check never ran — the exact hole the re-check exists to close, reachable
// with `sub -> /outside` and an ordinary file name below it.
//
// A DANGLING link is refused rather than followed or replaced: there is no
// resolved name to re-check, and a policy canonicalises a missing path through
// its nearest existing ancestor, so <workspace>/payload -> ~/.zshenv would be
// admitted as <workspace>/payload.
func replayTarget(path string) (string, error) {
	if _, err := os.Lstat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// The file is absent — the transaction may have deleted it, and the
			// replay recreates it. Its ancestors still need resolving.
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
// A missing path is fine — the replay may legitimately recreate a file the
// transaction had deleted.
//
// Residual, stated rather than papered over: this is a check-then-write, so a
// process racing the daemon could replace the path with a link between the two.
// Closing that would need O_NOFOLLOW, which is not portable across the platforms
// plumb builds for. The gap it does close is the one that matters here — a
// hostile repository is a set of files on disk, not a running process.
func refuseSymlink(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		// Any OTHER Lstat failure — a permission denial on the parent, an I/O
		// error — leaves us unable to say whether this is a link, and the previous
		// version returned nil for all of them while its comment claimed it was
		// only forgiving a missing file. "I could not check" must not read as
		// "I checked and it is fine".
		return fmt.Errorf("cannot determine whether path is a symlink: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("path is a symlink")
	}
	return nil
}

// snapshotPath is where Record stored the pre-write content of op n. op.N is an
// int, so strconv.Itoa can only produce digits and a leading minus — never a
// separator — and the join therefore cannot leave dir.
func snapshotPath(dir string, n int) string {
	return filepath.Join(dir, strconv.Itoa(n)+"-before")
}

// admitReplayPath decides whether one path out of an untrusted manifest may be
// written. A nil guard FAILS CLOSED: a caller with no policy to consult must
// refuse the replay rather than perform it unchecked.
func admitReplayPath(path string, guard PathGuard) error {
	if path == "" {
		return errors.New("manifest op has no path")
	}
	if !filepath.IsAbs(path) {
		// os.WriteFile would anchor a relative path to the daemon's working
		// directory — a singleton process whose cwd belongs to whichever client
		// happened to spawn it, i.e. an unrelated project.
		return errors.New("manifest op path is relative")
	}
	if filepath.Clean(path) != path {
		// A boundary policy canonicalises with filepath.Abs, which cancels "sub/.."
		// LEXICALLY; the kernel follows `sub` first and applies ".." to wherever
		// that landed. With `sub` a committed symlink the two name different files,
		// so the guard would admit an in-workspace path while the write escaped.
		// A manifest is machine-written, so demanding canonical form costs nothing.
		return errors.New("manifest op path is not in canonical form")
	}
	if guard == nil {
		return errors.New("no boundary guard supplied")
	}
	return guard(path)
}

// restoreOp restores one op's snapshot to target. perm is used only when the
// target does not already exist — see replayPerm.
//
// target is passed separately from op.Path because the two differ on the
// untrusted path: replayOrphan resolves op.Path and re-checks the resolved file,
// then writes to THAT. Rollback, whose ops this process recorded in memory,
// passes op.Path itself.
func restoreOp(dir string, op opMeta, perm os.FileMode, target string) {
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
	// Written through the same stage-and-rename primitive as every other durable
	// write in the tree, and that is a security property here, not just a
	// durability one.
	//
	// os.WriteFile OPENS THE EXISTING INODE AND TRUNCATES IT. A hardlink at an
	// admitted in-workspace name therefore made the guard's verdict true of the
	// NAME and false of the FILE: the content landed in whatever the inode was
	// also linked from, outside every allowed root. A rename replaces the
	// directory entry instead, so another link to the old inode is untouched.
	//
	// This is also what safeWrite has always done, which matters because the
	// previous version of this function claimed to "mirror the write primitive"
	// while using a different one. It mirrors it now.
	//
	// Not merely hostile input: pnpm hardlinks node_modules entries into a global
	// store, and cp -l and build caches do the same, so an in-place write during
	// recovery would corrupt every other project sharing that store.
	if err := fsync.AtomicWrite(target, content, fsync.Options{Mode: perm, Label: "txlog"}); err != nil {
		slog.Error("txlog: rollback: cannot restore file", "path", target, "err", err)
		return
	}
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
