package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/langsupport"
	"github.com/plumbkit/plumb/internal/tools"
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
		{"site/index.html", titleWord(n) + " structured tools"},
	}
	for _, c := range checks {
		assertFileContains(t, root, c.path, c.needle, n)
	}

	// The lean profile's REMAINDER — the schemas a lean client stops paying for
	// — is restated in prose too, and drifted on its own: the 62 → 57 fold
	// updated AGENTS.md and missed docs/configuration.md, which went on claiming
	// 41 against a real 36. It is derived, not independent: registry minus the
	// lean set.
	remainder := n - len(tools.LeanToolNames())
	needle := fmt.Sprintf("(%d tools today", remainder)
	for _, rel := range []string{"AGENTS.md", "docs/configuration.md"} {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("reading %s: %v", rel, err)
		}
		if !strings.Contains(string(b), needle) {
			t.Errorf("%s does not state the current non-lean remainder (%d = %d registered - %d lean).\n"+
				"Expected to find: %q", rel, remainder, n, len(tools.LeanToolNames()), needle)
		}
	}
}

// TestAgentsSkillListMatchesEmbedded guards AGENTS.md's prose enumeration of the
// installed skills against the embedded set. The set itself is pinned by
// TestEmbeddedSkills_HaveValidFrontmatter, but nothing tied it to the sentence
// that counts and names them — so adding a skill desynchronised the brief
// silently, exactly the drift TestDocToolCountMatchesRegistry exists to stop.
func TestAgentsSkillListMatchesEmbedded(t *testing.T) {
	root := repoRootFromCaller(t)
	skills := embeddedSkills()
	if len(skills) == 0 {
		t.Fatal("embeddedSkills() returned nothing — the embed is broken")
	}

	b, err := os.ReadFile(filepath.Join(root, "AGENTS.md"))
	if err != nil {
		t.Fatalf("reading AGENTS.md: %v", err)
	}
	brief := string(b)

	countPhrase := fmt.Sprintf("installs %s idempotent user-scoped skills", numberToWords(len(skills)))
	start := strings.Index(brief, countPhrase)
	if start < 0 {
		t.Fatalf("AGENTS.md does not state the current skill count (%d).\nExpected to find: %q",
			len(skills), countPhrase)
	}
	// Scope the name check to the paragraph that makes the claim. Searching the
	// whole brief would pass on a name that had drifted into an unrelated
	// section, which is the drift this test exists to catch.
	para := brief[start:]
	if end := strings.Index(para, "\n\n"); end >= 0 {
		para = para[:end]
	}
	for _, s := range skills {
		if needle := "`" + s.Name + "`"; !strings.Contains(para, needle) {
			t.Errorf("AGENTS.md's setup paragraph does not name the embedded skill %q — expected %s "+
				"alongside the count, not merely somewhere in the file", s.Name, needle)
		}
	}
}

// TestLanguageAndClientSourceCountsPinned pins the source-of-truth counts for
// supported languages and `plumb setup` clients. The website restates these as
// exact figures, but the displayed numbers are editorial: the language stat folds
// the .tsx alias into TypeScript (supported 18 → shown 17). The client stat now
// shows all 14 setup targets (the two Antigravity entries are listed separately).
// Encoding display rules in code would be brittle, so this test pins
// the source counts instead — change a count and CI goes red here, pointing at
// the exact display strings to revisit.
//
// The language pin counts SUPPORTED rows, not registry length: the registry also
// carries EngineNone rows for languages plumb recognises but does not index, and
// those must never inflate a count the website presents as capability. The
// uncovered pin moves in the opposite direction — it drops by one each time the
// coverage programme wires a language — so a PR that adds an extractor without
// flipping its row, or flips a row without shipping an extractor, goes red here
// as well as in TestBuildExtractorsCoversRegistry.
func TestLanguageAndClientSourceCountsPinned(t *testing.T) {
	const (
		wantLanguages = 30 // indexed languages; site shows 28 (.tsx folds into TypeScript), README says "15+"
		wantUncovered = 2  // langsupport rows recognised but not yet indexed — decreases as extractors land
		wantClients   = 14 // plumb setup targets; site shows 14 ("Fourteen agents")
	)
	var supported int
	for _, l := range langsupport.All() {
		if l.Structural != langsupport.EngineNone {
			supported++
		}
	}
	if supported != wantLanguages {
		t.Errorf("langsupport has %d indexed languages, pinned at %d.\n"+
			"If intended, update the website's \"languages & formats\" stat "+
			"(site/index.html — currently 17, the supported set minus the .tsx alias) and README's "+
			"\"15+\" tier table, then bump wantLanguages.", supported, wantLanguages)
	}
	if got := len(langsupport.Uncovered()); got != wantUncovered {
		t.Errorf("langsupport has %d uncovered languages, pinned at %d.\n"+
			"Wiring a language should move a row from uncovered to indexed, so both "+
			"counts change together; adding a newly-recognised language raises this one alone.",
			got, wantUncovered)
	}
	if got := len(allSetupClients()); got != wantClients {
		t.Errorf("plumb has %d setup clients, pinned at %d.\n"+
			"If intended, update the website's client count (site/index.html — the \"AI clients\" "+
			"stat and the \"Fourteen agents\" heading/chips, currently 14) "+
			"and the docs/cli-reference.md setup table, then bump wantClients.", got, wantClients)
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
		return strconv.Itoa(n)
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
