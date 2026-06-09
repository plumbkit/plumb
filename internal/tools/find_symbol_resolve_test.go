package tools

import (
	"testing"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

func docSymNames(syms []protocol.DocumentSymbol) []string {
	out := make([]string, len(syms))
	for i, s := range syms {
		out[i] = s.Name
	}
	return out
}

// TestResolveSymbolsByName_GoFlatMethods covers the gopls shape: Go methods are
// top-level symbols named "(*Recv).Method" / "(Recv).Method", never nested under
// the receiver type. A "ReceiverType.MethodName" query must still resolve them.
func TestResolveSymbolsByName_GoFlatMethods(t *testing.T) {
	syms := []protocol.DocumentSymbol{
		{Name: "daemonInfo", Kind: protocol.SKStruct, Children: []protocol.DocumentSymbol{
			{Name: "sessID", Kind: protocol.SKField},
		}},
		{Name: "(*daemonInfo).Execute", Kind: protocol.SKMethod},
		{Name: "(WriteDeps).postWriteDiagWindow", Kind: protocol.SKMethod},
		{Name: "NewDaemonInfo", Kind: protocol.SKFunction},
	}

	cases := []struct {
		query string
		want  string // expected single match Name; "" = expect no match
	}{
		{"daemonInfo.Execute", "(*daemonInfo).Execute"},                      // pointer receiver, plain dotted query
		{"(*daemonInfo).Execute", "(*daemonInfo).Execute"},                   // exact gopls name
		{"WriteDeps.postWriteDiagWindow", "(WriteDeps).postWriteDiagWindow"}, // value receiver
		{"daemonInfo.Missing", ""},                                           // wrong method
		{"Other.Execute", ""},                                                // wrong receiver
	}
	for _, c := range cases {
		got := resolveSymbolsByName(syms, c.query)
		if c.want == "" {
			if len(got) != 0 {
				t.Errorf("%q: expected no match, got %v", c.query, docSymNames(got))
			}
			continue
		}
		if len(got) != 1 || got[0].Name != c.want {
			t.Errorf("%q: got %v, want [%s]", c.query, docSymNames(got), c.want)
		}
	}
}

// TestResolveSymbolsByName_NestedMethodsStillResolve guards the nested shape
// (Python/Java/tree-sitter), where the method is a child of the type symbol.
func TestResolveSymbolsByName_NestedMethodsStillResolve(t *testing.T) {
	syms := []protocol.DocumentSymbol{
		{Name: "Greeter", Kind: protocol.SKClass, Children: []protocol.DocumentSymbol{
			{Name: "greet", Kind: protocol.SKMethod},
		}},
	}
	got := resolveSymbolsByName(syms, "Greeter.greet")
	if len(got) != 1 || got[0].Name != "greet" {
		t.Fatalf("nested dotted lookup broke: got %v", docSymNames(got))
	}
}
