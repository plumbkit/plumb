package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/mcp"
)

// servedMutationTool serves env.tool over pipes, like a real client connection
// that stays OPEN for the whole test: send writes one frame, frames delivers
// every frame the server writes.
func servedMutationTool(t *testing.T, env *mutationEnv) (send func(any), frames <-chan string) {
	t.Helper()
	srv := mcp.New(mcp.ServerInfo{Name: "t", Version: "0"})
	srv.Register(env.tool)
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = srv.Serve(context.Background(), inR, outW)
		_ = outW.Close()
	}()
	ch := make(chan string, 256)
	go func() {
		defer close(ch)
		sc := bufio.NewScanner(outR)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			ch <- sc.Text()
		}
	}()
	t.Cleanup(func() {
		_ = inW.Close()
		go func() {
			for range ch { //nolint:revive // drain so a blocked server write can finish
			}
		}()
		<-served
	})
	return func(v any) {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := inW.Write(append(b, '\n')); err != nil {
			t.Fatal(err)
		}
	}, ch
}

func mutationCall(id int, args json.RawMessage, meta map[string]any) map[string]any {
	params := map[string]any{"name": "mutation_test", "arguments": args}
	if meta != nil {
		params["_meta"] = meta
	}
	return map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call", "params": params}
}

// awaitResponse reads frames until the response to id, returning it and the
// progress notifications that came before it.
func awaitResponse(t *testing.T, frames <-chan string, id int, within time.Duration) (resp string, progress []map[string]any) {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case f, ok := <-frames:
			if !ok {
				t.Fatal("the server closed before responding")
			}
			var msg struct {
				ID     *int           `json:"id"`
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			if err := json.Unmarshal([]byte(f), &msg); err != nil {
				t.Fatalf("bad frame %q: %v", f, err)
			}
			if msg.Method == "notifications/progress" {
				progress = append(progress, msg.Params)
				continue
			}
			if msg.ID != nil && *msg.ID == id {
				return f, progress
			}
		case <-deadline:
			t.Fatalf("no response to request %d within %s", id, within)
		}
	}
}

// TestMutationTest_SlotFreedWhenClientCancelsTheCall is PLAN-450's core case:
// the client abandons the CALL (notifications/cancelled) but keeps its
// connection open, as Claude Code does when the user interrupts a tool on a
// shared serve connection. Watching only for the connection to close, an
// abandoned run carried on for another 50 minutes, writing mutant after mutant
// and refusing every other agent's run. It must stop instead: no further
// mutant, the file restored, the slot released — all while the connection is
// still open.
func TestMutationTest_SlotFreedWhenClientCancelsTheCall(t *testing.T) {
	const original = "answer = 42\nother = 7\n"
	env := newMutationEnv(t, original)
	env.installScript(t, env.testScript, holdSlotScript)
	raw, err := json.Marshal(map[string]any{"mutants": []map[string]any{
		{"file_path": env.file, "old_string": "42", "new_string": "43"},
		{"file_path": env.file, "old_string": "other = 7", "new_string": "other = 8"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	send, frames := servedMutationTool(t, env)
	send(mutationCall(1, raw, nil))
	waitForFile(t, filepath.Join(env.root, "test-started"))

	send(map[string]any{"jsonrpc": "2.0", "method": "notifications/cancelled", "params": map[string]any{"requestId": 1, "reason": "idle timeout"}})
	resp, _ := awaitResponse(t, frames, 1, 10*time.Second)
	if strings.Contains(resp, "other = 8") {
		t.Fatalf("the second mutant ran after the call was cancelled:\n%s", resp)
	}
	if !strings.Contains(resp, "1 of 2 mutants never ran") {
		t.Fatalf("the report must say the second mutant never ran:\n%s", resp)
	}
	if got := env.content(t); got != original {
		t.Fatalf("file not restored after the call was cancelled: got %q, want %q", got, original)
	}
	if h, held := mutationRun.snapshot(); held {
		t.Fatalf("slot still held after the call was cancelled: %+v", h)
	}
}

// TestMutationTest_ReportsProgressPerStep: with a progressToken, every compile
// and test step — the baseline's and each mutant's — sends one progress
// notification, which is what resets Claude Code's 30-minute idle window on a
// long run. Without a token none may be sent (the control).
func TestMutationTest_ReportsProgressPerStep(t *testing.T) {
	env := newMutationEnv(t, "answer = 42\nother = 7\n")
	raw, err := json.Marshal(map[string]any{"mutants": []map[string]any{
		{"file_path": env.file, "old_string": "42", "new_string": "43"},
		{"file_path": env.file, "old_string": "other = 7", "new_string": "other = 8"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	send, frames := servedMutationTool(t, env)

	send(mutationCall(1, raw, map[string]any{"progressToken": "mt-1"}))
	_, progress := awaitResponse(t, frames, 1, 30*time.Second)
	want := []string{
		"unmutated baseline, compile gate", "unmutated baseline, test command",
		"mutant 1/2, compile gate", "mutant 1/2, test command",
		"mutant 2/2, compile gate", "mutant 2/2, test command",
	}
	if len(progress) != len(want) {
		t.Fatalf("got %d progress notifications, want %d (one per step): %v", len(progress), len(want), progress)
	}
	for i, p := range progress {
		msg, _ := p["message"].(string)
		if p["progressToken"] != "mt-1" || p["progress"] != float64(i+1) || p["total"] != float64(len(want)) || !strings.Contains(msg, want[i]) {
			t.Errorf("notification %d = %v, want token mt-1, progress %d of %d, message naming %q", i, p, i+1, len(want), want[i])
		}
	}

	send(mutationCall(2, raw, nil))
	if _, progress = awaitResponse(t, frames, 2, 30*time.Second); len(progress) != 0 {
		t.Fatalf("a call without a progressToken got progress: %v", progress)
	}
}

// TestMutationTest_SilentRunOverBudgetIsRefusedUnmutated: a client that asked
// for no progress will hear nothing until the report, and Claude Code drops a
// silent call at 30 minutes. A run the baseline says will outlast that is
// refused before any mutant is written. The same run WITH a progressToken is
// kept alive by its progress, so it must not be refused (the control).
func TestMutationTest_SilentRunOverBudgetIsRefusedUnmutated(t *testing.T) {
	prev := mutationSilentBudget
	mutationSilentBudget = time.Nanosecond
	t.Cleanup(func() { mutationSilentBudget = prev })

	const original = "answer = 42\n"
	env := newMutationEnv(t, original)
	// A mutant that would be KILLED proves the run never got that far.
	env.failsOnlyWhenMutated(t, env.testScript, "43", "FAIL: TestAnswer")
	env.commitAll(t)

	_, err := env.run(t, "42", "43")
	if err == nil {
		t.Fatal("a silent run over the budget must be refused")
	}
	for _, want := range []string{"refused before mutating anything", "did not ask for progress", "test_target"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal is missing %q:\n%v", want, err)
		}
	}
	if got := env.content(t); got != original {
		t.Fatalf("the refused run touched the file: got %q", got)
	}

	send, frames := servedMutationTool(t, env)
	send(mutationCall(1, oneMutant(t, env.file), map[string]any{"progressToken": 1}))
	resp, _ := awaitResponse(t, frames, 1, 30*time.Second)
	if !strings.Contains(resp, "KILLED") {
		t.Fatalf("with progress the run must go ahead and kill the mutant:\n%s", resp)
	}
}

// TestSilentBudgetRefusal pins the budget's arithmetic: the time already spent
// counts, the batch size offered is what still fits, a run that fits is let
// through, and a call with progress or an unmeasured baseline is never refused.
func TestSilentBudgetRefusal(t *testing.T) {
	const budget = 25 * time.Minute
	m := time.Minute
	cases := []struct {
		name               string
		progress           bool
		elapsed, perMutant time.Duration
		mutants            int
		wantRefused        bool
		want               []string
	}{
		{name: "fits", elapsed: 4 * m, perMutant: 4 * m, mutants: 5},
		{name: "fits only without the time already spent", elapsed: 0, perMutant: 4 * m, mutants: 6},
		{
			name: "over", elapsed: 4 * m, perMutant: 4 * m, mutants: 6, wantRefused: true,
			want: []string{"took 4m", "6 mutants", "about 28m", "25m budget", "batches of at most 5"},
		},
		{
			name: "not even one fits", elapsed: 4 * m, perMutant: 30 * m, mutants: 1, wantRefused: true,
			want: []string{"1 mutant ", "Not even one mutant fits"},
		},
		{name: "progress keeps it alive", progress: true, elapsed: 4 * m, perMutant: 30 * m, mutants: 20},
		{name: "no measured baseline", elapsed: 4 * m, perMutant: 0, mutants: 20},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := silentBudgetRefusal(tc.progress, tc.elapsed, tc.perMutant, tc.mutants, budget)
			if (err != nil) != tc.wantRefused {
				t.Fatalf("refused = %v, want %v (err: %v)", err != nil, tc.wantRefused, err)
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("refusal is missing %q:\n%v", w, err)
				}
			}
		})
	}
}

// TestMutationHolder_BusyErrorSaysWhenTheSlotFrees: the refusal used to promise
// that the slot frees "as soon as its connection closes", which is false on a
// shared connection whose client merely stopped waiting — the exact case that
// left agents refused for an hour. It must name both real release paths and the
// case that releases nothing.
func TestMutationHolder_BusyErrorSaysWhenTheSlotFrees(t *testing.T) {
	msg := mutationHolder{step: stepTest, current: 1, mutants: 2, started: time.Now()}.busyError(time.Now()).Error()
	for _, want := range []string{"cancels the call", "its connection closes", "silently stops waiting", "holds the slot until it finishes"} {
		if !strings.Contains(msg, want) {
			t.Errorf("busy refusal is missing %q:\n%s", want, msg)
		}
	}
}
