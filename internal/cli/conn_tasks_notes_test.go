package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
)

// conn_tasks_notes_test.go covers the two seams the message tests cannot reach:
// the DELIVERY of the enriched {target} refusal to a live run_task caller, and
// the response notes that stop run_task rewriting or discarding a caller's
// intent silently.

// TestTaskResolver_TargetRefusalCrossesTheResolverSeam closes the one join the
// suite did not cross.
//
// The enriched refusal reaches a real caller through a single mapping from the
// errNoTargetPlaceholder sentinel. Every other fixture calls either the pure
// message builder or the pure step builder, so deleting that mapping left every
// live caller emitting the bare sentence this card exists to retire — and the
// whole suite stayed green. This drives the resolver itself.
func TestTaskResolver_TargetRefusalCrossesTheResolverSeam(t *testing.T) {
	ws := t.TempDir()
	const stored = "go test -race -count=1 ./..."
	s := newTaskTrustSession(t, ws, map[string]config.TasksConfig{"go": {Test: stored}})

	_, err := s.taskResolver("test", "./internal/cli", "")
	if err == nil {
		t.Fatal("a target against a placeholder-less command must be refused")
	}
	msg := err.Error()
	// Each of these is something the resolver alone can see — the stored command
	// and the file it came from — so none can be satisfied by the bare sentence.
	for _, want := range []string{"Stored command", stored, "-race", "{target"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the resolver flattened the enriched refusal (no %q): %s", want, msg)
		}
	}
	// The echo guard belongs HERE, at the only layer that has the caller's target
	// in hand and could therefore echo it. A refusal satisfied by quoting the
	// input back teaches the caller nothing about what is stored.
	if strings.Contains(msg, "./internal/cli") {
		t.Errorf("the refusal echoes the caller's own target rather than naming the stored command: %s", msg)
	}

	// The other direction, same session: an unscoped call still resolves and runs
	// the caller's command unchanged, so the assertions above cannot be satisfied
	// by a resolver that refuses everything.
	cmd, err := s.taskResolver("test", "", "")
	if err != nil {
		t.Fatalf("an unscoped call must still resolve: %v", err)
	}
	if want, _ := config.ParseTaskCommand(stored); len(cmd.Steps) != 1 || !slices.Equal(cmd.Steps[0], want) {
		t.Errorf("unscoped steps = %v, want the stored argv %v", cmd.Steps, want)
	}
}

// TestTaskStepsOrRefusal_IsTheSharedDeliverySeam pins that the mapping the CLI
// path uses is the same one the resolver uses. `plumb test <target>` cannot be
// driven without ambient workspace detection, so the two callers were unified
// onto this helper rather than left as two copies with a test on one of them.
func TestTaskStepsOrRefusal_IsTheSharedDeliverySeam(t *testing.T) {
	ws := t.TempDir()
	const stored = "gotestsum ./..."
	tc := config.TasksConfig{Test: stored}

	_, err := taskStepsOrRefusal(ws, tc, "go", "test", "./internal/cli")
	if err == nil {
		t.Fatal("a target against a placeholder-less command must be refused")
	}
	if !strings.Contains(err.Error(), stored) {
		t.Errorf("the shared seam must deliver the enriched refusal, got: %v", err)
	}
	// And it must not turn a perfectly good call into a refusal.
	steps, err := taskStepsOrRefusal(ws, tc, "go", "test", "")
	if err != nil || len(steps) != 1 {
		t.Fatalf("an unscoped call must build its steps: steps=%v err=%v", steps, err)
	}
}

// TestTaskResolver_CompositeSlotSaysTheTargetWasIgnored is the fold-in.
//
// run_task(slot:"verify", target:…) used to accept the target, discard it, run
// the WHOLE suite and report success — a green over a scope nobody asked for,
// which this package elsewhere calls worse than the hardcoded command it
// replaced. It must still not REFUSE (a refusal opens a new rejection cluster,
// the family this change exists to shrink); it must say so instead.
func TestTaskResolver_CompositeSlotSaysTheTargetWasIgnored(t *testing.T) {
	ws := t.TempDir()
	const target = "./internal/cli"
	s := newTaskTrustSession(t, ws, map[string]config.TasksConfig{"go": {
		Build: config.DefaultTaskCommand("go", "build"),
		Test:  config.DefaultTaskCommand("go", "test"),
	}})

	cmd, err := s.taskResolver("verify", target, "")
	if err != nil {
		t.Fatalf("a composite slot must not REFUSE a target — that trades one silent "+
			"failure for a new rejection cluster: %v", err)
	}
	subs, _ := compositeSubSlots("verify")
	if len(cmd.Steps) != len(subs) {
		t.Fatalf("steps = %v, want one per sub-slot %v", cmd.Steps, subs)
	}
	for _, argv := range cmd.Steps {
		if slices.Contains(argv, target) {
			t.Errorf("a step was scoped after all (%v) — the note would then be a lie", argv)
		}
	}
	note := strings.Join(cmd.Notes, "\n")
	// The target and the word that says it did not apply; the sub-slot the caller
	// should use instead (which the caller never named); and the sub-slot list.
	for _, want := range []string{target, "NOT applied", `slot "test"`, strings.Join(subs, " then ")} {
		if !strings.Contains(note, want) {
			t.Errorf("the response must state %q; notes were: %s", want, note)
		}
	}

	// Other direction, same build: nothing to report when nothing was dropped.
	un, err := s.taskResolver("verify", "", "")
	if err != nil {
		t.Fatalf("an unscoped composite must still resolve: %v", err)
	}
	if len(un.Notes) != 0 {
		t.Errorf("an unscoped call dropped nothing, so it must carry no note: %v", un.Notes)
	}
}

// TestTaskResolver_CompositeNoteNamesOnlyScopableSubSlots keeps the note honest
// when the workspace offers nothing to scope to. Recommending a sub-slot that
// would itself be refused sends the caller into the refusal this change exists
// to avoid.
func TestTaskResolver_CompositeNoteNamesOnlyScopableSubSlots(t *testing.T) {
	ws := t.TempDir()
	s := newTaskTrustSession(t, ws, map[string]config.TasksConfig{"go": {
		Build: "go build ./...",
		Test:  "gotestsum ./...", // no placeholder, and not a shipped default
	}})
	cmd, err := s.taskResolver("verify", "./internal/cli", "")
	if err != nil {
		t.Fatal(err)
	}
	note := strings.Join(cmd.Notes, "\n")
	if strings.Contains(note, `slot "test"`) || strings.Contains(note, `slot "build"`) {
		t.Errorf("the note recommends a sub-slot that would itself refuse the target: %s", note)
	}
	if !strings.Contains(note, "nothing to scope it to") {
		t.Errorf("with no scopable sub-slot the note must say so; got: %s", note)
	}
}

// TestTaskResolver_ScopedRunSaysThePlaceholderWasRestored is the disclosure half
// of reconciliation.
//
// Reconciliation rewrites a command the user wrote. That it is provably
// meaning-preserving is why it is allowed, not a reason to do it silently — and
// the pinned tools/list payload has no byte budget left for the schema to say
// so, which leaves the response and docs/tools.md.
func TestTaskResolver_ScopedRunSaysThePlaceholderWasRestored(t *testing.T) {
	ws := t.TempDir()
	stored := expandShippedDefault(t, "go", "test")
	s := newTaskTrustSession(t, ws, map[string]config.TasksConfig{"go": {Test: stored}})

	cmd, err := s.taskResolver("test", "./internal/cli", "")
	if err != nil {
		t.Fatalf("the expanded shipped default must still scope: %v", err)
	}
	note := strings.Join(cmd.Notes, "\n")
	for _, want := range []string{stored, "placeholder", "identical argv"} {
		if !strings.Contains(note, want) {
			t.Errorf("a rewritten command must be disclosed with %q; notes were: %s", want, note)
		}
	}

	// Other direction: a command plumb did NOT rewrite must claim no rewrite.
	untouched := newTaskTrustSession(t, ws, map[string]config.TasksConfig{
		"go": {Test: config.DefaultTaskCommand("go", "test")},
	})
	plain, err := untouched.taskResolver("test", "./internal/cli", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(plain.Notes) != 0 {
		t.Errorf("a command carrying its own placeholder was not rewritten, so nothing "+
			"should be claimed: %v", plain.Notes)
	}
}
