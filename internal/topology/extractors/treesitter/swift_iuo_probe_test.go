package treesitter

import (
	"testing"

	tsg "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"
)

// TestSwift_IUO_GotreesitterStillBroken is a tripwire. Swift is now extracted via
// the canonical grammar compiled to WASM (internal/topology/extractors/wasmts);
// the gotreesitter Swift extractor in this package — and its recoverIUOBangs
// byte-blanking workaround — survive ONLY as the wasm init-failure fallback.
//
// This probe parses an implicitly-unwrapped optional type directly through the
// pure-Go gotreesitter grammar with NO recovery. It asserts the bug is STILL
// present (the parse errors / the class collapses). When a future gotreesitter
// release fixes it, this assertion flips and fails — the signal that the
// recoverIUOBangs workaround AND this gotreesitter Swift fallback can be removed.
// See docs/internal/treesitter-plan.md and the todo.md follow-ups.
func TestSwift_IUO_GotreesitterStillBroken(t *testing.T) {
	src := []byte("class VC {\n    var manager: Manager!\n    func go() {}\n}\n")
	lang := grammars.SwiftLanguage()
	tree, err := tsg.NewParser(lang).Parse(src)
	if err != nil || tree == nil {
		t.Fatalf("raw parse failed: err=%v tree=%v", err, tree)
	}
	defer tree.Release()
	root := tree.RootNode()

	if !root.HasError() {
		t.Errorf("gotreesitter now parses `var x: T!` without error — drop recoverIUOBangs "+
			"and the gotreesitter Swift fallback; sexp: %s", root.SExpr(lang))
	}
}
