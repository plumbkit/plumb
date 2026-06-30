package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/langsupport"
)

// TestDocToolCountMatchesRegistry guards the human-written tool count in the
// README, docs, and website against drift from the code. The count is derived
// from conn_register.go — the single place every MCP tool is wired up — and must
// appear in every doc/site location that restates it. It has drifted before (the
// site said 50 and the docs said 51 while the registry already held 53), so this
// test goes red the moment a tool is added or removed without updating the prose,
// forcing them to move together. Mirrors TestServerJSONVersionMatchesVERSION.
func TestDocToolCountMatchesRegistry(t *testing.T) {
	root := repoRootFromCaller(t)
	n := countToolRegistrations(t, root)

	checks := []struct {
		path   string
		needle string
	}{
		{"README.md", fmt.Sprintf("**%d tools**", n)},
		{"docs/tools.md", fmt.Sprintf("**%d** structured tools", n)},
		{"docs/architecture.md", fmt.Sprintf("(%d tools —", n)},
		{"docs/token-efficiency.md", fmt.Sprintf("same %d tools", n)},
		{"docs/index.md", fmt.Sprintf("the %d tools", n)},
		{"site/index.html", fmt.Sprintf(`data-count="%d">0</div><div class="l">structured tools`, n)},
		{"site/index.html", fmt.Sprintf("%s structured tools", titleWord(n))},
	}
	for _, c := range checks {
		assertFileContains(t, root, c.path, c.needle, n)
	}
}

// TestLanguageAndClientSourceCountsPinned pins the source-of-truth counts for
// supported languages and `plumb setup` clients. The website restates these as
// exact figures, but the displayed numbers are editorial: the language stat folds
// the .tsx alias into TypeScript (registry 18 → shown 17). The client stat now
// shows all 13 setup targets (the two Antigravity entries are listed separately).
// Encoding display rules in code would be brittle, so this test pins
// the source counts instead — change a count and CI goes red here, pointing at
// the exact display strings to revisit.
func TestLanguageAndClientSourceCountsPinned(t *testing.T) {
	const (
		wantLanguages = 18 // langsupport registry entries; site shows 17 (.tsx folds into TypeScript), README says "15+"
		wantClients   = 13 // plumb setup targets; site shows 13 ("Thirteen agents")
	)
	if got := len(langsupport.All()); got != wantLanguages {
		t.Errorf("langsupport registry has %d entries, pinned at %d.\n"+
			"If intended, update the website's \"languages & formats\" stat "+
			"(site/index.html — currently 17, the registry minus the .tsx alias) and README's "+
			"\"15+\" tier table, then bump wantLanguages.", got, wantLanguages)
	}
	if got := len(allSetupClients()); got != wantClients {
		t.Errorf("plumb has %d setup clients, pinned at %d.\n"+
			"If intended, update the website's client count (site/index.html — the \"AI clients\" "+
			"stat and the \"Thirteen agents\" heading/chips, currently 13) "+
			"and the docs/cli-reference.md + AGENTS.md setup tables, then bump wantClients.", got, wantClients)
	}
}

// countToolRegistrations counts the MCP tools wired up in conn_register.go via
// its uniform `srv.Register(tools.…)` calls — the prompt and resource
// registrations use different prefixes and are not counted.
func countToolRegistrations(t *testing.T, root string) int {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(root, "internal", "cli", "conn_register.go"))
	if err != nil {
		t.Fatalf("reading conn_register.go: %v", err)
	}
	n := strings.Count(string(src), "srv.Register(tools.")
	if n == 0 {
		t.Fatal("found no srv.Register(tools.…) calls in conn_register.go — the counting heuristic is broken")
	}
	return n
}

func assertFileContains(t *testing.T, root, rel, needle string, n int) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("reading %s: %v", rel, err)
	}
	if !strings.Contains(string(b), needle) {
		t.Errorf("%s does not state the current tool count (%d).\n"+
			"Expected to find: %q\n"+
			"conn_register.go now registers %d tools — update this file to match.",
			rel, n, needle, n)
	}
}

// titleWord renders n as a capitalised English word ("Fifty-three"), matching the
// website's prose. Falls back to digits outside [0,99].
func titleWord(n int) string {
	w := numberToWords(n)
	if w == "" {
		return w
	}
	return strings.ToUpper(w[:1]) + w[1:]
}

func numberToWords(n int) string {
	ones := []string{
		"zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine",
		"ten", "eleven", "twelve", "thirteen", "fourteen", "fifteen", "sixteen", "seventeen", "eighteen", "nineteen",
	}
	tens := []string{"", "", "twenty", "thirty", "forty", "fifty", "sixty", "seventy", "eighty", "ninety"}
	switch {
	case n < 0 || n >= 100:
		return fmt.Sprintf("%d", n)
	case n < 20:
		return ones[n]
	default:
		w := tens[n/10]
		if r := n % 10; r != 0 {
			w += "-" + ones[r]
		}
		return w
	}
}
