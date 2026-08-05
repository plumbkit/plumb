package treesitter

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tsg "github.com/odvcencio/gotreesitter"

	"github.com/plumbkit/plumb/internal/topology"
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

func TestExtractWith_MidParseDeadlineReturnsError(t *testing.T) {
	// The middle of the contract: the deadline is still ahead when Extract is
	// called, so the remaining <= 0 guard passes and the parse starts — but
	// the budget handed to SetTimeoutMicros is far smaller than the parse
	// needs. gotreesitter reports that stop as a PARTIAL tree with a nil
	// error; extractWith must turn it into an error, because walking the
	// partial tree would record a truncated symbol set as the whole file.
	//
	// The deadline must clear the one-off per-Extract costs — lazy grammar
	// load and parser construction, which run BEFORE extractWith computes the
	// remaining budget (measured: ~15ms, ~70ms under -race) — while staying
	// far below the full parse time of the source (~3.5s for ~1.8MB, ~35s
	// under -race). 500ms against ~1.8MB of source leaves a wide margin on
	// both sides on any machine.
	var src strings.Builder
	for i := range 50000 {
		fmt.Fprintf(&src, "def f%d(x):\n    return x + %d\n\n", i, i)
	}

	// Warm the lazy grammar up before the clock starts, or the one-off
	// grammar load would itself burn the budget and trip the guard.
	ex := NewPython()
	if _, _, err := ex.Extract(context.Background(), "warm.py", []byte("def w():\n    pass\n")); err != nil {
		t.Fatalf("warm-up Extract: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	nodes, edges, err := ex.Extract(ctx, "big.py", []byte(src.String()))
	if err == nil || !strings.Contains(err.Error(), "parse stopped early") {
		t.Errorf("err = %v, want a \"parse stopped early\" error for a parse the budget cut short", err)
	}
	if nodes != nil || edges != nil {
		t.Error("expected no nodes/edges from a parse that stopped early — a partial symbol set must not be returned")
	}
}

func TestExtractWith_CancelledContextDoesNotParse(t *testing.T) {
	// The other half of the dead-context contract. A cancelled context usually
	// carries no deadline at all, so the budget check above cannot see it: the
	// envelope parsed the file to completion and handed back the symbols of a
	// file its caller had already abandoned. The walk sentinel is what
	// discriminates — the parse runs before it, so a walk that never ran means
	// no parse was started.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	walked := false
	nodes, edges, err := extractWith(ctx, NewPython().lang.get(), []byte("def f():\n    pass\n"),
		func(*tsg.Node) ([]topology.Node, []topology.Edge) {
			walked = true
			return []topology.Node{{Name: "f"}}, nil
		})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled — a dead context must not start a parse", err)
	}
	if walked {
		t.Error("the walk ran, so the parse did too: a cancelled context started work")
	}
	if nodes != nil || edges != nil {
		t.Error("expected no nodes/edges from a cancelled extract")
	}
}
