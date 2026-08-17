package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestClassify_ACancelledTestCommandIsNeverAKill states the cancellation half of
// the same rule: a test command killed by a cancelled context carries exitCode
// -1, the same shape a naive check reads as "the tests ran and failed", so
// without its own branch a cancelled run certifies a kill for an assertion it
// never exercised.
func TestClassify_ACancelledTestCommandIsNeverAKill(t *testing.T) {
	var r mutationResult
	r.classify(
		stepOutcome{ran: true, exitCode: 0},
		stepOutcome{ran: true, exitCode: -1, cancelled: true},
	)
	if r.outcome == MutationKilled {
		t.Fatal("a test command killed by a cancelled context must never be classified killed")
	}
	if r.outcome != MutationInvalid || r.reason != reasonTestCancelled {
		t.Fatalf("outcome = %q reason = %q, want invalid/%s", r.outcome, r.reason, reasonTestCancelled)
	}
}

// TestStepOutcome_Failure pins failure()'s precedence directly: cancelled must
// resolve to its own cause and never collapse into a non-zero exit, which is
// the collapse that produced the false kill.
func TestStepOutcome_Failure(t *testing.T) {
	cases := []struct {
		name string
		in   stepOutcome
		want stepFailure
	}{
		{"ok", stepOutcome{ran: true, exitCode: 0}, stepOK},
		{"exited non-zero", stepOutcome{ran: true, exitCode: 1}, stepExited},
		{"unrunnable", stepOutcome{ran: true, exitCode: -1, startErr: true}, stepUnrunnable},
		{"timed out", stepOutcome{ran: true, exitCode: -1, timedOut: true}, stepTimedOut},
		{"cancelled", stepOutcome{ran: true, exitCode: -1, cancelled: true}, stepCancelled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.failure(); got != tc.want {
				t.Errorf("failure() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestMutationTest_CancelledTestRunIsInvalidNotKilled reproduces the false kill
// reached from the request side instead of the mutant side: a test command
// killed by a cancelled context is INVALID, never KILLED. The marker-based
// script makes the cancellation land mid-test deterministically rather than
// racing the scheduler.
func TestMutationTest_CancelledTestRunIsInvalidNotKilled(t *testing.T) {
	const original = "answer = 42\n"
	env := newMutationEnv(t, original)
	// The test step writes a marker and sleeps ONLY when the mutant is on disk,
	// so the baseline (unmutated) passes and the cancellation lands mid-test on
	// the mutant run — the exact shape that used to read as a kill.
	env.installScript(t, env.testScript, `if grep -q '43' "$(dirname "$0")/target.txt"; then
    touch "$(dirname "$0")/test-started"
    sleep 30
fi
exit 0`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	raw, err := json.Marshal(map[string]any{
		"mutants": []map[string]any{{"file_path": env.file, "old_string": "42", "new_string": "43"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := env.tool.Execute(ctx, raw)
		done <- result{out, err}
	}()

	// Wait until the mutant's test step is genuinely running, then cancel.
	waitForFile(t, filepath.Join(env.root, "test-started"))
	cancel()

	res := <-done
	if res.err != nil {
		t.Fatalf("run: %v", res.err)
	}
	if strings.Contains(res.out, "KILLED") {
		t.Fatalf("a cancelled test run must never be reported killed:\n%s", res.out)
	}
	if !strings.Contains(res.out, "INVALID") || !strings.Contains(res.out, reasonTestCancelled) {
		t.Fatalf("want INVALID with reason %q, got:\n%s", reasonTestCancelled, res.out)
	}
	if got := env.content(t); got != original {
		t.Fatalf("file not restored after cancellation: got %q, want %q", got, original)
	}
}

// waitForFile polls until path exists, failing the test after a generous
// deadline. The cancellation must land while the test step is running, and
// polling for the step's own marker is deterministic where a sleep would race
// the scheduler.
func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}
