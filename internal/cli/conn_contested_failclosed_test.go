package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/tools"
)

// connFailClosedJSON marshals v into the json.RawMessage a tool Execute expects.
func connFailClosedJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// TestContestedFailClosed_RelativeRefusedAbsoluteWorks is section 3's headline: a
// single-agent connection is completely unaffected (relative paths resolve as
// they always have), while a connection made contested by the two-root force
// signature refuses RELATIVE path-bearing calls, instructively, and still
// admits absolute paths inside the current jail.
func TestContestedFailClosed_RelativeRefusedAbsoluteWorks(t *testing.T) {
	store, ss := newOriginStore(t)
	rootA, rootB := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, rootA)
	mustGitDir(t, rootB)
	ctx := context.Background()
	s := newPersistSession(t, store, ss, "proxyX")

	// A single explicit pin is NOT contested.
	if _, err := s.repinWorkspace(ctx, rootA, "", false); err != nil {
		t.Fatalf("repinWorkspace(A): %v", err)
	}
	if s.pinContested() {
		t.Fatal("a single explicit pin must not contest the connection")
	}
	write := tools.NewWriteFile(s.buildWriteDeps())
	if err := execWriteFile(t, write, "before.txt", "hi"); err != nil {
		t.Fatalf("single-agent relative write_file must succeed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rootA, "before.txt")); err != nil {
		t.Fatalf("single-agent relative write did not land in the pinned root: %v", err)
	}

	// Two forced alternations between two distinct roots: the trigger.
	if _, err := s.repinWorkspace(ctx, rootB, "", true); err != nil {
		t.Fatalf("forced repin(B): %v", err)
	}
	if s.pinContested() {
		t.Fatal("one forced re-pin is an ordinary switch, not a contest")
	}
	if _, err := s.repinWorkspace(ctx, rootA, "", true); err != nil {
		t.Fatalf("forced repin back to A: %v", err)
	}
	if !s.pinContested() {
		t.Fatal("two forced alternations between two roots did not make the connection contested")
	}

	// Relative write is refused, and the refusal is the instruction.
	err := execWriteFile(t, write, "after.txt", "hi")
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "absolute path") {
		t.Fatalf("relative write_file on contested must refuse instructively, got %v", err)
	}

	// Absolute write inside the current jail still works.
	abs := filepath.Join(rootA, "after.txt")
	if err := execWriteFile(t, write, abs, "hi"); err != nil {
		t.Fatalf("absolute write_file inside the jail must succeed: %v", err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("absolute write did not land: %v", err)
	}

	// The read half is refused too (STEP 0: 90 of the incident's read/edit calls
	// were relative), and the absolute read still works.
	read := tools.NewReadFile(s.readTracker).
		WithBoundary(s.readBoundaryGuardFor).
		WithWorkspace(s.workspaceFor).
		WithContested(s.pinContested)
	if _, err := read.Execute(ctx, connFailClosedJSON(t, map[string]any{"file_path": "after.txt"})); err == nil ||
		!strings.Contains(strings.ToLower(err.Error()), "absolute path") {
		t.Fatalf("relative read_file on contested must refuse instructively, got %v", err)
	}
	if _, err := read.Execute(ctx, connFailClosedJSON(t, map[string]any{"file_path": abs})); err != nil {
		t.Fatalf("absolute read_file inside the jail must succeed: %v", err)
	}
}

// TestContestedFailClosed_PathlessToolsRefused covers the genuinely pathless
// state-changing tools section 3 must refuse on a contested connection: git
// without an explicit repo, run_task (which has no workspace argument), and
// undo_edit.
func TestContestedFailClosed_PathlessToolsRefused(t *testing.T) {
	store, ss := newOriginStore(t)
	rootA, rootB := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, rootA)
	mustGitDir(t, rootB)
	ctx := context.Background()
	s := newPersistSession(t, store, ss, "proxyX")
	if _, err := s.repinWorkspace(ctx, rootA, "", false); err != nil {
		t.Fatalf("repinWorkspace(A): %v", err)
	}
	if _, err := s.repinWorkspace(ctx, rootB, "", true); err != nil {
		t.Fatalf("forced repin(B): %v", err)
	}
	if _, err := s.repinWorkspace(ctx, rootA, "", true); err != nil {
		t.Fatalf("forced repin back to A: %v", err)
	}
	if !s.pinContested() {
		t.Fatal("two forced alternations did not contest the connection")
	}

	// git without repo falls back to the pinned workspace, the exact root being
	// fought over, and is refused, naming the repo escape hatch.
	git := tools.NewGit(s.buildWriteDeps(), nil)
	if _, err := git.Execute(ctx, connFailClosedJSON(t, map[string]any{"subcommand": "status"})); err == nil ||
		!strings.Contains(strings.ToLower(err.Error()), "repo") {
		t.Fatalf("git without repo on contested must refuse naming repo, got %v", err)
	}

	// run_task has no workspace argument, so on a contested connection it cannot
	// be attributed to a root and is refused outright.
	tasks := tools.NewTasks(s.buildWriteDeps(), nil)
	if _, err := tasks.Execute(ctx, connFailClosedJSON(t, map[string]any{"slot": "test"})); err == nil ||
		!strings.Contains(err.Error(), "run_task") {
		t.Fatalf("run_task on contested must refuse, got %v", err)
	}

	// undo_edit reverts the connection's most recent write, which on a contested
	// connection cannot be attributed to the agent that made it.
	undo := tools.NewUndoEdit(s.buildWriteDeps())
	if _, err := undo.Execute(ctx, connFailClosedJSON(t, map[string]any{"file_path": filepath.Join(rootA, "x.txt")})); err == nil ||
		!strings.Contains(err.Error(), "undo_edit") {
		t.Fatalf("undo_edit on contested must refuse, got %v", err)
	}
}

func execWriteFile(t *testing.T, w *tools.WriteFile, path, content string) error {
	t.Helper()
	_, err := w.Execute(context.Background(), connFailClosedJSON(t, map[string]any{"file_path": path, "content": content}))
	return err
}
