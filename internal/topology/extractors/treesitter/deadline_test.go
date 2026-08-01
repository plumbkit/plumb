package treesitter

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The extractors are the pure-Go gotreesitter path, whose GLR error recovery can
// go superlinear on a file the size caps happily admit (upstream
// odvcencio/gotreesitter#576). extractWith therefore hands the context's
// remaining budget to the parser; these tests pin the two ends of that contract.

func TestExtractWith_ExpiredContextDoesNotParse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond) // ensure the deadline is behind us

	nodes, edges, err := NewPython().Extract(ctx, "a.py", []byte("def f():\n    pass\n"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded — an expired budget must not start a parse", err)
	}
	if nodes != nil || edges != nil {
		t.Error("expected no nodes/edges when the deadline has already passed")
	}
}

func TestExtractWith_NoDeadlineParsesNormally(t *testing.T) {
	// context.Background() has no deadline: the parser must run unbounded, which
	// is what every existing caller and the disabled-timeout config rely on.
	nodes, _, err := NewPython().Extract(context.Background(), "a.py", []byte("def f():\n    pass\n"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(nodes) == 0 {
		t.Error("expected symbols from a well-formed file parsed without a deadline")
	}
}

func TestExtractWith_AmpleDeadlineParsesNormally(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	nodes, _, err := NewPython().Extract(ctx, "a.py", []byte("def f():\n    pass\n"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(nodes) == 0 {
		t.Error("expected symbols — a deadline the parse fits inside must not change the result")
	}
}
