package wasmts

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A wasm parse cannot be interrupted (newRuntime explains why the interruptible
// wazero mode is not worth its measured cost), so the contract these tests pin
// is narrower but the one that matters: a context that is already dead starts
// no parse, an expired deadline ends the WAIT, and the extractor recovers
// rather than staying wedged behind the abandoned parse. Both tests hold the
// runtime's parse lock so the select inside Extract has exactly one reachable
// arm — without the wedge, a fast parse can race the closed ctx.Done() and the
// outcome depends on the scheduler (the source of a real CI flake).

const tsFixture = `export class Widget { run(a: number): string { return String(a); } }`

// TestExtract_ExpiredContextReturnsPromptly: a dead context returns its error
// without touching the runtime. The wedge makes both worlds deterministic:
// with the entry guard the call never reaches the runtime; without it the
// parse goroutine parks on the held lock, the select abandons, and the warm
// runtime is discarded — which the structural assert catches.
func TestExtract_ExpiredContextReturnsPromptly(t *testing.T) {
	ex := NewTypeScript()
	want, _, err := ex.Extract(context.Background(), "warm.ts", []byte(tsFixture))
	if err != nil {
		t.Fatalf("warmup Extract: %v", err)
	}
	if len(want) == 0 {
		t.Fatal("warmup extracted no symbols; fixture is wrong")
	}

	ex.mu.Lock()
	warm := ex.rt
	ex.mu.Unlock()
	if warm == nil {
		t.Fatal("no runtime after warmup; the extractor fell back")
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	warm.mu.Lock()
	if _, _, err := ex.Extract(cancelled, "x.ts", []byte(tsFixture)); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	ex.mu.Lock()
	still := ex.rt
	ex.mu.Unlock()
	warm.mu.Unlock()
	if still != warm {
		t.Fatal("a dead context cost the extractor its runtime — the call started (and abandoned) a parse")
	}

	// The runtime it kept is still the one doing the work.
	got, _, err := ex.Extract(context.Background(), "y.ts", []byte(tsFixture))
	if err != nil {
		t.Fatalf("Extract after the cancelled call: %v", err)
	}
	if len(got) != len(want) {
		t.Errorf("got %d symbols after the cancelled call, want %d", len(got), len(want))
	}
}

// TestExtract_AbandonedParseDoesNotBlockTheNextExtract is the mutation pin for
// discard — the lock-convoy regression guard. Holding the runtime's parse lock
// for the rest of the test is a faithful stand-in for a parse that never
// returns: rt.parse takes rt.mu for its whole duration, so a later file sees
// exactly this — a lock it cannot have. With the discard the next Extract
// builds a fresh runtime and finishes; without it that Extract queues behind
// the wedged one and never returns.
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

	// The deadline must be alive at Extract's entry (a dead context starts no
	// parse and would skip the discard this test exists to pin) and generous
	// enough that it cannot expire between its creation and that entry check
	// even on a stalled runner; the parse goroutine then parks on stuck.mu and
	// never reaches the send, so the deadline arm is the only way out.
	wedged, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, _, err := ex.Extract(wedged, "wedged.ts", []byte(tsFixture)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected the wedged parse to be abandoned at its deadline, got %v", err)
	}

	// Structural fast-fail in front of the behavioural pin: the discard must
	// have dropped the wedged runtime. This fails in milliseconds where the
	// liveness pin below takes its full budget to time out — and it keeps the
	// test honest if rt.parse ever stops holding rt.mu for its whole duration,
	// where the liveness pin alone would pass on the old runtime vacuously.
	ex.mu.Lock()
	still := ex.rt
	ex.mu.Unlock()
	if still == stuck {
		t.Fatal("the abandoned parse's runtime was not discarded")
	}

	// The pin: stuck.mu is still held, so this can only succeed on a runtime the
	// extractor built after dropping the wedged one. The budget is far longer
	// than the sibling test's because this window contains a whole wasm rebuild
	// (seconds under -race on a shared runner), and it is measuring "finishes"
	// against "never finishes" rather than timing anything: a generous budget
	// costs nothing on the happy path and cannot mask the regression, which
	// blocks forever.
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

// TestExtract_DiscardReArmsTheFallbackWarning pins the warned reset in
// discard: the fallback warning is a latch (one log per lifetime), but a
// discard starts a new lifetime — a rebuild that then fails must get its own
// warning rather than hiding behind one spent on the pre-discard runtime.
// Without the reset the latch stays spent and the permanent fallback is silent.
func TestExtract_DiscardReArmsTheFallbackWarning(t *testing.T) {
	ex := NewTypeScript()
	if _, _, err := ex.Extract(context.Background(), "warm.ts", []byte(tsFixture)); err != nil {
		t.Fatalf("warmup Extract: %v", err)
	}

	ex.mu.Lock()
	rt := ex.rt
	ex.mu.Unlock()
	if rt == nil {
		t.Fatal("no runtime after warmup; the extractor fell back")
	}

	ex.warned.Store(true) // as a pre-discard init failure or parse fault would leave it
	ex.discard(rt)
	if ex.warned.Load() {
		t.Fatal("discard did not re-arm the fallback warning — a failed rebuild would fall back silently")
	}
}
