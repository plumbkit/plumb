package treesitter

import (
	"testing"

	tsg "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"
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
}
