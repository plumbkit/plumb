package cli

import (
	"slices"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/collab"
)

// benchGateSamples bounds the retained latency samples; beyond it every k-th
// call is kept.
const benchGateSamples = 200_000

// benchGateCeiling is what "the gate is free" is allowed to mean. The gate is a
// mutex acquisition, a slice compare and a clock read — tens of nanoseconds —
// so a p50 anywhere near this means something with I/O in it has moved behind
// the gate, which is the whole regression this benchmark exists to catch. It is
// three orders of magnitude above the real cost on purpose: this must not go
// red because the machine was busy, only because the gate stopped being a gate.
const benchGateCeiling = 5 * time.Microsecond

// BenchmarkChatWatchGate_Idle pins the cost of the steady-state mailbox check —
// the notifier read plus the gate that decides not to touch SQLite. That is the
// whole claim the notifier makes for itself, so it gets both a number and an
// assertion: without the ceiling below, a gate that silently started querying
// would still pass, and the "drift is pinned" claim would be decoration.
//
// It also anchors internal/collab's mailprobe benchmark, which has to reproduce
// this gate as benchGate (chatWatch is unexported in a package that cannot
// reach collab's *sql.DB). Both files publish ns/op as the per-call p50 of
// individually timed calls, NOT Go's default wall/b.N — so the two headline
// columns are directly comparable, and a divergence between them is evidence
// the copy has drifted.
func BenchmarkChatWatchGate_Idle(b *testing.B) {
	n := collab.NewNotifier()
	keys := []string{"bench-sess", collab.NotifyKey(b.TempDir(), collab.AddresseeNext)}
	w := &chatWatch{}
	w.due(keys, n.Gens(keys), time.Now()) // prime the baseline and the backstop

	step := max((b.N+benchGateSamples-1)/benchGateSamples, 1)
	kept := make([]time.Duration, 0, min(b.N, benchGateSamples)+1)
	fired := 0
	for i := range b.N {
		t0 := time.Now()
		due := w.due(keys, n.Gens(keys), time.Now())
		d := time.Since(t0)
		if due {
			fired++
		}
		if i%step == 0 {
			kept = append(kept, d)
		}
	}
	b.StopTimer()
	if len(kept) == 0 {
		return
	}
	slices.Sort(kept)
	p50 := kept[len(kept)/2]
	b.ReportMetric(float64(p50.Nanoseconds()), "ns/op")
	b.ReportMetric(float64(len(kept)), "samples-n")

	if p50 > benchGateCeiling {
		b.Fatalf("gate p50 = %v, ceiling %v — the steady-state check is no longer free", p50, benchGateCeiling)
	}
	// Nothing was ever bumped, so the only legitimate reason to report due is
	// the 30s backstop. Anything more means the gate is not gating.
	if want := int(b.Elapsed()/chatFullCheckInterval) + 1; fired > want {
		b.Fatalf("gate reported due %d times in %s; at most %d expected from the backstop alone",
			fired, b.Elapsed(), want)
	}
}
