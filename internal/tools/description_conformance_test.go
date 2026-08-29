package tools

import (
	"testing"

	"github.com/plumbkit/plumb/internal/clientcaps"
)

// TestDescriptionConformance is the per-client generalisation of
// TestDescriptionRuneCeiling (PLAN-370). That test pins every description
// under one hand-copied constant (maxDescriptionChars, itself a Claude-Code
// measurement with headroom); this one renders every registered tool's
// description and checks it against EVERY client in clientcaps' registry,
// using each client's own measured DescriptionCapRunes — so a future client
// row is covered automatically, with no second constant to remember to
// update.
//
// "Rendered form, not the source constant" (the PLAN-370 trap): as of this
// writing internal/mcp/server_handlers.go's snapshotTools deliberately calls
// Tool.Description() under a lock that forbids any client-aware rendering —
// its own doc comment states "Every description in the tree is a fixed
// string today" and that varying a description by client would have to be
// "hoisted out to registration time", which no tool does. So today
// tl.Description() already IS the exact bytes tools/list sends to every
// client; there is no per-client suffix layered on top of it to route around.
// This test still renders through Description() rather than a source
// constant, both because that is the only form available and so that this
// test does not silently stop covering a future per-client rendering path
// without a deliberate re-check of this comment.
//
// A client with no measured DescriptionCapRunes (0) is checked against
// clientcaps.StrictestDescriptionCapRunes() — the smallest cap measured for
// ANY client — rather than skipped: "unmeasured" is not "uncapped", and
// skipping an unmeasured client would silently narrow this test's coverage
// back down to the single client that happens to have been measured.
func TestDescriptionConformance(t *testing.T) {
	strictest := clientcaps.StrictestDescriptionCapRunes()
	if strictest == 0 {
		t.Fatal("clientcaps.StrictestDescriptionCapRunes() = 0 — no client has a measured DescriptionCapRunes, so this test has no cap to enforce")
	}

	allTools := append(leanToolSet(), nonLeanToolSet()...)
	overflows := 0
	for _, c := range clientcaps.All() {
		budget := c.DescriptionCapRunes
		unmeasured := budget == 0
		if unmeasured {
			budget = strictest
		}
		for _, tl := range allTools {
			n := len([]rune(tl.Description()))
			if n <= budget {
				continue
			}
			overflows++
			basis := "measured cap"
			if unmeasured {
				basis = "strictest known cap — DescriptionCapRunes unmeasured for this client"
			}
			t.Errorf("%s description is %d runes, over %s's %d-rune %s by %d — a client at or under this cap will silently truncate the tail before the model sees it",
				tl.Name(), n, c.Name, budget, basis, n-budget)
		}
	}
	if overflows > 0 {
		t.Logf("%d tool+client overflow(s) found across %d clients and %d tools", overflows, len(clientcaps.All()), len(allTools))
	}
}
