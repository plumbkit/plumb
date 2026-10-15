package treesitter

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/topology"
)

// siteText renders a site as kind:qualifier.callee@enclosing (or =spec#imported
// for a binding), for comparing a file's sites in one line each.
func siteText(s topology.CallSite, nodes []topology.Node) string {
	if s.Kind == topology.CallSiteBinding {
		return "bind:" + s.Callee + "=" + s.Qualifier + "#" + s.FirstStringArg
	}
	enc := "-"
	if s.EnclosingIdx >= 0 && s.EnclosingIdx < len(nodes) {
		enc = nodes[s.EnclosingIdx].Qualified
	}
	q := s.Callee
	if s.Qualifier != "" {
		q = s.Qualifier + "." + s.Callee
	}
	return string(s.Kind) + ":" + q + "@" + enc
}

func TestPythonCallSitesAndBindings(t *testing.T) {
	src := []byte(`import a.b
import c as d
from .e import f as g, h
from x import *


def top():
    print(1)
    g()
    a.b.f()
    d.z()
    h()
    obj.m()


class A:
    def run(self):
        return self.go()

    def go(self):
        def inner():
            return h()
        return inner()


class B:
    def run(self):
        return self.stop()
`)
	nodes, _, sites, err := NewPython().ExtractWithCallSites(context.Background(), "pkg/m.py", src)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(sites))
	for _, s := range sites {
		got = append(got, siteText(s, nodes))
	}
	want := []string{
		"bind:a.b=a.b#", "bind:d=c#", "bind:g=.e#f", "bind:h=.e#h",
		"call:g@top", "call:a.b.f@top", "call:d.z@top", "call:h@top",
		"call:self.go@A.run", "call:h@A.go.inner", "call:self.stop@B.run",
	}
	if !slices.Equal(got, want) {
		t.Errorf("sites:\n got  %s\n want %s", strings.Join(got, " "), strings.Join(want, " "))
	}
	quals := map[string]bool{}
	for _, n := range nodes {
		quals[n.Qualified] = true
	}
	for _, q := range []string{"A.run", "B.run", "A.go", "A.go.inner", "top", "A"} {
		if !quals[q] {
			t.Errorf("no node qualified %q (have %v)", q, quals)
		}
	}
}
