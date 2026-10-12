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

// namedFixture is one package around Seed:
//
//	TestDirect  -> Seed                      1 hop
//	TestZTwo    -> X -> Seed                 2 hops (X is a caller of Seed)
//	TestADeep   -> A -> B -> Seed            3 hops: past the walk's depth from Seed,
//	                                         reached only through the caller B
//	TestAHelper -> helper <- Seed            Seed's callee: never runs Seed (aside)
//	TestBBoth   -> helper, and -> A -> B -> Seed: reached both ways, it keeps
//	                                         the path that runs Seed (3 hops)
const namedFixtureSeed = `package seed

func Seed() int { return helper() + 1 }

func helper() int { return 1 }

func B() int { return Seed() }

func A() int { return B() }

func X() int { return Seed() }
`

const namedFixtureTests = `package seed

import "testing"

func TestDirect(t *testing.T)  { Seed() }
func TestZTwo(t *testing.T)    { X() }
func TestADeep(t *testing.T)   { A() }
func TestAHelper(t *testing.T) { helper() }
func TestBBoth(t *testing.T)   { helper(); A() }
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
	want := []string{"TestDirect", "TestZTwo", "TestADeep", "TestBBoth", "TestAHelper"}
	if !slices.Equal(got.Names, want) {
		t.Errorf("named = %q, want %q: tests that run Seed first, closest first (TestADeep is three hops, "+
			"reached through the caller B, and TestBBoth by its path through B), and the test of Seed's callee last", got.Names, want)
	}
}
