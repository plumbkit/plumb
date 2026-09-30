package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// expected_mtime_same_tick_test.go: an expected_mtime that MATCHES the file does
// not prove the content is what the caller read. A coarse timestamp clock (ext4,
// HFS+, FAT, some network mounts) gives two writes in one tick the same mtime, and
// cp -p / rsync -t / tar restore it on purpose. When the session recorded a SHA at
// that mtime, the three guarded write tools must check it — the same fallback the
// unguarded path (changedSinceSessionRead) already applies — or following the
// documented read_file → expected_mtime protocol protects LESS than passing no
// guard at all. This is also what made TestWriteFile_ExpectedMtimeGuard flaky on
// Linux CI: its own two writes shared an mtime.

// sameTickFixture: the session reads v1 through read_file, which records v1's
// mtime and SHA. When peer is true, another writer then replaces the content and
// leaves v1's mtime in place. It returns the session's deps, the path, and the
// expected_mtime the session took from its read.
func sameTickFixture(t *testing.T, peer bool) (WriteDeps, string, string) {
	t.Helper()
	ws := initPlumbWorkspace(t)
	path := filepath.Join(ws, "guarded.txt")
	if err := os.WriteFile(path, []byte("line one\nline two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	deps := newSessionDeps(t, ws)
	if _, err := NewReadFile(deps.Reads).Execute(context.Background(), mustJSON(map[string]any{"file_path": path})); err != nil {
		t.Fatalf("read_file: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	mtime := info.ModTime()
	if peer {
		if err := os.WriteFile(path, []byte("PEER one\nPEER two\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	return deps, path, mtime.Format(time.RFC3339Nano)
}

// sameTickWrites are the three tools' guarded writes, each replacing line one.
var sameTickWrites = map[string]func(deps WriteDeps, path, mtime string) error{
	"write_file": func(deps WriteDeps, path, mtime string) error {
		_, err := NewWriteFile(deps).Execute(context.Background(), mustJSON(map[string]any{
			"file_path": path, "content": "mine\n", "expected_mtime": mtime,
		}))
		return err
	},
	"edit_file line range": func(deps WriteDeps, path, mtime string) error {
		_, err := NewEditFile(deps).Execute(context.Background(), mustJSON(map[string]any{
			"file_path": path, "expected_mtime": mtime,
			"edits": []map[string]any{{"start_line": 1, "end_line": 1, "new_string": "mine"}},
		}))
		return err
	},
	"transaction_apply": func(deps WriteDeps, path, mtime string) error {
		old := "line one"
		if b, _ := os.ReadFile(path); strings.HasPrefix(string(b), "PEER") {
			old = "PEER one" // an anchor that matches, so only the guard can refuse
		}
		_, err := NewTransactionApply(deps).Execute(context.Background(), mustJSON(map[string]any{
			"operations": []map[string]any{{
				"file_path": path, "expected_mtime": mtime,
				"edits": []map[string]string{{"old_string": old, "new_string": "mine"}},
			}},
		}))
		return err
	},
}

func TestExpectedMtime_ASameMtimePeerChangeIsRefused(t *testing.T) {
	for name, write := range sameTickWrites {
		t.Run(name, func(t *testing.T) {
			deps, path, mtime := sameTickFixture(t, true)
			err := write(deps, path, mtime)
			if err == nil || !strings.Contains(err.Error(), "same mtime, different content") {
				t.Fatalf("a peer's change behind a matching expected_mtime must be refused; got %v", err)
			}
			if got, _ := os.ReadFile(path); string(got) != "PEER one\nPEER two\n" {
				t.Errorf("the refused write must leave the peer's content; got %q", got)
			}
		})
	}
}

// TestExpectedMtime_AnUnchangedFileIsStillWritten is the control: the same
// matching token over content the session did read must go through.
func TestExpectedMtime_AnUnchangedFileIsStillWritten(t *testing.T) {
	for name, write := range sameTickWrites {
		t.Run(name, func(t *testing.T) {
			deps, path, mtime := sameTickFixture(t, false)
			if err := write(deps, path, mtime); err != nil {
				t.Fatalf("a matching expected_mtime over unchanged content must be written: %v", err)
			}
			if got, _ := os.ReadFile(path); !strings.HasPrefix(string(got), "mine") {
				t.Errorf("the write must land; got %q", got)
			}
		})
	}
}
