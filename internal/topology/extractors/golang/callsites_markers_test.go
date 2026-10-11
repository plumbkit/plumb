package golang

import (
	"context"
	"testing"

	"github.com/plumbkit/plumb/internal/topology"
)

// sitesByCallee extracts src and returns each call site's qualifier by callee name.
func sitesByCallee(t *testing.T, src string) map[string][]string {
	t.Helper()
	_, _, sites, err := New().ExtractWithCallSites(context.Background(), "p/p.go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for _, s := range sites {
		if s.Kind == topology.CallSiteCall {
			out[s.Callee] = append(out[s.Callee], s.Qualifier)
		}
	}
	return out
}

// A call on a receiver with no name chain must not look like a bare call: an empty
// qualifier is what the same-package resolver reads as `F()`, and it linked
// `NewX(s).Execute()` to a package function named Execute.
func TestCallSites_AReceiverWithNoNameIsMarkedNotBare(t *testing.T) {
	sites := sitesByCallee(t, `package p

func Chain(r Reader, x any, m map[string]Item) {
	NewDecoder(r).Decode(nil)
	x.(*Item).Start()
	Item{}.Len()
	m["k"].Begin()
	(*ptrs[0]).Close()
	(*r).Read()
	Plain()
}
`)
	for callee, want := range map[string]string{
		"Decode": topology.QualifierOpaque,
		"Start":  topology.QualifierOpaque,
		"Len":    topology.QualifierOpaque,
		"Begin":  "m" + topology.QualifierIndexedSuffix,
		"Close":  "ptrs" + topology.QualifierIndexedSuffix, // a dereference does not hide the index
		"Read":   "r",                                      // control: a dereferenced named receiver keeps its name
		"Plain":  "",                                       // control: a bare call stays bare
	} {
		got := sites[callee]
		if len(got) != 1 || got[0] != want {
			t.Errorf("%s: qualifiers %q, want [%q]", callee, got, want)
		}
	}
}

// A bare call of a name the function declares itself is a call of that local
// value, not of a package function with the same name.
func TestCallSites_ABareCallOfALocalNameIsMarkedLocal(t *testing.T) {
	sites := sitesByCallee(t, `package p

func Walk(visit func()) (done func()) {
	walk := func() {}
	walk()
	visit()
	done()
	for _, step := range steps {
		step()
	}
	var each func()
	each()
	func(inner func()) { inner() }(nil)
	type count int
	_ = count(3)
	helper()
	return nil
}
`)
	for _, local := range []string{"walk", "visit", "done", "step", "each", "inner", "count"} {
		if got := sites[local]; len(got) != 1 || got[0] != topology.QualifierLocal {
			t.Errorf("%s: qualifiers %q, want [%q]", local, got, topology.QualifierLocal)
		}
	}
	if got := sites["helper"]; len(got) != 1 || got[0] != "" {
		t.Errorf("control: helper is not declared locally and must stay bare, got %q", got)
	}
}

// Locals do not leak from one function into the next.
func TestCallSites_LocalsAreTheEnclosingFunctionsOwn(t *testing.T) {
	sites := sitesByCallee(t, `package p

func A() { helper := func() {}; helper() }

func B() { helper() }
`)
	got := sites["helper"]
	if len(got) != 2 || got[0] != topology.QualifierLocal || got[1] != "" {
		t.Errorf("helper qualifiers %q, want [%q, \"\"] (local in A, bare in B)", got, topology.QualifierLocal)
	}
}
