package tools

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// context_affected_named_wiring_test.go — the named tests' order end to end, over
// a real index, so each piece of the gather that decides it is pinned where it is
// wired: the walk from a neighbour starting one hop out, the aside mark for a
// neighbour the seed only calls, and which neighbours count as callers.

// namedFixture is one package around Seed. Callers of Seed: B, X. Its callees:
// helperA, helperB (aside: their tests never run Seed). Each name is chosen so
// that a wrong hop count or a wrong kind reorders the line.
//
//	TestDirect   -> Seed                    1 hop
//	TestATwo     -> X -> Seed               2 hops; the walk from the caller X would say 1
//	                                        if a neighbour did not start one hop out
//	TestADeep    -> A -> B -> Seed          3 hops: past the walk's depth from Seed,
//	                                        reached only through the caller B
//	TestBBoth    -> A -> B -> Seed, and -> helperA: runs Seed, 3 hops
//	TestACallees -> f -> helperA (3), and -> helperB (2): aside, and the shorter
//	                                        path is found second (helperA walks first)
//	TestBHelper  -> helperA                 aside, 2 hops
const namedFixtureSeed = `package seed

func Seed() int { return helperA() + helperB() }

func helperA() int { return 1 }

func helperB() int { return 1 }

func f() int { return helperA() }

func B() int { return Seed() }

func A() int { return B() }

func X() int { return Seed() }
`

const namedFixtureTests = `package seed

import "testing"

func TestDirect(t *testing.T)   { Seed() }
func TestATwo(t *testing.T)     { X() }
func TestADeep(t *testing.T)    { A() }
func TestBBoth(t *testing.T)    { A(); helperA() }
func TestACallees(t *testing.T) { f(); helperB() }
func TestBHelper(t *testing.T)  { helperA() }
`

func TestContextForTask_Affected_NamedTestsRunTheSeedFirstAndClosestFirst(t *testing.T) {
	root := t.TempDir()
	for name, body := range map[string]string{
		"go.mod":            "module example.com/fixture\n\ngo 1.22\n",
		"seed/seed.go":      namedFixtureSeed,
		"seed/seed_test.go": namedFixtureTests,
	} {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s := newShopTool(t, openContextStore(t, root, 2), root)
	pack := s.collect(t, map[string]any{"symbols": []string{"seed/seed.go#Seed"}, "intent": contextIntentChange, "max_bytes": 32000})
	got := pack.Affected.Named["seed"]
	want := []string{"TestDirect", "TestATwo", "TestADeep", "TestBBoth", "TestACallees", "TestBHelper"}
	if !slices.Equal(got.Names, want) {
		t.Errorf("named = %q, want %q: tests that run Seed first and closest first, then the tests of Seed's "+
			"callees, closest first (see namedFixture)", got.Names, want)
	}
}
