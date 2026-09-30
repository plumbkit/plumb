package treesitter

import (
	"testing"

	tsg "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"
)

// TestSwift_HashTokenAfterStatement_GotreesitterStillBroken is a tripwire for a
// gotreesitter regression that arrived with v0.54.0's tree-sitter-swift bump
// (upstream PR #1164): a `#`-prefixed token that follows a statement parses to
// ERROR. v0.53.0 parsed every case below cleanly, and so does canonical
// tree-sitter-swift at the commit gotreesitter pins (00bbb0a2550f), so this is
// a port defect rather than a grammar gap.
//
// Production Swift indexes through wasmts, so this only reaches users through
// the pure-Go fallback. It is one of the things keeping that WASM path alive
// (PLAN-1). When this test FAILS because every case parses cleanly, upstream has
// fixed it: invert it into a ParsesCleanly guard and re-run the PLAN-1 parity
// sweep.
func TestSwift_HashTokenAfterStatement_GotreesitterStillBroken(t *testing.T) {
	lang := grammars.SwiftLanguage()
	hasError := func(t *testing.T, src string) bool {
		t.Helper()
		tree, err := tsg.NewParser(lang).Parse([]byte(src))
		if err != nil || tree == nil {
			t.Fatalf("raw parse failed: err=%v tree=%v", err, tree)
		}
		defer tree.Release()
		return tree.RootNode().HasError()
	}

	// Positive control: a directive with nothing before it parses cleanly, so
	// the failures below are about what precedes the `#`, not about directives
	// being unparseable.
	if hasError(t, "#if swift(>=5.11)\nimport B\n#endif\n") {
		t.Fatal("control regressed: a leading #if no longer parses cleanly, so this probe no longer isolates the bug")
	}

	cases := map[string]string{
		"warning after let":  "let x = 1\n#warning(\"w\")\n",
		"if after let":       "let x = 1\n#if compiler(>=5.9)\nlet y = 2\n#endif\n",
		"else after import":  "#if FOO\nimport A\n#else\nimport B\n#endif\n",
		"Preview after type": "struct A {}\n#Preview {\n  Text(\"hi\")\n}\n",
		"expect in function": "func f() {\n  let x = 1\n  #expect(x == 1)\n}\n",
	}
	fixed := 0
	for name, src := range cases {
		if !hasError(t, src) {
			t.Logf("%s: now parses cleanly", name)
			fixed++
		}
	}
	if fixed == len(cases) {
		t.Fatal("gotreesitter now parses a `#` token after a statement cleanly — invert this tripwire and re-run the PLAN-1 parity sweep")
	}
	if fixed > 0 {
		t.Errorf("%d of %d cases now parse cleanly — a partial upstream fix; update this tripwire and re-check PLAN-1", fixed, len(cases))
	}
}
