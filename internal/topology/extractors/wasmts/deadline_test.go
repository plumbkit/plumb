package wasmts

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A wasm parse cannot be interrupted (newRuntime explains why the interruptible
// wazero mode is not worth its measured cost), so the contract these tests pin
// is narrower but the one that matters: an expired deadline ends the WAIT, and
// the extractor recovers rather than staying wedged behind the abandoned parse.

const tsFixture = `export class Widget { run(a: number): string { return String(a); } }`

func TestExtract_ExpiredContextReturnsPromptly(t *testing.T) {
	ex := NewTypeScript()
	// Warm the runtime first so the expired-context path is not just measuring
	// wasm compilation.
	if _, _, err := ex.Extract(context.Background(), "warm.ts", []byte(tsFixture)); err != nil {
		t.Fatalf("warmup Extract: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already done before the call

	done := make(chan error, 1)
	go func() {
		_, _, err := ex.Extract(ctx, "x.ts", []byte(tsFixture))
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Extract did not return on a cancelled context")
	}
}

// TestExtract_RecoversAfterAnAbandonedParse is the self-heal guard: dropping the
// runtime on abandonment is what stops one overrunning file from serialising
// every later file behind its lock. Without the discard this still passes only
// because the abandoned parse here actually completes; the assertion that earns
// its keep is that extraction is intact and unchanged afterwards.
func TestExtract_RecoversAfterAnAbandonedParse(t *testing.T) {
	ex := NewTypeScript()
	want, _, err := ex.Extract(context.Background(), "a.ts", []byte(tsFixture))
	if err != nil {
		t.Fatalf("baseline Extract: %v", err)
	}
	if len(want) == 0 {
		t.Fatal("baseline extracted no symbols; fixture is wrong")
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := ex.Extract(cancelled, "a.ts", []byte(tsFixture)); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected the cancelled parse to be abandoned, got %v", err)
	}

	got, _, err := ex.Extract(context.Background(), "a.ts", []byte(tsFixture))
	if err != nil {
		t.Fatalf("Extract after an abandoned parse: %v — the extractor did not recover", err)
	}
	if len(got) != len(want) {
		t.Errorf("got %d symbols after recovery, want %d — the rebuilt runtime is not equivalent", len(got), len(want))
	}
}

// TestExtract_AbandonedParseDoesNotBlockTheNextExtract is the mutation pin for
// discard — the lock-convoy regression guard. The test above cannot be it: its
// abandoned parse actually completes, so the runtime frees itself and the test
// stays green with the discard deleted. Here the wedge is real. Holding the
// runtime's parse lock for the rest of the test is a faithful stand-in for a
// parse that never returns: rt.parse takes rt.mu for its whole duration, so a
// later file sees exactly this — a lock it cannot have. With the discard the
// next Extract builds a fresh runtime and finishes; without it that Extract
// queues behind the wedged one and never returns.
func TestExtract_AbandonedParseDoesNotBlockTheNextExtract(t *testing.T) {
	ex := NewTypeScript()
	want, _, err := ex.Extract(context.Background(), "warm.ts", []byte(tsFixture))
	if err != nil {
		t.Fatalf("warmup Extract: %v", err)
	}
	if len(want) == 0 {
		t.Fatal("warmup extracted no symbols; fixture is wrong")
	}

	ex.mu.Lock()
	stuck := ex.rt
	ex.mu.Unlock()
	if stuck == nil {
		t.Fatal("no runtime after warmup; the extractor fell back")
	}
	stuck.mu.Lock()
	defer stuck.mu.Unlock()

	// The parse goroutine parks on stuck.mu and never reaches the send, so the
	// select can only take ctx.Done — the branch that discards the runtime.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := ex.Extract(cancelled, "wedged.ts", []byte(tsFixture)); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected the wedged parse to be abandoned, got %v", err)
	}

	// The pin: stuck.mu is still held, so this can only succeed on a runtime the
	// extractor built after dropping the wedged one. The budget is far longer
	// than the 5s the tests above use because this window contains a whole wasm
	// rebuild (seconds under -race on a shared runner), and it is measuring
	// "finishes" against "never finishes" rather than timing anything: a
	// generous budget costs nothing on the happy path and cannot mask the
	// regression, which blocks forever.
	type result struct {
		n   int
		err error
	}
	next := make(chan result, 1)
	go func() {
		nodes, _, err := ex.Extract(context.Background(), "next.ts", []byte(tsFixture))
		next <- result{n: len(nodes), err: err}
	}()

	select {
	case res := <-next:
		if res.err != nil {
			t.Fatalf("Extract after an abandoned parse: %v — the extractor did not recover", res.err)
		}
		if res.n != len(want) {
			t.Errorf("got %d symbols from the rebuilt runtime, want %d — it is not equivalent", res.n, len(want))
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the next Extract serialised behind the stuck runtime's lock: the abandoned parse was not discarded")
	}
}
