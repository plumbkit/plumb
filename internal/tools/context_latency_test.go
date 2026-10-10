package tools

import (
	"cmp"
	"os"
	"slices"
	"strconv"
	"testing"
	"time"
)

// context_latency_test.go — how long a structural pack takes on the shop fixture
// (PLAN-462 A5). The target is a warm p95 under 500 ms; the figures are LOGGED, and the
// only thing asserted is the call's own 2 s deadline, because a timing assertion
// tighter than the machine's worst day is a flaky test, not a guard. Run
//
//	go test ./internal/tools -run 'TestContextForTask_(Warm|Cold)Latency' -v -count=1
//
// to read them; PLUMB_CONTEXT_LATENCY_CALLS raises the sample size per request shape.

// latencyTarget is the gate's warm structural p95 target.
const latencyTarget = 500 * time.Millisecond

// latencyShapes are the requests the figures are taken over: one symbol, one file, two
// symbols with prose and the change intent, and a seed in the largest file (whose body
// the source-read cap refuses), so the p95 is not the p95 of the easiest call.
var latencyShapes = []map[string]any{
	{"symbols": []string{"cart/cart.go#Cart.Total"}},
	{"files": []string{"cart/cart.go"}},
	{"symbols": []string{"cart/cart.go#Cart.Total", "pricing/discount.go#Apply"}, "task": "fix the discount rounding", "intent": "change"},
	{"symbols": []string{"reports/reports.go#Summarise"}},
}

// quantile is the q-th quantile (0..1) of ds by the nearest-rank rule.
func quantile(ds []time.Duration, q float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	sorted := slices.Clone(ds)
	slices.Sort(sorted)
	rank := int(float64(len(sorted))*q+0.999999) - 1
	return sorted[min(max(rank, 0), len(sorted)-1)]
}

func latencyCalls() int {
	if n, err := strconv.Atoi(os.Getenv("PLUMB_CONTEXT_LATENCY_CALLS")); err == nil && n > 0 {
		return n
	}
	return 50
}

// timeCall runs one pack and returns how long it took.
func timeCall(t *testing.T, s shopTool, args map[string]any) time.Duration {
	t.Helper()
	start := time.Now()
	if _, err := s.run(t, args); err != nil {
		t.Fatalf("%v: %v", args, err)
	}
	return time.Since(start)
}

func TestQuantile_NearestRank(t *testing.T) {
	ds := make([]time.Duration, 0, 100)
	for i := 100; i >= 1; i-- { // unsorted on purpose
		ds = append(ds, time.Duration(i)*time.Millisecond)
	}
	for _, tc := range []struct {
		q    float64
		want time.Duration
	}{{0.5, 50 * time.Millisecond}, {0.95, 95 * time.Millisecond}, {1, 100 * time.Millisecond}, {0, time.Millisecond}} {
		if got := quantile(ds, tc.q); got != tc.want {
			t.Errorf("quantile(%v) = %s, want %s", tc.q, got, tc.want)
		}
	}
	if ds[0] != 100*time.Millisecond {
		t.Error("quantile sorted its input in place")
	}
}

// The warm structural p95 on the shop fixture, over every request shape, with no
// language server wired (the figure the gate's latency rule is about: LSP, cold and
// timeout tails are reported apart).
func TestContextForTask_WarmLatency(t *testing.T) {
	s := newShop(t)
	for _, args := range latencyShapes { // warm: the index, the page cache and the code paths
		timeCall(t, s, args)
		timeCall(t, s, args)
	}
	calls := latencyCalls()
	all := make([]time.Duration, 0, calls*len(latencyShapes))
	for _, args := range latencyShapes {
		shape := make([]time.Duration, 0, calls)
		for range calls {
			shape = append(shape, timeCall(t, s, args))
		}
		t.Logf("warm %-4d calls p50 %-8s p95 %-8s max %-8s  %v", len(shape),
			quantile(shape, 0.5).Round(time.Microsecond), quantile(shape, 0.95).Round(time.Microsecond),
			slices.MaxFunc(shape, cmp.Compare[time.Duration]).Round(time.Microsecond), args)
		all = append(all, shape...)
	}
	p95 := quantile(all, 0.95)
	verdict := "within"
	if p95 >= latencyTarget {
		verdict = "OVER"
	}
	t.Logf("warm structural p95 = %s over %d calls (%s the %s target; p50 %s)",
		p95.Round(time.Microsecond), len(all), verdict, latencyTarget, quantile(all, 0.5).Round(time.Microsecond))
	if p95 >= contextExpansionDeadline {
		t.Errorf("warm p95 %s reached the call's own %s deadline, so the walk is being cut on a fixture it should finish", p95, contextExpansionDeadline)
	}
}

// The first call on a fresh index, per request shape. Within a test binary the process
// is already warm, so for the process-cold figure run this test alone with -count=1.
func TestContextForTask_ColdLatency(t *testing.T) {
	for _, args := range latencyShapes {
		s := newShop(t)
		d := timeCall(t, s, args)
		t.Logf("cold first call on a fresh index: %-8s  %v", d.Round(time.Microsecond), args)
		if d >= contextExpansionDeadline {
			t.Errorf("a first call took %s, past the call's own %s deadline", d, contextExpansionDeadline)
		}
	}
}
