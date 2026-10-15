package treesitter

import (
	"testing"

	tsg "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"
)

// TestSwift_HashTokenAfterStatement_GotreesitterParsesCleanly guards the Swift
// directive regression introduced in gotreesitter v0.54.0 and fixed in v0.56.0
// (upstream #1399). Swift still indexes through wasmts; these cases protect the
// pure-Go fallback without changing the WASM retirement gate.
func TestSwift_HashTokenAfterStatement_GotreesitterParsesCleanly(t *testing.T) {
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

	// A leading directive remains the control for the statement-prefix cases.
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
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			if hasError(t, src) {
				t.Fatal("a # token after a statement must parse without ERROR nodes")
			}
		})
	}
}
