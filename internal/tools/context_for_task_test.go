package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/toolerror"
)

// requireErr fails unless err is non-nil and mentions every fragment.
func requireErr(t *testing.T, err error, fragments ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want an error mentioning %q, got none", fragments)
	}
	for _, f := range fragments {
		if !strings.Contains(err.Error(), f) {
			t.Errorf("error %q does not mention %q", err, f)
		}
	}
}

// requireKind fails unless err carries the given toolerror classification.
func requireKind(t *testing.T, err error, want toolerror.Kind) {
	t.Helper()
	te, ok := toolerror.Classify(err)
	if !ok {
		t.Fatalf("error %q carries no classification, want %s", err, want)
	}
	if te.Kind != want {
		t.Errorf("error kind = %s, want %s (%q)", te.Kind, want, err)
	}
}

func seedPaths(p contextPack) []string {
	out := make([]string, 0, len(p.Seeds))
	for _, s := range p.Seeds {
		out = append(out, s.Path)
	}
	return out
}

func candidatePaths(m contextMiss) []string {
	out := make([]string, 0, len(m.Candidates))
	for _, c := range m.Candidates {
		out = append(out, c.Path)
	}
	slices.Sort(out)
	return out
}

func TestContextForTask_Schema(t *testing.T) {
	var schema struct {
		Properties           map[string]json.RawMessage `json:"properties"`
		AdditionalProperties bool                       `json:"additionalProperties"`
	}
	tool := NewContextForTask(nil)
	if err := json.Unmarshal(tool.InputSchema(), &schema); err != nil {
		t.Fatalf("schema is not JSON: %v", err)
	}
	got := make([]string, 0, len(schema.Properties))
	for k := range schema.Properties {
		got = append(got, k)
	}
	slices.Sort(got)
	want := []string{"corpora", "files", "have", "intent", "max_bytes", "symbols", "task", "within"}
	if !slices.Equal(got, want) {
		t.Errorf("schema properties = %v, want %v", got, want)
	}
	if schema.AdditionalProperties {
		t.Error("schema must set additionalProperties:false")
	}
	// These are the argument names the daemon reads a connection pin from
	// (cli.seedPathFromArgs). A scope glob in `paths` would seed the pin from a
	// glob, so none of them may be a property of this tool.
	for _, pinSeed := range []string{"uri", "file_path", "path", "root", "workspace", "paths", "operations"} {
		if _, bad := schema.Properties[pinSeed]; bad {
			t.Errorf("schema declares %q, an argument the daemon reads a workspace pin from", pinSeed)
		}
	}
}

func TestContextForTask_DescriptionStatesTheContract(t *testing.T) {
	desc := strings.ToLower(NewContextForTask(nil).Description())
	for _, must := range []string{
		"experimental", "read-only", "at least one file or symbol", "never seeds",
		"1024-byte reserve", "whole response", "workspace_search", "unpinned call is refused",
	} {
		if !strings.Contains(desc, must) {
			t.Errorf("description does not say %q", must)
		}
	}
}

func TestContextForTask_WireBytesWithinTarget(t *testing.T) {
	const target = 2000
	tool := NewContextForTask(nil)
	entry, err := json.Marshal(toolDef{Name: tool.Name(), Description: tool.Description(), InputSchema: tool.InputSchema()})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("context_for_task tools/list entry: %d bytes", len(entry))
	if len(entry) > target {
		t.Errorf("tools/list entry is %d bytes, over the %d-byte target for this tool", len(entry), target)
	}
}

func TestContextForTask_UnpinnedUnleanAndCollectorTracksNoReads(t *testing.T) {
	if IsPinned("context_for_task") {
		t.Error("context_for_task must not be in PinnedTools: it ships unpinned")
	}
	if slices.Contains(LeanToolNames(), "context_for_task") {
		t.Error("context_for_task must not be in LeanTools: a lean client reports it unavailable")
	}
	// Gathering a pack never records a read, so the collector may not be able to:
	// no field of it mentions the read tracker. Recording belongs to the tool, and
	// only for bodies it delivered. The positive controls prove the detector sees
	// a tracker where there is one.
	for _, typ := range []reflect.Type{reflect.TypeFor[ReadFile](), reflect.TypeFor[ContextForTask]()} {
		if len(readTrackerFields(typ)) == 0 {
			t.Fatalf("control: %s holds a read tracker, but the detector found none", typ.Name())
		}
	}
	if fields := readTrackerFields(reflect.TypeFor[ContextCollector]()); len(fields) > 0 {
		t.Errorf("ContextCollector has read-tracker field(s) %v; collecting a pack must never record a read", fields)
	}
}

func readTrackerFields(typ reflect.Type) []string {
	var out []string
	for f := range typ.Fields() {
		if strings.Contains(f.Type.String(), "ReadTracker") {
			out = append(out, f.Name)
		}
	}
	return out
}

// C03: a bare selector that matches two declarations is ambiguous. Both are
// offered and neither becomes a seed.
func TestContextForTask_AmbiguousSelectorChoosesNothing(t *testing.T) {
	s := newShop(t)
	pack := s.collect(t, map[string]any{"symbols": []string{"Total"}})
	if len(pack.Seeds) != 0 {
		t.Fatalf("an ambiguous selector produced seeds %v; none may be chosen", seedPaths(pack))
	}
	if len(pack.Misses) != 1 || !pack.Misses[0].Ambiguous {
		t.Fatalf("want exactly one ambiguous miss, got %+v", pack.Misses)
	}
	if got, want := candidatePaths(pack.Misses[0]), []string{"cart/cart.go", "store/ledger.go"}; !slices.Equal(got, want) {
		t.Errorf("candidates = %v, want %v", got, want)
	}
	out, err := s.run(t, map[string]any{"symbols": []string{"Total"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"seeds (0 resolved, 1 unresolved)", "none was chosen",
		"cart/cart.go#(*Cart).Total", "store/ledger.go#(*Ledger).Total",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "read_symbol") {
		t.Errorf("an unresolved selector must not offer a read_symbol next call:\n%s", out)
	}
}

func TestContextForTask_AmbiguousCandidatesAreBounded(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		writeFileT(t, root, name+".go", "package big\n\nfunc Dup() {}\n")
	}
	s := newShopTool(t, openContextStore(t, root, 7), root)
	pack := s.collect(t, map[string]any{"symbols": []string{"Dup"}})
	if len(pack.Misses) != 1 || !pack.Misses[0].Ambiguous {
		t.Fatalf("want one ambiguous miss, got %+v", pack.Misses)
	}
	m := pack.Misses[0]
	if len(m.Candidates) != contextMaxCandidates || m.More != 2 {
		t.Errorf("candidates shown = %d, more = %d; want %d shown and 2 more", len(m.Candidates), m.More, contextMaxCandidates)
	}
	out, _ := s.run(t, map[string]any{"symbols": []string{"Dup"}})
	if !strings.Contains(out, "… and 2 more") {
		t.Errorf("the elided candidates are not counted in the output:\n%s", out)
	}
}

// C04: the three spellings of one Go method resolve to the same node.
func TestContextForTask_ReceiverFormsResolveToTheSameNode(t *testing.T) {
	s := newShop(t)
	var first contextSeed
	for i, form := range []string{"cart/cart.go#(*Cart).Add", "cart/cart.go#Cart.Add", "cart/cart.go#*Cart.Add"} {
		pack := s.collect(t, map[string]any{"symbols": []string{form}})
		if len(pack.Seeds) != 1 || len(pack.Misses) != 0 {
			t.Fatalf("%q: want exactly one seed, got seeds=%v misses=%+v", form, seedPaths(pack), pack.Misses)
		}
		got := pack.Seeds[0]
		if got.Selector != "(*Cart).Add" || got.Path != "cart/cart.go" || got.Line != 20 {
			t.Errorf("%q resolved to %+v", form, got)
		}
		if i == 0 {
			first = got
		} else if got != first {
			t.Errorf("%q resolved to %+v, but the first form resolved to %+v", form, got, first)
		}
	}
	// Control: a different method must not resolve to the same seed, or the
	// loop above would pass for any constant answer.
	other := s.collect(t, map[string]any{"symbols": []string{"cart/cart.go#Cart.Remove"}})
	if len(other.Seeds) != 1 || other.Seeds[0] == first {
		t.Errorf("Cart.Remove resolved to %+v; it must be a different seed from Cart.Add", other.Seeds)
	}
}

// C12: a selector the index does not know resolves to nothing, says so, and
// invents no seed.
func TestContextForTask_UnknownSelectorInventsNothing(t *testing.T) {
	s := newShop(t)
	args := map[string]any{"symbols": []string{"cart/cart.go#Cart.Coupon"}, "task": "Make Cart.Coupon case-insensitive."}
	pack := s.collect(t, args)
	if len(pack.Seeds) != 0 {
		t.Fatalf("an unknown selector produced seeds %v", seedPaths(pack))
	}
	if len(pack.Misses) != 1 || pack.Misses[0].Ambiguous || len(pack.Misses[0].Candidates) != 0 {
		t.Fatalf("want one plain unresolved miss without candidates, got %+v", pack.Misses)
	}
	if !strings.Contains(pack.Misses[0].Reason, "nothing was invented") {
		t.Errorf("reason %q does not say nothing was invented", pack.Misses[0].Reason)
	}
	out, err := s.run(t, args)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "\n  symbol ") || strings.Contains(out, "next:") {
		t.Errorf("an unresolved selector rendered as a seed or offered next calls:\n%s", out)
	}
}

// A selector that exists, but not in the file the caller named, is a labelled
// no-match: its other homes are candidates, never seeds.
func TestContextForTask_SelectorElsewhereIsACandidateNotASeed(t *testing.T) {
	s := newShop(t)
	pack := s.collect(t, map[string]any{"symbols": []string{"pricing/discount.go#Total"}})
	if len(pack.Seeds) != 0 {
		t.Fatalf("a path-hint mismatch produced seeds %v", seedPaths(pack))
	}
	if len(pack.Misses) != 1 {
		t.Fatalf("want one miss, got %+v", pack.Misses)
	}
	m := pack.Misses[0]
	if m.Ambiguous {
		t.Error("a mismatch against the path hint is a no-match, not an ambiguity")
	}
	if !strings.Contains(m.Reason, "candidates, not seeds") {
		t.Errorf("reason %q does not label the candidates as non-seeds", m.Reason)
	}
	if got, want := candidatePaths(m), []string{"cart/cart.go", "store/ledger.go"}; !slices.Equal(got, want) {
		t.Errorf("candidates = %v, want %v", got, want)
	}
}

// C14: a file the index has no extractor for is a coverage gap, never an empty
// file.
func TestContextForTask_UncoveredFilesAreLabelledNotEmpty(t *testing.T) {
	s := newShop(t)
	for _, tc := range []struct {
		file, coverage, next string
	}{
		{"web/CartBadge.svelte", "svelte is not covered by an extractor: symbols and relations are unknown, not absent", "read_file"},
		{"go.mod", "unrecognised file type", "read_file"},
		{"cart/cart.go", "", "file_outline"},
		{"scripts/export.py", "", "file_outline"},
	} {
		pack := s.collect(t, map[string]any{"files": []string{tc.file}})
		if len(pack.Seeds) != 1 {
			t.Fatalf("%s: want one seed, got seeds=%v misses=%+v", tc.file, seedPaths(pack), pack.Misses)
		}
		if !strings.Contains(pack.Seeds[0].Coverage, tc.coverage) || (tc.coverage == "") != (pack.Seeds[0].Coverage == "") {
			t.Errorf("%s: coverage = %q, want %q", tc.file, pack.Seeds[0].Coverage, tc.coverage)
		}
		out, err := s.run(t, map[string]any{"files": []string{tc.file}})
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(out, "coverage gap"); got != (tc.coverage != "") {
			t.Errorf("%s: output mentions a coverage gap = %v, want %v:\n%s", tc.file, got, tc.coverage != "", out)
		}
		if !strings.Contains(out, "\n  "+tc.next+" {") {
			t.Errorf("%s: want a %s next call:\n%s", tc.file, tc.next, out)
		}
		if strings.Contains(out, "no symbols") {
			t.Errorf("%s: the output claims the file has no symbols:\n%s", tc.file, out)
		}
	}
}

func TestContextForTask_NextCallsAreExactAndAbsolute(t *testing.T) {
	s := newShop(t)
	out, err := s.run(t, map[string]any{"files": []string{"cart/cart.go"}, "symbols": []string{"cart/cart.go#Cart.Add"}})
	if err != nil {
		t.Fatal(err)
	}
	abs := filepath.Join(canonicalRoot(s.root), "cart", "cart.go")
	want := `file_outline {"uri":` + string(quotedJSON(t, abs)) + `}`
	if !strings.Contains(out, "\n  "+want) {
		t.Errorf("output lacks the exact next call %s:\n%s", want, out)
	}
	// A delivered body is its own answer: it offers no read_symbol call for the
	// same declaration.
	if strings.Contains(out, "read_symbol") {
		t.Errorf("a delivered body must not also offer a read_symbol next call:\n%s", out)
	}
}

func quotedJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestContextForTask_Refusals(t *testing.T) {
	s := newShop(t)
	outside := filepath.Join(t.TempDir(), "outside.go")
	if err := os.WriteFile(outside, []byte("package outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name     string
		args     map[string]any
		contains []string
		kind     toolerror.Kind // expected classification; empty skips the check
		boundary bool
	}{
		{"no seeds", map[string]any{}, []string{"at least one file or symbol", "workspace_search"}, toolerror.KindInvalidArguments, false},
		{"prose alone", map[string]any{"task": "fix the cart"}, []string{"at least one file or symbol", "workspace_search"}, toolerror.KindInvalidArguments, false},
		{"empty file entry", map[string]any{"files": []string{" "}}, []string{"files[0] is empty"}, toolerror.KindInvalidArguments, false},
		{"relative directory", map[string]any{"files": []string{"cart"}}, []string{"a directory is scope, not a seed"}, toolerror.KindInvalidArguments, false},
		{"absolute directory", map[string]any{"files": []string{s.root}}, []string{"a directory is scope, not a seed"}, toolerror.KindInvalidArguments, false},
		{"document selector", map[string]any{"symbols": []string{"docs/pricing.md#Totals"}}, []string{"names a document", "corpora"}, toolerror.KindInvalidArguments, false},
		{"missing selector", map[string]any{"symbols": []string{"cart/cart.go#"}}, []string{"no selector after '#'"}, toolerror.KindInvalidArguments, false},
		{"outside the workspace", map[string]any{"files": []string{outside}}, []string{"different project"}, "", true},
		{"outside by relative climb", map[string]any{"files": []string{"../outside.go"}}, []string{"different project"}, "", true},
		{"outside symbol path", map[string]any{"symbols": []string{outside + "#Foo"}}, []string{"different project"}, "", true},
		{"absolute with dotdot", map[string]any{"files": []string{s.root + "/cart/../cart/cart.go"}}, []string{"canonical form"}, toolerror.KindWorkspaceBoundary, true},
		{"bad intent", map[string]any{"files": []string{"cart/cart.go"}, "intent": "refactor"}, []string{"intent must be"}, toolerror.KindInvalidArguments, false},
		{"bad corpus", map[string]any{"files": []string{"cart/cart.go"}, "corpora": []string{"web"}}, []string{"not one of code, docs, memory"}, toolerror.KindInvalidArguments, false},
		{"budget below the reserve", map[string]any{"files": []string{"cart/cart.go"}, "max_bytes": 1024}, []string{"below the minimum 1536"}, toolerror.KindInvalidArguments, false},
		{"negative budget", map[string]any{"files": []string{"cart/cart.go"}, "max_bytes": -5}, []string{"below the minimum"}, toolerror.KindInvalidArguments, false},
		{"within outside the root", map[string]any{"files": []string{"cart/cart.go"}, "within": []string{outside}}, []string{"outside the workspace root"}, toolerror.KindInvalidArguments, false},
		{"within bad glob", map[string]any{"files": []string{"cart/cart.go"}, "within": []string{"[x"}}, []string{"not a valid glob"}, toolerror.KindInvalidArguments, false},
		{"have without a symbol", map[string]any{"files": []string{"cart/cart.go"}, "have": []map[string]string{{"symbol": "", "content_sha256": strings.Repeat("a", 64)}}}, []string{"have[0].symbol is empty"}, toolerror.KindInvalidArguments, false},
		{"have with a short sha", map[string]any{"files": []string{"cart/cart.go"}, "have": []map[string]string{{"symbol": "x", "content_sha256": "abc"}}}, []string{"64 hex characters"}, toolerror.KindInvalidArguments, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := s.run(t, tc.args)
			if err == nil {
				t.Fatalf("want a refusal, got output:\n%s", out)
			}
			requireErr(t, err, tc.contains...)
			if tc.kind != "" {
				requireKind(t, err, tc.kind)
			}
			if tc.boundary && !IsWorkspaceBoundaryError(err) {
				t.Errorf("want a workspace-boundary refusal, got %v", err)
			}
		})
	}
}

// An unpinned caller is told to pin; the tool resolves nothing and attaches
// nothing, even for an absolute path that exists.
func TestContextForTask_UnpinnedCallerIsRefusedWithAHandoff(t *testing.T) {
	root := copyShopFixture(t)
	abs := filepath.Join(root, "cart", "cart.go")
	for name, ws := range map[string]WorkspaceFn{
		"empty workspace": func(context.Context) string { return "" },
		"no workspace fn": nil,
	} {
		t.Run(name, func(t *testing.T) {
			col := NewContextCollector(nil).WithWorkspace(ws).WithBoundary(testBoundaryGuard(root))
			raw, _ := json.Marshal(map[string]any{"files": []string{abs}})
			out, err := NewContextForTask(col).Execute(context.Background(), raw)
			if err == nil {
				t.Fatalf("an unpinned call produced output:\n%s", out)
			}
			requireErr(t, err, "session_start")
			if !IsWorkspaceBoundaryError(err) {
				t.Errorf("want an unattached-workspace refusal, got %v", err)
			}
		})
	}
}

func TestContextForTask_ContestedPinRefusesRelativePathsButNotAbsoluteOnes(t *testing.T) {
	root := copyShopFixture(t)
	col := NewContextCollector(nil).
		WithWorkspace(func(context.Context) string { return root }).
		WithBoundary(testBoundaryGuard(root)).
		WithContested(func() bool { return true })
	tool := NewContextForTask(col)
	rel, _ := json.Marshal(map[string]any{"files": []string{"cart/cart.go"}})
	_, err := tool.Execute(context.Background(), rel)
	requireErr(t, err, "contested")
	abs, _ := json.Marshal(map[string]any{"files": []string{filepath.Join(root, "cart", "cart.go")}})
	if out, err := tool.Execute(context.Background(), abs); err != nil || !strings.Contains(out, "file cart/cart.go") {
		t.Errorf("an absolute path must still resolve on a contested pin: out=%q err=%v", out, err)
	}
}

func TestContextForTask_MissingAndOddFilesAreReportedNotFatal(t *testing.T) {
	s := newShop(t)
	pack := s.collect(t, map[string]any{"files": []string{"cart/nope.go", "cart/cart.go"}})
	if got := seedPaths(pack); !slices.Equal(got, []string{"cart/cart.go"}) {
		t.Errorf("seeds = %v, want only the existing file", got)
	}
	if len(pack.Misses) != 1 || pack.Misses[0].Reason != "file not found" {
		t.Errorf("misses = %+v, want a single file-not-found", pack.Misses)
	}
}

func TestContextForTask_SeedCapReportsTheOverflow(t *testing.T) {
	s := newShop(t)
	files := []string{
		"cart/cart.go", "cart/cart_test.go", "pricing/discount.go", "pricing/discount_test.go",
		"reports/reports.go", "reports/reports_test.go", "store/ledger.go", "api/checkout.go", "scripts/export.py",
	}
	pack := s.collect(t, map[string]any{"files": files})
	if len(pack.Seeds) != contextMaxSeeds || len(pack.Misses) != 1 {
		t.Fatalf("seeds=%d misses=%d, want %d seeds and the ninth reported", len(pack.Seeds), len(pack.Misses), contextMaxSeeds)
	}
	if m := pack.Misses[0]; m.Input != "scripts/export.py" || !strings.Contains(m.Reason, "8-seed cap") {
		t.Errorf("the ninth seed is reported as %+v", m)
	}
}

func TestContextForTask_RepeatedSeedsAreCollapsed(t *testing.T) {
	s := newShop(t)
	dup := s.collect(t, map[string]any{"files": []string{"cart/cart.go", "cart/cart.go"}, "symbols": []string{"cart/cart.go#Cart.Add", "cart/cart.go#Cart.Add"}})
	if len(dup.Seeds) != 2 {
		t.Errorf("repeated seeds were not collapsed: %v", seedPaths(dup))
	}
}

// within and corpora narrow what a seed may resolve to, and an excluded
// candidate is counted but never named.
func TestContextForTask_ScopeNarrowsSeeds(t *testing.T) {
	s := newShop(t)

	narrowed := s.collect(t, map[string]any{"symbols": []string{"Total"}, "within": []string{"cart"}})
	if len(narrowed.Seeds) != 1 || narrowed.Seeds[0].Path != "cart/cart.go" || len(narrowed.Misses) != 0 {
		t.Errorf("within [cart] should leave exactly cart's Total, got seeds=%v misses=%+v", seedPaths(narrowed), narrowed.Misses)
	}

	out, err := s.run(t, map[string]any{"symbols": []string{"Total"}, "within": []string{"pricing"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "2 more excluded by within/corpora") {
		t.Errorf("the exclusion is not counted:\n%s", out)
	}
	for _, leaked := range []string{"cart/cart.go", "store/ledger.go"} {
		if strings.Contains(out, leaked) {
			t.Errorf("a candidate outside the within filter (%s) appears in the output:\n%s", leaked, out)
		}
	}

	for _, tc := range []struct {
		name   string
		args   map[string]any
		seeds  []string
		reason string
	}{
		{"corpus excludes code", map[string]any{"files": []string{"cart/cart.go"}, "corpora": []string{"docs"}}, nil, "corpus code is not in corpora"},
		{"corpus admits docs", map[string]any{"files": []string{"docs/pricing.md"}, "corpora": []string{"docs"}}, []string{"docs/pricing.md"}, ""},
		{"within excludes a file", map[string]any{"files": []string{"cart/cart.go"}, "within": []string{"store"}}, nil, "outside the within filter"},
		{"glob within", map[string]any{"files": []string{"cart/cart.go", "docs/pricing.md"}, "within": []string{"*.go"}}, []string{"cart/cart.go"}, "outside the within filter"},
	} {
		pack := s.collect(t, tc.args)
		if !slices.Equal(seedPaths(pack), tc.seeds) {
			t.Errorf("%s: seeds = %v, want %v", tc.name, seedPaths(pack), tc.seeds)
		}
		if tc.reason != "" && (len(pack.Misses) != 1 || pack.Misses[0].Reason != tc.reason) {
			t.Errorf("%s: misses = %+v, want reason %q", tc.name, pack.Misses, tc.reason)
		}
	}
}

func TestContextForTask_WithoutAnIndexSymbolsAreUnresolvedAndFilesStillResolve(t *testing.T) {
	root := copyShopFixture(t)
	col := NewContextCollector(nil).
		WithWorkspace(func(context.Context) string { return root }).
		WithBoundary(testBoundaryGuard(root))
	raw, _ := json.Marshal(map[string]any{"files": []string{"cart/cart.go"}, "symbols": []string{"cart/cart.go#Cart.Add"}})
	out, err := NewContextForTask(col).Execute(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"file cart/cart.go", "no topology index is available", "topology index unavailable"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestContextForTask_BudgetIsDisclosed(t *testing.T) {
	s := newShop(t)
	first := func(args map[string]any) string {
		out, err := s.run(t, args)
		if err != nil {
			t.Fatal(err)
		}
		return strings.SplitN(out, "\n", 2)[0]
	}
	if h := first(map[string]any{"files": []string{"cart/cart.go"}}); !strings.Contains(h, "max_bytes=12000") || strings.Contains(h, "clamped") {
		t.Errorf("default header = %q, want max_bytes=12000 and no clamp", h)
	}
	if h := first(map[string]any{"files": []string{"cart/cart.go"}, "max_bytes": 99999}); !strings.Contains(h, "max_bytes=32000 (clamped from 99999)") {
		t.Errorf("over-cap header = %q, want the clamp disclosed", h)
	}
	if h := first(map[string]any{"files": []string{"cart/cart.go"}, "max_bytes": 32000}); strings.Contains(h, "clamped") {
		t.Errorf("a value at the cap is not clamped, header = %q", h)
	}
}

// countRecords counts the packer's records in a rendered pack: one per line,
// except that a delivered body (its head, its guard and its numbered lines) is one
// record, which is how the omission footer counts it.
func countRecords(out string) int {
	n, inBody := 0, false
	for _, l := range strings.Split(out, "\n") {
		switch {
		case bodyHeadRe.MatchString(l):
			n++
			inBody = true
		case inBody && (guardRe.MatchString(l) || gutterRe.MatchString(l)):
		default:
			n++
			inBody = false
		}
	}
	return n
}

// With a tiny max_bytes the whole output stays inside max_bytes minus the
// reserve, and the omitted count is exactly the records that went missing.
func TestContextForTask_TinyBudgetOmitsExactlyAndNeverOverruns(t *testing.T) {
	s := newShop(t)
	args := func(maxBytes int) map[string]any {
		return map[string]any{
			"files":     []string{"cart/cart.go", "cart/cart_test.go", "pricing/discount.go", "store/ledger.go", "web/CartBadge.svelte"},
			"symbols":   []string{"Total", "cart/cart.go#Cart.Coupon"},
			"max_bytes": maxBytes,
		}
	}
	full, err := s.run(t, args(contextMaxBytesCap))
	if err != nil {
		t.Fatal(err)
	}
	fullRecords := countRecords(full)
	sawOmission := false
	for mb := contextMinMaxBytes; mb <= contextMinMaxBytes+1200; mb += 7 {
		out, err := s.run(t, args(mb))
		if err != nil {
			t.Fatalf("max_bytes %d: %v", mb, err)
		}
		if len(out) > mb-contextReserveBytes {
			t.Fatalf("max_bytes %d: output is %d bytes, over the %d-byte pack budget", mb, len(out), mb-contextReserveBytes)
		}
		lines := strings.Split(out, "\n")
		omitted := 0
		if last := lines[len(lines)-1]; strings.HasPrefix(last, "omitted: ") {
			sawOmission = true
			if _, err := fmt.Sscanf(last, "omitted: %d ", &omitted); err != nil {
				t.Fatalf("max_bytes %d: unparseable footer %q", mb, last)
			}
			lines = lines[:len(lines)-1]
		}
		if shown := countRecords(strings.Join(lines, "\n")); shown+omitted != fullRecords {
			t.Errorf("max_bytes %d: %d records shown + %d omitted != %d in the full pack", mb, shown, omitted, fullRecords)
		}
	}
	if !sawOmission {
		t.Error("the sweep never produced an omission, so it proved nothing about the count")
	}
}
