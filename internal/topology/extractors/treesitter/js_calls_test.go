package treesitter

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/topology"
)

func render(sites []topology.CallSite, nodes []topology.Node) []string {
	out := make([]string, 0, len(sites))
	for _, s := range sites {
		out = append(out, siteText(s, nodes))
	}
	return out
}

func TestTypeScriptAndJavaScriptCallSitesAndBindings(t *testing.T) {
	src := `import def, { a as b, C } from "./m";
import * as ns from "./n";
const r = require("./r");

export function top() {
  b();
  ns.f();
  def();
  r.g();
  console.log(1);
  new C();
  local.m();
}

export const arrow = () => ns.h();

class A {
  run() { return this.go(); }
  go() { return 1; }
}
class B {
  run() { return this.stop(); }
}
`
	want := []string{
		"bind:def=./m#default", "bind:b=./m#a", "bind:C=./m#C", "bind:ns=./n#*", "bind:r=./r#",
		"call:b@top", "call:ns.f@top", "call:def@top", "call:r.g@top", "call:C@top",
		"call:ns.h@arrow", "call:this.go@A.run", "call:this.stop@B.run",
	}
	ts, _, tsSites, err := NewTypeScript().ExtractWithCallSites(context.Background(), "src/x.ts", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	js, _, jsSites, err := NewJavaScript().ExtractWithCallSites(context.Background(), "src/x.js", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	for lang, got := range map[string][]string{"typescript": render(tsSites, ts), "javascript": render(jsSites, js)} {
		if !slices.Equal(got, want) {
			t.Errorf("%s sites:\n got  %s\n want %s", lang, strings.Join(got, " "), strings.Join(want, " "))
		}
	}
}
