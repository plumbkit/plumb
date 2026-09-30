package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/paths"
)

// Issue #528: after a write, recordWritten stat'ed and re-hashed the PATH to
// refresh the session's read record. An outside writer landing between plumb's
// write and that hash had its content recorded as the version this session
// wrote, and the session's next unguarded write went straight over it. The
// per-path lock only excludes plumb's own writers, so the window is real.
//
// Each test below lands an outside write inside that window deterministically,
// through a post-write hook that runs before the bookkeeping (the adapter hook
// or the language-server notification), then asserts the next write still sees
// the change. Each also has a positive control: without the outside writer the
// session's own consecutive writes are never flagged.

// outsiderWrite overwrites path the way a process outside plumb would. With
// keepMtime it also restores the file's mtime, as cp -p, rsync -t and a
// same-tick write do, so only the recorded SHA can tell the versions apart.
func outsiderWrite(t *testing.T, path, content string, keepMtime bool) {
	t.Helper()
	before, err := os.Stat(path)
	if err != nil {
		t.Errorf("outsider: stat %s: %v", path, err)
		return
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Errorf("outsider: write %s: %v", path, err)
		return
	}
	when := before.ModTime()
	if !keepMtime {
		when = when.Add(2 * time.Second)
	}
	if err := os.Chtimes(path, when, when); err != nil {
		t.Errorf("outsider: chtimes %s: %v", path, err)
	}
}

// onceHook returns a hook that runs fn the first time it is called for path.
func onceHook(path string, fn func()) func(string) {
	var once sync.Once
	return func(p string) {
		if p == path {
			once.Do(fn)
		}
	}
}

// notifyHookClient is an lsp.Client whose didChangeWatchedFiles runs a hook per
// changed path. The embedded nil interface panics on any other method, which is
// the point: the write path under test touches nothing else.
type notifyHookClient struct {
	lsp.Client
	hook func(path string)
}

func (c *notifyHookClient) DidChangeWatchedFiles(_ context.Context, p protocol.DidChangeWatchedFilesParams) error {
	for _, ch := range p.Changes {
		if ch.Type != protocol.FileDeleted {
			c.hook(paths.URIToPath(ch.URI))
		}
	}
	return nil
}

// assertNextWriteSeesOutsider runs an unguarded write_file over path and
// asserts it is refused and the outsider's content survives.
func assertNextWriteSeesOutsider(t *testing.T, deps WriteDeps, path, outsider string) {
	t.Helper()
	deps.PostWriteNotifyFn, deps.Client = nil, nil
	_, err := NewWriteFile(deps).Execute(context.Background(), mustJSON(map[string]any{"file_path": path, "content": "clobber\n"}))
	if err == nil || !strings.Contains(err.Error(), "changed on disk since you read it") {
		t.Fatalf("the next write must see the outside change made after plumb's write, got: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != outsider {
		t.Fatalf("outside change was overwritten: %q", got)
	}
}

func raceDeps() WriteDeps {
	return WriteDeps{Reads: NewReadTracker(), Writes: NewWriteTracker()}
}

var outsiderModes = []struct {
	name      string
	keepMtime bool
}{
	{"mtime advances", false},
	{"mtime preserved", true},
}

func TestWriteFile_OutsiderWriteAfterOwnWriteIsNotRecordedAsOwn(t *testing.T) {
	for _, m := range outsiderModes {
		t.Run(m.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "f.txt")
			deps := raceDeps()
			hook := onceHook(path, func() { outsiderWrite(t, path, "outsider\n", m.keepMtime) })
			deps.PostWriteNotifyFn = func(_ context.Context, p string) error { hook(p); return nil }
			if _, err := NewWriteFile(deps).Execute(context.Background(), mustJSON(map[string]any{"file_path": path, "content": "mine\n"})); err != nil {
				t.Fatal(err)
			}
			assertNextWriteSeesOutsider(t, deps, path, "outsider\n")
			if m.keepMtime {
				return // the write tracker is mtime-only; nothing to see here
			}
			// The write tracker records the same written version, so read_file's
			// concurrent-edit note still fires for the outsider's later mtime.
			out, err := NewReadFile(NewReadTracker()).WithWrites(deps.Writes).Execute(context.Background(), mustJSON(map[string]any{"file_path": path}))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out, "changed on disk since plumb last wrote it") {
				t.Fatalf("read_file must warn that the file changed after plumb's write:\n%s", out)
			}
		})
	}
}

func TestEditFile_OutsiderWriteAfterOwnEditIsNotRecordedAsOwn(t *testing.T) {
	for _, m := range outsiderModes {
		t.Run(m.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "f.go")
			if err := os.WriteFile(path, []byte("a\nb\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			deps := raceDeps()
			readVia(t, deps.Reads, path)
			hook := onceHook(path, func() { outsiderWrite(t, path, "outsider\n", m.keepMtime) })
			deps.PostWriteNotifyFn = func(_ context.Context, p string) error { hook(p); return nil }
			if _, err := NewEditFile(deps).Execute(context.Background(), mustJSON(map[string]any{
				"file_path": path,
				"edits":     []map[string]string{{"old_string": "a", "new_string": "A"}},
			})); err != nil {
				t.Fatal(err)
			}
			assertNextWriteSeesOutsider(t, deps, path, "outsider\n")
		})
	}
}

func TestTransactionApply_OutsiderWriteAfterCommitIsNotRecordedAsOwn(t *testing.T) {
	for _, m := range outsiderModes {
		t.Run(m.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "f.go")
			if err := os.WriteFile(path, []byte("a\nb\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			deps := raceDeps()
			hook := onceHook(path, func() { outsiderWrite(t, path, "outsider\n", m.keepMtime) })
			deps.PostWriteNotifyFn = func(_ context.Context, p string) error { hook(p); return nil }
			if _, err := NewTransactionApply(deps).Execute(context.Background(), mustJSON(map[string]any{
				"operations": []map[string]any{{
					"file_path": path,
					"edits":     []map[string]string{{"old_string": "a", "new_string": "A"}},
				}},
			})); err != nil {
				t.Fatal(err)
			}
			assertNextWriteSeesOutsider(t, deps, path, "outsider\n")
		})
	}
}

// TestRenameFile_OutsiderWriteAfterMoveIsNotRecordedAsOwn covers the one write
// that holds no bytes: rename_file moves an inode. The version it records must
// be the one it moved, not whatever the destination holds by the time the
// bookkeeping runs.
func TestRenameFile_OutsiderWriteAfterMoveIsNotRecordedAsOwn(t *testing.T) {
	for _, m := range outsiderModes {
		t.Run(m.name, func(t *testing.T) {
			dir := t.TempDir()
			from, to := filepath.Join(dir, "a.txt"), filepath.Join(dir, "b.txt")
			if err := os.WriteFile(from, []byte("moved\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			deps := raceDeps()
			deps.BlockDirtyFn = func() bool { return false }
			deps.Client = &notifyHookClient{hook: onceHook(to, func() { outsiderWrite(t, to, "outsider\n", m.keepMtime) })}
			if _, err := NewRenameFile(deps).Execute(context.Background(), mustJSON(map[string]any{"from": from, "to": to})); err != nil {
				t.Fatal(err)
			}
			assertNextWriteSeesOutsider(t, deps, to, "outsider\n")
		})
	}
}

// TestRenameFile_UnreadableSourceRecordsNoReadState: when rename_file cannot
// read the version it moves, it must not invent one. The destination then has no
// read record, exactly like a file the session never read, rather than a
// zero-mtime record that makes every later write look stale.
func TestRenameFile_UnreadableSourceRecordsNoReadState(t *testing.T) {
	dir := t.TempDir()
	from, to := filepath.Join(dir, "a.txt"), filepath.Join(dir, "b.txt")
	if err := os.WriteFile(from, []byte("secret\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	if f, err := os.Open(from); err == nil {
		_ = f.Close()
		t.Skip("a mode-0 file is readable here (running as root?)")
	}
	deps := raceDeps()
	deps.BlockDirtyFn = func() bool { return false }
	if _, err := NewRenameFile(deps).Execute(context.Background(), mustJSON(map[string]any{"from": from, "to": to})); err != nil {
		t.Fatal(err)
	}
	if e, ok := deps.Reads.recorded(to); ok {
		t.Fatalf("an unreadable move recorded read state %+v; want none", e)
	}
	if !deps.Writes.Wrote(to) {
		t.Fatal("the move must still be recorded as this session's write")
	}
}

// TestRecordWritten_OwnWritesStayUnflagged is the positive control for every
// tool above: with no outside writer, the version plumb records is the version
// on disk, so the session's next write — and strict mode's next edit — pass.
func TestRecordWritten_OwnWritesStayUnflagged(t *testing.T) {
	t.Setenv("PLUMB_STRICT_EDITS", "1")
	dir := t.TempDir()
	path := filepath.Join(dir, "f.go")
	deps := raceDeps()
	deps.BlockDirtyFn = func() bool { return false }
	ctx := context.Background()
	steps := []struct {
		name string
		run  func() error
	}{
		{"write_file creates", func() error {
			_, err := NewWriteFile(deps).Execute(ctx, mustJSON(map[string]any{"file_path": path, "content": "a\nb\n"}))
			return err
		}},
		{"edit_file (strict, no re-read)", func() error {
			_, err := NewEditFile(deps).Execute(ctx, mustJSON(map[string]any{
				"file_path": path, "edits": []map[string]string{{"old_string": "a", "new_string": "A"}},
			}))
			return err
		}},
		{"transaction_apply", func() error {
			_, err := NewTransactionApply(deps).Execute(ctx, mustJSON(map[string]any{
				"operations": []map[string]any{{"file_path": path, "edits": []map[string]string{{"old_string": "b", "new_string": "B"}}}},
			}))
			return err
		}},
		{"rename_file", func() error {
			moved := filepath.Join(dir, "g.go")
			if _, err := NewRenameFile(deps).Execute(ctx, mustJSON(map[string]any{"from": path, "to": moved})); err != nil {
				return err
			}
			path = moved
			return nil
		}},
		{"write_file overwrites", func() error {
			_, err := NewWriteFile(deps).Execute(ctx, mustJSON(map[string]any{"file_path": path, "content": "final\n"}))
			return err
		}},
	}
	for _, s := range steps {
		if err := s.run(); err != nil {
			t.Fatalf("%s: the session's own write must not be flagged: %v", s.name, err)
		}
		e, ok := deps.Reads.recorded(path)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		sha, _ := fileSHA256(path)
		if !ok || !e.mtime.Equal(info.ModTime()) || e.sha != sha {
			t.Fatalf("%s: recorded (%v, %s, ok=%v), want the on-disk version (%v, %s)", s.name, e.mtime, e.sha, ok, info.ModTime(), sha)
		}
	}
}
