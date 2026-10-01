package tools

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/mcp"
)

// holdSlotScript makes the test step of the MUTATED run write a marker and then
// sleep, so a test can act while a run is provably mid-mutant. The baseline
// (unmutated) run passes straight through.
const holdSlotScript = `if grep -q '43' "$(dirname "$0")/target.txt"; then
    touch "$(dirname "$0")/test-started"
    sleep 30
fi
exit 0`

func oneMutant(t *testing.T, file string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"mutants": []map[string]any{{"file_path": file, "old_string": "42", "new_string": "43"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestMutationTest_RefusalNamesTheHolder: the second caller must be told WHO
// holds the slot, since when, and how far it has got. "Another run is in
// progress" alone left agents blocked for over an hour unable to tell a long run
// from a stuck one, or to find the session to ask.
func TestMutationTest_RefusalNamesTheHolder(t *testing.T) {
	env := newMutationEnv(t, "answer = 42\n")
	env.installScript(t, env.testScript, holdSlotScript)
	holder := env.tool.WithSession(
		func(context.Context) string { return "holder-bison" },
		func(context.Context) string { return "sess-holder-1" },
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := holder.Execute(ctx, oneMutant(t, env.file))
		done <- err
	}()
	t.Cleanup(func() { cancel(); <-done })
	waitForFile(t, filepath.Join(env.root, "test-started"))

	_, err := env.tool.Execute(context.Background(), oneMutant(t, env.file))
	if err == nil {
		t.Fatal("a second run while the first holds the slot must be refused")
	}
	msg := err.Error()
	for _, want := range []string{
		"already in progress", // the existing contract still holds
		"holder-bison",        // who
		"sess-holder-1",       // which exact session
		env.root,              // where
		"started",             // since when
		"mutant 1 of 1",       // how far
		"test command",        // doing what
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal is missing %q:\n%s", want, msg)
		}
	}
}

// TestMutationTest_SlotFreedWhenOwnerConnectionCloses drives the run through a
// real mcp.Server and then closes the client's side of the connection mid-mutant.
// The run must be abandoned — the mutant cancelled, the file restored, the slot
// released — rather than holding the daemon-wide slot until a 30 s (in
// production: up to 20 mutants × two 600 s steps) run completes for a result
// nobody can receive.
func TestMutationTest_SlotFreedWhenOwnerConnectionCloses(t *testing.T) {
	const original = "answer = 42\n"
	env := newMutationEnv(t, original)
	env.installScript(t, env.testScript, holdSlotScript)

	srv := mcp.New(mcp.ServerInfo{Name: "t", Version: "0"})
	srv.Register(env.tool)
	pr, pw := io.Pipe()
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = srv.Serve(context.Background(), pr, io.Discard)
	}()
	req, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "mutation_test", "arguments": oneMutant(t, env.file)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pw.Write(append(req, '\n')); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, filepath.Join(env.root, "test-started"))

	_ = pw.Close() // the owning agent's connection goes away mid-run
	select {
	case <-served: // Serve returns only once the in-flight run has returned
	case <-time.After(10 * time.Second):
		t.Fatal("the run kept going after its connection closed — the slot stays held for a result nobody can receive")
	}
	if got := env.content(t); got != original {
		t.Fatalf("file not restored after the owner went away: got %q, want %q", got, original)
	}
	if h, held := mutationRun.snapshot(); held {
		t.Fatalf("slot still held after the owner went away: %+v", h)
	}
}

// TestMutationTest_CancelledRunStartsNoFurtherMutant: once the request is
// cancelled mid-mutant, the next mutant must not be written to disk — its
// commands would be refused at start and misreported as a tooling fault — and
// the report must say that the rest never ran rather than read as complete.
func TestMutationTest_CancelledRunStartsNoFurtherMutant(t *testing.T) {
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := env.tool.Execute(ctx, raw)
		done <- result{out, err}
	}()
	waitForFile(t, filepath.Join(env.root, "test-started"))
	cancel()

	res := <-done
	if res.err != nil {
		t.Fatalf("run: %v", res.err)
	}
	if strings.Contains(res.out, "other = 8") {
		t.Fatalf("the second mutant ran after the request was cancelled:\n%s", res.out)
	}
	if !strings.Contains(res.out, "1 of 2 mutants never ran") {
		t.Fatalf("the report must say the second mutant never ran:\n%s", res.out)
	}
	if got := env.content(t); got != original {
		t.Fatalf("file not restored: got %q, want %q", got, original)
	}
}

// TestMutationHolder_Describe pins the refusal's progress phrasing for each
// phase, including the ones a live run passes through too quickly to observe.
func TestMutationHolder_Describe(t *testing.T) {
	now := time.Now()
	base := mutationHolder{session: "amber-owl", sessionID: "s-1", workspace: "/w", started: now.Add(-3 * time.Minute), mutants: 4}
	cases := []struct {
		name string
		h    func() mutationHolder
		want []string
	}{
		{"preflight", func() mutationHolder { h := base; h.step = stepPreflight; return h }, []string{"amber-owl", "s-1", "/w", "started 3m ago", "checking its mutants"}},
		{"baseline", func() mutationHolder { h := base; h.step = stepCompile; return h }, []string{"unmutated baseline", "compile gate", "4 mutants queued"}},
		{"mutant", func() mutationHolder { h := base; h.current, h.step = 2, stepTest; return h }, []string{"mutant 2 of 4", "test command"}},
		{"anonymous", func() mutationHolder { h := base; h.session, h.sessionID = "", ""; h.step = stepPreflight; return h }, []string{"an unidentified session"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.h().describe(now)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("describe() = %q, missing %q", got, w)
				}
			}
		})
	}
}
