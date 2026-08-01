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
