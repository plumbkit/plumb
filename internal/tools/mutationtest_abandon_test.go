package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
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

// seenMutantScript is a test step that records, in a marker file next to the
// target, that it ran against a MUTATED tree (any line rewritten to start with
// "b"). The file is restored after every mutant, so its final content cannot
// tell "never mutated" from "mutated and put back"; the marker can.
const seenMutantScript = `if grep -q '^b' "$(dirname "$0")/target.txt"; then touch "$(dirname "$0")/mutant-seen"; fi
exit 0`

// lineMutants rewrites lines a1..aN to b1..bN, one mutant each.
func lineMutants(t *testing.T, file string, n int) json.RawMessage {
	t.Helper()
	ms := make([]map[string]any, n)
	for i := range n {
		ms[i] = map[string]any{"file_path": file, "old_string": fmt.Sprintf("a%d\n", i+1), "new_string": fmt.Sprintf("b%d\n", i+1)}
	}
	raw, err := json.Marshal(map[string]any{"mutants": ms})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func lineFixture(n int) string {
	var sb strings.Builder
	for i := range n {
		fmt.Fprintf(&sb, "a%d\n", i+1)
	}
	return sb.String()
}

func withSilentBudget(t *testing.T, d time.Duration) {
	t.Helper()
	prev := mutationSilentBudget
	mutationSilentBudget = d
	t.Cleanup(func() { mutationSilentBudget = prev })
}

// TestMutationTest_SilentRunOverBudgetIsRefusedUnmutated: a client that asked
// for no progress will hear nothing until the report, and Claude Code drops a
// silent call at 30 minutes. A run the baseline says will outlast that is
// refused before any mutant is written — pinned by a marker the test step
// leaves whenever it sees a mutant, since the restored file cannot show it. The
// same run WITH a progressToken is kept alive by its progress, so it must not be
// refused (the control).
func TestMutationTest_SilentRunOverBudgetIsRefusedUnmutated(t *testing.T) {
	withSilentBudget(t, time.Nanosecond)
	original := lineFixture(1)
	env := newMutationEnv(t, original)
	env.installScript(t, env.testScript, seenMutantScript)
	env.commitAll(t)
	marker := filepath.Join(env.root, "mutant-seen")

	_, err := env.tool.Execute(context.Background(), lineMutants(t, env.file, 1))
	if err == nil {
		t.Fatal("a silent run over the budget must be refused")
	}
	for _, want := range []string{"refused before mutating anything", "did not ask for progress", "test_target"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal is missing %q:\n%v", want, err)
		}
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("the refused run applied a mutant and ran its tests before refusing")
	}
	if got := env.content(t); got != original {
		t.Fatalf("the refused run touched the file: got %q", got)
	}

	send, frames := servedMutationTool(t, env)
	send(mutationCall(1, lineMutants(t, env.file, 1), map[string]any{"progressToken": 1}))
	resp, _ := awaitResponse(t, frames, 1, 30*time.Second)
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Fatalf("with progress the run must go ahead and test the mutant:\n%s", resp)
	}
}

// TestMutationTest_SilentBudgetUsesTheMeasuredBaseline pins what the up-front
// check is fed, with a real budget rather than a 1 ns one: a 0.4 s compile per
// cycle, 10 mutants and a 3 s budget is ~4.5 s and must be refused, while 1
// mutant (~1 s) must run — about 2 s of margin either way for a loaded machine.
// Feeding it a wrong per-mutant cost or mutant count flips one of the two.
func TestMutationTest_SilentBudgetUsesTheMeasuredBaseline(t *testing.T) {
	withSilentBudget(t, 3*time.Second)
	env := newMutationEnv(t, lineFixture(10))
	env.installScript(t, env.compileScript, "sleep 0.4\nexit 0")
	env.installScript(t, env.testScript, seenMutantScript)
	env.commitAll(t)
	marker := filepath.Join(env.root, "mutant-seen")

	if _, err := env.tool.Execute(context.Background(), lineMutants(t, env.file, 10)); err == nil || !strings.Contains(err.Error(), "refused before mutating anything") {
		t.Fatalf("10 mutants at ~0.4 s each against a 3 s budget must be refused up front: %v", err)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("the refused run tested a mutant")
	}

	out, err := env.tool.Execute(context.Background(), lineMutants(t, env.file, 1))
	if err != nil {
		t.Fatalf("1 mutant at ~0.4 s fits a 3 s budget and must run: %v", err)
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Fatalf("the fitting run never tested its mutant:\n%s", out)
	}
}

// TestMutationTest_SilentRunStopsWhenAMutantCostsMoreThanTheBaseline: the
// baseline is only an estimate. Go's test cache can make the unmutated suite
// look nearly free while every mutant pays for the whole suite, so a run the
// up-front check let through can still outlast the budget. Here the baseline is
// instant and each mutant takes 1.2 s: the up-front check sees ~0.5 s against a
// 2 s budget and lets it through, but after the first mutant 3 more would take
// ~3.6 s, so the run stops, reports the one that ran and says why the rest did
// not. With progress the same run completes (the control).
func TestMutationTest_SilentRunStopsWhenAMutantCostsMoreThanTheBaseline(t *testing.T) {
	withSilentBudget(t, 2*time.Second)
	original := lineFixture(4)
	env := newMutationEnv(t, original)
	env.installScript(t, env.testScript, `grep -q '^b' "$(dirname "$0")/target.txt" && sleep 1.2
exit 0`)
	env.commitAll(t)

	out, err := env.tool.Execute(context.Background(), lineMutants(t, env.file, 4))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, want := range []string{"stopped early: 3 of 4 mutants never ran", "costliest compile+test cycle", "did not ask for progress"} {
		if !strings.Contains(out, want) {
			t.Errorf("report is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "b2") || strings.Contains(out, "cancelled:") {
		t.Errorf("only the first mutant may run, and the stop is not a cancellation:\n%s", out)
	}
	if got := env.content(t); got != original {
		t.Fatalf("file not restored: got %q", got)
	}

	send, frames := servedMutationTool(t, env)
	send(mutationCall(1, lineMutants(t, env.file, 4), map[string]any{"progressToken": "keep-alive"}))
	resp, _ := awaitResponse(t, frames, 1, 30*time.Second)
	if strings.Contains(resp, "stopped early") || !strings.Contains(resp, "b4") {
		t.Fatalf("with progress every mutant must run:\n%s", resp)
	}
}

// TestNewSilentRunWatch_SeedsTheBaseline: the projection starts from the
// measured baseline, so a first mutant costlier than the budget allows is
// caught before the second; and a call with no progress is the one it watches.
func TestNewSilentRunWatch_SeedsTheBaseline(t *testing.T) {
	withSilentBudget(t, 7*time.Minute)
	start := time.Now()
	w := newSilentRunWatch(context.Background(), start, 3*time.Minute)
	if !w.active || w.worst != 3*time.Minute || w.budget != 7*time.Minute || !w.start.Equal(start) {
		t.Fatalf("watch = %+v, want active, worst 3m (the baseline), budget 7m, the given start", *w)
	}
	if note := w.stopBefore(start, 0, 3); note == "" {
		t.Error("3 cycles at the baseline's 3m pass a 7m budget, so the watch must stop before the first")
	}
}

// TestSilentRunWatch_StopBefore pins the projection: inert with progress or
// nothing left, the time already spent counts, and the costliest cycle seen —
// not the baseline — is what the rest is projected from.
func TestSilentRunWatch_StopBefore(t *testing.T) {
	start := time.Now()
	now := start.Add(4 * time.Minute)
	newWatch := func(active bool) *silentRunWatch {
		return &silentRunWatch{active: active, start: start, budget: 25 * time.Minute, worst: time.Minute}
	}

	if note := newWatch(true).stopBefore(now, 1, 20); note != "" {
		t.Errorf("4m + 19 × 1m fits 25m, but it stopped: %s", note)
	}
	w := newWatch(true)
	w.observe(30 * time.Second) // cheaper than the baseline: the worst stays 1m
	if note := w.stopBefore(now, 1, 22); note != "" {
		t.Errorf("4m + 21 × 1m is exactly 25m and fits, but it stopped: %s", note)
	}
	if note := w.stopBefore(now, 1, 23); note == "" {
		t.Error("4m + 22 × 1m is past 25m and must stop")
	}
	w = newWatch(true)
	w.observe(6 * time.Minute)
	note := w.stopBefore(now, 1, 5)
	for _, want := range []string{"4 of 5 mutants never ran", "took 6m", "about 28m in all", "25m budget"} {
		if !strings.Contains(note, want) {
			t.Errorf("note is missing %q: %s", want, note)
		}
	}
	if note := w.stopBefore(now, 5, 5); note != "" {
		t.Errorf("nothing left to run, but it stopped: %s", note)
	}
	inactive := newWatch(false)
	inactive.observe(time.Hour)
	if note := inactive.stopBefore(now, 1, 20); note != "" {
		t.Errorf("a call with progress is never stopped, but it was: %s", note)
	}
	var none *silentRunWatch
	none.observe(time.Hour)
	if note := none.stopBefore(now, 0, 3); note != "" {
		t.Errorf("a nil watch must be inert: %s", note)
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
		{
			name: "over an hour keeps its minutes", elapsed: 2 * m, perMutant: 11 * m, mutants: 8, wantRefused: true,
			want: []string{"took 11m", "about 1h30m", "batches of at most 2"},
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
