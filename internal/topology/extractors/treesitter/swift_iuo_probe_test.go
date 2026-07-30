package treesitter

import (
	"context"
	"testing"

	tsg "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"

	"github.com/plumbkit/plumb/internal/topology"
)

// TestSwift_IUO_GotreesitterParsesCleanly is the inverted successor of the old
// TestSwift_IUO_GotreesitterStillBroken tripwire. The tripwire asserted the
// implicitly-unwrapped-optional (`var x: T!`) parse collapse was still present
// so the recoverIUOBangs byte-blanking workaround could not be dropped by
// mistake; gotreesitter v0.47.x fixed the underlying GLR bug, the workaround
// was retired, and this guard now pins the FIXED behaviour: the pinned grammar
// must parse an IUO property cleanly and keep the enclosing class intact. If a
// future gotreesitter change reintroduces the collapse, this fails loudly —
// the pure-Go fallback has no recovery shim any more.
func TestSwift_IUO_GotreesitterParsesCleanly(t *testing.T) {
	src := []byte("class VC {\n    var manager: Manager!\n    func go() {}\n}\n")
	lang := grammars.SwiftLanguage()
	tree, err := tsg.NewParser(lang).Parse(src)
	if err != nil || tree == nil {
		t.Fatalf("raw parse failed: err=%v tree=%v", err, tree)
	}
	defer tree.Release()
	root := tree.RootNode()

	if root.HasError() {
		t.Errorf("gotreesitter regressed on `var x: T!` — the IUO collapse is back; sexp: %s", root.SExpr(lang))
	}

	// An error-free parse is necessary but not sufficient: the bug's actual damage
	// was structural — the ERROR cascaded up and collapsed the enclosing class, so
	// the type AND every member vanished from the outline. Assert the extractor
	// still sees all three, or a partial regression would pass the check above.
	nodes, _, err := NewSwift().Extract(context.Background(), "VC.swift", src)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	want := []struct {
		kind topology.NodeKind
		name string
	}{
		{topology.KindClass, "VC"},
		{topology.KindVariable, "manager"},
		{topology.KindMethod, "go"},
	}
	for _, w := range want {
		found := false
		for _, n := range nodes {
			if n.Kind == w.kind && n.Name == w.name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s %q missing from the outline — the IUO collapse dropped the type or its members; nodes=%+v", w.kind, w.name, nodes)
		}
	}
}
