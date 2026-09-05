package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// mutationtest_safety_test.go holds mutation_test's SAFETY half: the restore
// guarantee on every exit path, and the preconditions the tool refuses to start
// without. The classification tests live in mutationtest_test.go beside the
// harness they share.
//
// The split is by responsibility rather than size: these tests are about what
// the tool must never do to a working tree, and they are the ones to read first
// when changing anything in the mutate → verify → restore cycle.

func TestMutationTest_RestoresOnEveryOutcome(t *testing.T) {
	const original = "answer = 42\n"
	// Each script reacts to the mutant rather than failing outright, so the
	// baseline is green and the failure it later sees is attributable.
	cases := []struct {
		name                    string
		compileFails, testFails bool
	}{
		{"tests pass (survived)", false, false},
		{"tests fail (killed)", false, true},
		{"compile fails (invalid)", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newMutationEnv(t, original)
			if tc.compileFails {
				env.failsOnlyWhenMutated(t, env.compileScript, "999", "compile error")
			}
			if tc.testFails {
				env.failsOnlyWhenMutated(t, env.testScript, "999", "FAIL: TestAnswer")
			}
			env.commitAll(t)

			if _, err := env.run(t, "42", "999"); err != nil {
				t.Fatalf("run: %v", err)
			}
			if got := env.content(t); got != original {
				t.Fatalf("file not restored: got %q, want %q", got, original)
			}
		})
	}
}

// TestMutationTest_UnstartableCommandIsRefusedBeforeMutating covers a command
// that cannot be launched at all: the baseline meets it first, so the run is
// refused with nothing written.
func TestMutationTest_UnstartableCommandIsRefusedBeforeMutating(t *testing.T) {
	const original = "answer = 42\n"
	env := newMutationEnv(t, original)
	env.tool = NewMutationTest(
		WriteDeps{WorkspaceFn: func(context.Context) string { return env.root }},
		func(slot, _, _ string) (TaskCommand, error) {
			return TaskCommand{Slot: slot, Steps: [][]string{{"/nonexistent/plumb-mutation-binary"}}, Provenance: "default"}, nil
		})

	out, err := env.run(t, "42", "43")
	if err == nil {
		t.Fatalf("a command that cannot be started must refuse the run; got report:\n%s", out)
	}
	if got := env.content(t); got != original {
		t.Fatalf("nothing may be written when the baseline refuses: got %q", got)
	}
	if strings.Contains(out, "KILLED") {
		t.Errorf("a command that could not start must never read as a kill; got:\n%s", out)
	}
}

// TestMutationTest_RefusesAnAlreadyRedSuite is the baseline's reason to exist.
//
// A kill means "the suite passed before this change and fails after it". Only
// the second half was ever checked, so a suite that was already failing — a
// peer's edit elsewhere in the tree, a pre-existing failure, a missing test
// dependency — reported EVERY mutant killed, certifying assertions that were
// never exercised. The dirty-file refusal does not cover this: it guards the
// file being mutated, not the rest of the workspace.
func TestMutationTest_RefusesAnAlreadyRedSuite(t *testing.T) {
	const original = "answer = 42\n"
	env := newMutationEnv(t, original)
	// Fails whatever the file says — nothing to do with any mutant.
	env.installScript(t, env.testScript, "echo 'FAIL: TestSomethingElse'; exit 1")
	env.commitAll(t)

	out, err := env.run(t, "42", "43")
	if err == nil {
		t.Fatalf("an already-failing suite must refuse the run, not report kills; got:\n%s", out)
	}
	if !strings.Contains(err.Error(), "ALREADY fails") {
		t.Errorf("the refusal must name the cause; got: %v", err)
	}
	if strings.Contains(err.Error(), "KILLED") {
		t.Errorf("no mutant may be reported killed; got: %v", err)
	}
	if got := env.content(t); got != original {
		t.Fatalf("the file must not be touched when the baseline refuses: got %q", got)
	}
}

// TestMutationTest_RefusesWhenTheBaselineDoesNotCompile is the same guarantee on
// the compile side: a workspace that was already broken cannot attribute a
// failed compile to the mutant.
func TestMutationTest_RefusesWhenTheBaselineDoesNotCompile(t *testing.T) {
	const original = "answer = 42\n"
	env := newMutationEnv(t, original)
	env.installScript(t, env.compileScript, "echo 'pre-existing build error'; exit 2")
	env.commitAll(t)

	out, err := env.run(t, "42", "43")
	if err == nil {
		t.Fatalf("a workspace that does not build must refuse the run; got:\n%s", out)
	}
	if !strings.Contains(err.Error(), "BEFORE any mutant") {
		t.Errorf("the refusal must say the breakage predates the mutant; got: %v", err)
	}
	if got := env.content(t); got != original {
		t.Fatalf("the file must not be touched when the baseline refuses: got %q", got)
	}
}

// TestMutationTest_RestoresOnPanic proves the deferred restore survives a panic
// unwinding through runOne — the exit path a plain post-run cleanup would miss.
func TestMutationTest_RestoresOnPanic(t *testing.T) {
	const original = "answer = 42\n"
	env := newMutationEnv(t, original)
	// Panic only once the mutant is genuinely on disk. Gating it on the file's
	// content is what stops this test passing vacuously: WorkspaceFn is first
	// consulted during preflight, long before anything is written, so an
	// unconditional panic would "prove" a restore that never had to happen.
	env.tool.deps.WorkspaceFn = func(context.Context) string {
		if b, err := os.ReadFile(env.file); err == nil && string(b) != original {
			panic("boom")
		}
		return env.root
	}

	func() {
		defer func() {
			if recover() == nil {
				t.Error("expected the panic to propagate")
			}
		}()
		_, _ = env.run(t, "42", "43")
	}()

	if got := env.content(t); got != original {
		t.Fatalf("file not restored after a panic: got %q, want %q", got, original)
	}
}

// TestMutationTest_RestoresOnCancelledContext pins that restoration is not
// cancellable: the request's context is already dead when the restore runs.
func TestMutationTest_RestoresOnCancelledContext(t *testing.T) {
	const original = "answer = 42\n"
	env := newMutationEnv(t, original)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Cancel once the mutant is genuinely on disk, not before. A context that is
	// already dead at entry is refused by the baseline without mutating anything,
	// which would pass this test while proving nothing about the restore.
	env.tool.deps.WorkspaceFn = cancelOnceMutated(env, original, cancel)

	raw, err := json.Marshal(map[string]any{
		"mutants": []map[string]any{{"file_path": env.file, "old_string": "42", "new_string": "43"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.tool.Execute(ctx, raw); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := env.content(t); got != original {
		t.Fatalf("file not restored under a cancelled context: got %q, want %q", got, original)
	}
}

// cancelOnceMutated returns a WorkspaceFn that kills the request the first time
// it is consulted with the mutant already on disk.
//
// WorkspaceFn is read by runStep just before each command launches, so this
// fires between the mutating write and the compile step — the narrow window a
// cancellation has to hit for the restore to be the thing under test.
func cancelOnceMutated(env *mutationEnv, original string, cancel context.CancelFunc) func(context.Context) string {
	return func(context.Context) string {
		if b, err := os.ReadFile(env.file); err == nil && string(b) != original {
			cancel()
		}
		return env.root
	}
}

// ctxRecordingClient is an lsp.Client that records the context each
// didChangeWatchedFiles notification arrives with. The embedded nil interface
// satisfies the other 22 methods at compile time and panics if one is called,
// which is the desired outcome — the write path must touch nothing else.
type ctxRecordingClient struct {
	lsp.Client
	mu   sync.Mutex
	errs []error
}

func (c *ctxRecordingClient) DidChangeWatchedFiles(ctx context.Context, _ protocol.DidChangeWatchedFilesParams) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.errs = append(c.errs, ctx.Err())
	return nil
}

func (c *ctxRecordingClient) recorded() []error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]error(nil), c.errs...)
}

// TestMutationTest_RestoreNotificationOutlivesACancelledContext pins the
// context.WithoutCancel in restore. Restoring the BYTES survives cancellation
// for free (the write path takes no context), so the only thing WithoutCancel
// buys is the notification that tells the language server the mutant is gone —
// and without it, a cancelled request leaves the server believing the mutated
// content is still on disk. That is invisible to a test with no LSP client
// wired, which is exactly how it survived mutation until this test existed.
func TestMutationTest_RestoreNotificationOutlivesACancelledContext(t *testing.T) {
	const original = "answer = 42\n"
	env := newMutationEnv(t, original)
	client := &ctxRecordingClient{}
	env.tool.deps.Client = client

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// As above: the cancellation has to land AFTER the mutant is written, or the
	// baseline refuses the run and there is no restore notification to observe.
	env.tool.deps.WorkspaceFn = cancelOnceMutated(env, original, cancel)
	raw, err := json.Marshal(map[string]any{
		"mutants": []map[string]any{{"file_path": env.file, "old_string": "42", "new_string": "43"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.tool.Execute(ctx, raw); err != nil {
		t.Fatalf("run: %v", err)
	}

	got := client.recorded()
	if len(got) < 2 {
		t.Fatalf("want a notification for both the mutation and the restore, got %d", len(got))
	}
	// The LAST notification is the restore's, and it must carry a live context.
	if last := got[len(got)-1]; last != nil {
		t.Fatalf("the restore notification ran on a cancelled context (%v) — "+
			"the language server is left believing the mutant is still on disk", last)
	}
}

// --- safety preconditions ----------------------------------------------------

func TestMutationTest_RefusesDirtyFile(t *testing.T) {
	env := newMutationEnv(t, "answer = 42\n")
	if err := os.WriteFile(env.file, []byte("answer = 42\nlocal edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := env.run(t, "42", "43")
	if err == nil {
		t.Fatal("expected a refusal for a file with uncommitted changes")
	}
	if !strings.Contains(err.Error(), "uncommitted changes") {
		t.Errorf("refusal should name the dirty file; got %v", err)
	}
	// The refusal must be all-or-nothing: the working copy is untouched.
	if got := env.content(t); got != "answer = 42\nlocal edit\n" {
		t.Errorf("a refused run must not touch the file; got %q", got)
	}
}

// TestMutationTest_RefusesDirtyFileBeforeMutatingAnyOther pins the
// all-or-nothing preflight: one dirty file in a batch stops the whole run, so a
// clean sibling is never left mutated by a request that was going to be refused.
func TestMutationTest_RefusesDirtyFileBeforeMutatingAnyOther(t *testing.T) {
	env := newMutationEnv(t, "answer = 42\n")
	clean := filepath.Join(env.root, "clean.txt")
	if err := os.WriteFile(clean, []byte("value = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env.commitAll(t)
	if err := os.WriteFile(env.file, []byte("answer = 42\ndirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := env.runArgs(t, map[string]any{"mutants": []map[string]any{
		{"file_path": clean, "old_string": "1", "new_string": "2"},
		{"file_path": env.file, "old_string": "42", "new_string": "43"},
	}})
	if err == nil {
		t.Fatal("expected the batch to be refused")
	}
	b, readErr := os.ReadFile(clean)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(b) != "value = 1\n" {
		t.Errorf("the clean sibling must not have been mutated by a refused batch; got %q", b)
	}
}

// TestMutationTest_RefusesWithoutACompileGate pins that a workspace with no
// build command is refused outright rather than served verdicts no compile
// check stands behind.
func TestMutationTest_RefusesWithoutACompileGate(t *testing.T) {
	env := newMutationEnv(t, "answer = 42\n")
	env.tool = NewMutationTest(
		WriteDeps{WorkspaceFn: func(context.Context) string { return env.root }},
		func(slot, _, _ string) (TaskCommand, error) {
			if slot == "build" {
				return TaskCommand{Slot: slot, Provenance: "default"}, nil // no steps
			}
			return TaskCommand{Slot: slot, Steps: [][]string{{"/bin/sh", env.testScript}}, Provenance: "default"}, nil
		})

	_, err := env.run(t, "42", "43")
	if err == nil {
		t.Fatal("expected a refusal when no compile command is configured")
	}
	if !strings.Contains(err.Error(), "cannot be proven") {
		t.Errorf("refusal should explain the missing compile proof; got %v", err)
	}
	if got := env.content(t); got != "answer = 42\n" {
		t.Errorf("a refused run must not touch the file; got %q", got)
	}
}

// TestMutationTest_RefusesConcurrentRun drives two real runs at once rather
// than holding mutationRunLock from the test. Holding it here would make the
// guard's REMOVAL surface as "unlock of unlocked mutex" — a process-level
// fatal, not an assertion — which fails for the right reason by accident and
// says nothing about what broke. Two genuine callers make the removal show up
// as what it is: two runs proceeding where one had to be refused.
func TestMutationTest_RefusesConcurrentRun(t *testing.T) {
	env := newMutationEnv(t, "answer = 42\n")
	// Hold the first run inside its test step long enough for the second to overlap.
	env.installScript(t, env.testScript, "sleep 1; exit 0")
	env.commitAll(t)

	raw, err := json.Marshal(map[string]any{
		"mutants": []map[string]any{{"file_path": env.file, "old_string": "42", "new_string": "43"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	start := make(chan struct{})
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = env.tool.Execute(context.Background(), raw)
		}()
	}
	close(start)
	wg.Wait()

	var refused int
	for _, e := range errs {
		if e != nil && strings.Contains(e.Error(), "already in progress") {
			refused++
		}
	}
	if refused != 1 {
		t.Fatalf("exactly one of two concurrent runs must be refused, got %d refusals (errs: %v, %v)", refused, errs[0], errs[1])
	}
}
