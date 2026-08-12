package cli

import (
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/collab"
)

// BenchmarkChatWatchGate_Idle pins the cost of the steady-state mailbox check —
// the notifier read plus the gate that decides not to touch SQLite. It is the
// whole claim the notifier makes for itself, so it is worth a number of its own.
//
// It also anchors internal/collab's mailprobe benchmark, which has to reproduce
// this gate (chatWatch is unexported in a package that cannot reach collab's
// *sql.DB). If the two diverge, the v3-notifier figures there stop meaning what
// they say.
func BenchmarkChatWatchGate_Idle(b *testing.B) {
	n := collab.NewNotifier()
	keys := []string{"bench-sess", collab.NotifyKey(b.TempDir(), collab.AddresseeNext)}
	w := &chatWatch{}
	w.due(keys, n.Gens(keys), time.Now()) // prime the baseline and the backstop

	fired := 0
	for b.Loop() {
		if w.due(keys, n.Gens(keys), time.Now()) {
			fired++
		}
	}
	// Nothing was ever bumped, so the only legitimate reason to report due is
	// the 30s backstop. Anything more means the gate is not gating.
	if want := int(b.Elapsed()/chatFullCheckInterval) + 1; fired > want {
		b.Fatalf("gate reported due %d times in %s; at most %d expected from the backstop alone",
			fired, b.Elapsed(), want)
	}
}
