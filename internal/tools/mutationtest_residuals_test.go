package tools

import (
	"context"
	"encoding/json"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMutationTest_HeartbeatDuringALongStep: progress only at a step's start
// leaves a step longer than the client's idle window silent for its whole
// length, and Claude Code drops the call (30 min for stdio, 5 for http/sse). A
// step that is still running re-reports progress on a heartbeat. The values
// must still strictly increase, and a heartbeat must stay below the next step's
// whole number, so the per-step reports keep their meaning. Without a token
// there is no heartbeat either (the control).
func TestMutationTest_HeartbeatDuringALongStep(t *testing.T) {
	prev := mutationHeartbeat
	mutationHeartbeat = 60 * time.Millisecond
	t.Cleanup(func() { mutationHeartbeat = prev })
	env := newMutationEnv(t, "answer = 42\n")
	env.installScript(t, env.testScript, "sleep 0.5\nexit 0")
	env.commitAll(t)
	send, frames := servedMutationTool(t, env)

	send(mutationCall(1, oneMutant(t, env.file), map[string]any{"progressToken": "hb"}))
	_, progress := awaitResponse(t, frames, 1, 30*time.Second)
	last, beats, steps := 0.0, 0, 0
	for _, p := range progress {
		v, _ := p["progress"].(float64)
		msg, _ := p["message"].(string)
		if v <= last {
			t.Fatalf("progress went %v → %v; it must strictly increase: %v", last, v, progress)
		}
		last = v
		switch whole := v == math.Floor(v); {
		case strings.Contains(msg, "still running"):
			beats++
			if whole {
				t.Errorf("heartbeat %v reached a whole step number; it must stay below the next step's", v)
			}
		case !whole:
			t.Errorf("step report %v (%q) is not a whole step number", v, msg)
		default:
			steps++
		}
	}
	if steps != 4 {
		t.Errorf("got %d step reports, want 4 (baseline and mutant, compile and test): %v", steps, progress)
	}
	if beats < 2 {
		t.Errorf("got %d heartbeats across two 0.5 s test steps with a 60 ms heartbeat, want several: %v", beats, progress)
	}

	send(mutationCall(2, oneMutant(t, env.file), nil))
	if _, progress = awaitResponse(t, frames, 2, 30*time.Second); len(progress) != 0 {
		t.Fatalf("a call without a progressToken got progress or a heartbeat: %v", progress)
	}
}

// TestMutationTest_RefundsWriteBudgetItNeverUsed: the write budget is charged
// before the baseline, so a run refused by it (red suite) or by the silent
// budget, or stopped before some mutants start, has paid for writes it never
// made. Those slots are given back; a mutant that ran keeps its charge.
func TestMutationTest_RefundsWriteBudgetItNeverUsed(t *testing.T) {
	spent := func(t *testing.T, lim *RateLimiter) int {
		t.Helper()
		n, _, _ := lim.Snapshot()
		return n
	}

	t.Run("refused by a red baseline", func(t *testing.T) {
		env := newMutationEnv(t, "answer = 42\n")
		lim := NewRateLimiter(100, time.Minute)
		env.tool.deps.Limiter = lim
		env.installScript(t, env.testScript, "exit 1")
		env.commitAll(t)
		if _, err := env.run(t, "42", "43"); err == nil {
			t.Fatal("a red baseline must refuse the run")
		}
		if n := spent(t, lim); n != 0 {
			t.Errorf("a run the baseline refused kept %d write slots, want 0", n)
		}
	})

	t.Run("refused by the silent budget", func(t *testing.T) {
		withSilentBudget(t, time.Nanosecond)
		env := newMutationEnv(t, lineFixture(3))
		lim := NewRateLimiter(100, time.Minute)
		env.tool.deps.Limiter = lim
		if _, err := env.tool.Execute(context.Background(), lineMutants(t, env.file, 3)); err == nil {
			t.Fatal("the silent budget must refuse the run")
		}
		if n := spent(t, lim); n != 0 {
			t.Errorf("a run the silent budget refused kept %d write slots, want 0", n)
		}
	})

	t.Run("cancelled after the first of two", func(t *testing.T) {
		env := newMutationEnv(t, "answer = 42\nother = 7\n")
		lim := NewRateLimiter(100, time.Minute)
		env.tool.deps.Limiter = lim
		env.installScript(t, env.testScript, holdSlotScript)
		raw, err := json.Marshal(map[string]any{"mutants": []map[string]any{
			{"file_path": env.file, "old_string": "42", "new_string": "43"},
			{"file_path": env.file, "old_string": "other = 7", "new_string": "other = 8"},
		}})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { _, err := env.tool.Execute(ctx, raw); done <- err }()
		waitForFile(t, filepath.Join(env.root, "test-started"))
		cancel()
		if err := <-done; err != nil {
			t.Fatalf("run: %v", err)
		}
		if n := spent(t, lim); n != 1 {
			t.Errorf("a run cancelled during its first of two mutants kept %d write slots, want 1 (the mutant that ran)", n)
		}
	})
}

// TestRateLimiter_RefundReturnsExactlyWhatAllowTook: refund gives the slot back
// locally and in the shared parent — and only where Allow recorded one, so a
// child with limiting disabled never pops a sibling's stamp from the parent.
func TestRateLimiter_RefundReturnsExactlyWhatAllowTook(t *testing.T) {
	parent := NewRateLimiter(10, time.Minute)
	child := NewRateLimiter(10, time.Minute)
	child.SetParent(parent)
	for range 3 {
		if !child.Allow() {
			t.Fatal("Allow refused under the limit")
		}
	}
	for range 3 {
		child.refund()
	}
	if c, _, _ := child.Snapshot(); c != 0 {
		t.Errorf("child kept %d slots after refunding all three", c)
	}
	if p, _, _ := parent.Snapshot(); p != 0 {
		t.Errorf("parent kept %d slots after the child refunded all three", p)
	}

	sibling := NewRateLimiter(10, time.Minute)
	sibling.SetParent(parent)
	if !sibling.Allow() {
		t.Fatal("sibling Allow refused")
	}
	disabled := NewRateLimiter(0, time.Minute)
	disabled.SetParent(parent)
	disabled.Allow()
	disabled.refund()
	if p, _, _ := parent.Snapshot(); p != 1 {
		t.Errorf("a disabled child's refund changed the parent to %d slots; the sibling's 1 must stay", p)
	}
}
