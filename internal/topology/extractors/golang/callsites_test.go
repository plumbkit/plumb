package golang

import (
	"context"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/topology"
)

// callSiteFixture holds every shape the call-sites table exists to capture, and
// two it must NOT invent:
//
//   - an unresolved package-qualified call (`fmtpkg.Do`) — the shape today's
//     extractor silently drops;
//   - a PACKAGE-LEVEL registration call (`mux.HandleFunc("/x", h)`), which the
//     intra-file edge walk never visits because it descends fn.Body only;
//   - a COMPOSITE-LITERAL field value (`Use: "serve"`), which is not a call at
//     all and is where a command's name actually lives;
//   - a receiver method call, which must be recorded and left unresolved;
//   - a spread argument, whose element identifiers do not exist syntactically;
//   - a conversion (`[]byte(s)`), which looks like a call and is not one.
const callSiteFixture = `package app

import (
	fmtpkg "example.com/mod/fmtpkg"
	"net/http"
	"example.com/mod/cobra"
)

var mux = http.NewServeMux()

var _ = mux.HandleFunc("/x", handle)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "",
	RunE:  runServe,
}

func handle()    {}
func runServe()  {}
func addAll()    { root.AddCommand(taskCmds...) }
func convert(s string) []byte { return []byte(s) }

func work(c *client) {
	fmtpkg.Do("hello", nameArg)
	c.Method()
	local()
	wide(a1, a2, a3, a4, a5, a6, a7, a8, a9, a10)
}

func local() {}
`

func extractFixtureSites(t *testing.T, src string) []topology.CallSite {
	t.Helper()
	_, _, sites, err := New().ExtractWithCallSites(context.Background(), "app/app.go", []byte(src))
	if err != nil {
		t.Fatalf("ExtractWithCallSites: %v", err)
	}
	return sites
}

func findSite(sites []topology.CallSite, kind topology.CallSiteKind, qualifier, callee string) *topology.CallSite {
	for i := range sites {
		s := &sites[i]
		if s.Kind == kind && s.Qualifier == qualifier && s.Callee == callee {
			return s
		}
	}
	return nil
}

// TestCallSites_RecordsUnresolvedQualifiedCall pins the core claim: a call whose
// callee is NOT defined in this file is recorded rather than dropped, and the
// qualifier that identifies which package it came from survives.
func TestCallSites_RecordsUnresolvedQualifiedCall(t *testing.T) {
	sites := extractFixtureSites(t, callSiteFixture)
	s := findSite(sites, topology.CallSiteCall, "fmtpkg", "Do")
	if s == nil {
		t.Fatalf("fmtpkg.Do() was not recorded; sites = %s", summarise(sites))
	}
	if s.StartLine == 0 || s.StartByte == 0 {
		t.Errorf("fmtpkg.Do() recorded at line %d byte %d; a site with no position cannot be located",
			s.StartLine, s.StartByte)
	}
	if !s.HasStringArg || s.FirstStringArg != "hello" {
		t.Errorf("first string arg = (%q, %v), want (\"hello\", true)", s.FirstStringArg, s.HasStringArg)
	}
	if len(s.ArgIdents) != 1 || s.ArgIdents[0] != "nameArg" {
		t.Errorf("arg idents = %v, want [nameArg]", s.ArgIdents)
	}
	if s.ArgCount != 2 {
		t.Errorf("arg count = %d, want 2", s.ArgCount)
	}
}

// TestCallSites_RecordsPackageLevelCall is the walk-shape fix. callEdgesFor
// descends fn.Body only, so a call in a package-level initialiser is invisible
// to the edge walk — and package-level registration calls are exactly what route
// and command recovery reads.
func TestCallSites_RecordsPackageLevelCall(t *testing.T) {
	sites := extractFixtureSites(t, callSiteFixture)
	s := findSite(sites, topology.CallSiteCall, "mux", "HandleFunc")
	if s == nil {
		t.Fatalf("package-level mux.HandleFunc() was not recorded; sites = %s", summarise(sites))
	}
	if !s.HasStringArg || s.FirstStringArg != "/x" {
		t.Errorf("route literal = (%q, %v), want (\"/x\", true)", s.FirstStringArg, s.HasStringArg)
	}
	if len(s.ArgIdents) != 1 || s.ArgIdents[0] != "handle" {
		t.Errorf("handler idents = %v, want [handle]", s.ArgIdents)
	}
	if s.EnclosingIdx < 0 {
		t.Error("package-level site has no enclosing declaration index; it should point at the var it initialises")
	}
}

// TestCallSites_RecordsCompositeLiteralField covers the shape that carries no
// call at all. Without it a Cobra command tree cannot be recovered: the command
// name is a struct field, not an argument.
func TestCallSites_RecordsCompositeLiteralField(t *testing.T) {
	sites := extractFixtureSites(t, callSiteFixture)
	use := findSite(sites, topology.CallSiteField, "cobra.Command", "Use")
	if use == nil {
		t.Fatalf("Use: \"serve\" was not recorded; sites = %s", summarise(sites))
	}
	if !use.HasStringArg || use.FirstStringArg != "serve" {
		t.Errorf("Use value = (%q, %v), want (\"serve\", true)", use.FirstStringArg, use.HasStringArg)
	}
	// An empty literal is a different fact from an absent one, and the pair
	// (value, present) is the only way to tell them apart.
	short := findSite(sites, topology.CallSiteField, "cobra.Command", "Short")
	if short == nil {
		t.Fatal(`Short: "" was not recorded`)
	}
	if !short.HasStringArg || short.FirstStringArg != "" {
		t.Errorf(`Short value = (%q, %v), want ("", true) — an empty literal must not read as absent`,
			short.FirstStringArg, short.HasStringArg)
	}
	runE := findSite(sites, topology.CallSiteField, "cobra.Command", "RunE")
	if runE == nil || len(runE.ArgIdents) != 1 || runE.ArgIdents[0] != "runServe" {
		t.Errorf("RunE idents = %v, want [runServe]", runE)
	}
	if runE != nil && runE.HasStringArg {
		t.Error("RunE recorded a string argument; its value is an identifier")
	}
}

// TestCallSites_MethodCallRecordedWithReceiver pins that the modal Go call is
// captured with its receiver text intact. It is not resolvable — that is the
// resolver's decision, made once, in one place — but it must be recorded, or
// "how many calls could not be resolved" has no denominator.
func TestCallSites_MethodCallRecordedWithReceiver(t *testing.T) {
	sites := extractFixtureSites(t, callSiteFixture)
	if s := findSite(sites, topology.CallSiteCall, "c", "Method"); s == nil {
		t.Fatalf("receiver method call c.Method() was not recorded; sites = %s", summarise(sites))
	}
	bare := findSite(sites, topology.CallSiteCall, "", "local")
	if bare == nil {
		t.Fatal("bare call local() was not recorded")
	}
	if bare.Qualifier != "" {
		t.Errorf("bare call qualifier = %q, want empty", bare.Qualifier)
	}
}

// TestCallSites_SpreadAndArgCap covers the two ways an argument list lies. A
// spread's elements do not exist syntactically, so recording `taskCmds` as an
// ordinary argument would be a false datum; and a capped list must be
// distinguishable from a short one.
func TestCallSites_SpreadAndArgCap(t *testing.T) {
	sites := extractFixtureSites(t, callSiteFixture)
	spread := findSite(sites, topology.CallSiteCall, "root", "AddCommand")
	if spread == nil {
		t.Fatal("root.AddCommand(taskCmds...) was not recorded")
	}
	if !spread.ArgSpread {
		t.Error("spread call not flagged; its single ident would read as one ordinary argument")
	}
	wide := findSite(sites, topology.CallSiteCall, "", "wide")
	if wide == nil {
		t.Fatal("wide(...) was not recorded")
	}
	if len(wide.ArgIdents) != topology.MaxCallSiteArgIdents {
		t.Errorf("arg idents = %d, want the cap %d", len(wide.ArgIdents), topology.MaxCallSiteArgIdents)
	}
	if wide.ArgCount != 10 {
		t.Errorf("arg count = %d, want 10 — a truncated list must still report its true length", wide.ArgCount)
	}
	if wide.ArgCount <= len(wide.ArgIdents) {
		t.Error("arg count does not exceed the recorded idents; truncation is undetectable")
	}
}

// TestCallSites_ConversionIsNotACall is the false-positive direction: `[]byte(s)`
// parses as a CallExpr and is not a call. Recording it would put a row in the
// table that no resolver could match and no consumer could interpret.
func TestCallSites_ConversionIsNotACall(t *testing.T) {
	nodes, _, sites, err := New().ExtractWithCallSites(context.Background(), "app/app.go", []byte(callSiteFixture))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sites {
		if s.Callee == "" {
			t.Errorf("a site was recorded with an empty callee: %+v", s)
		}
	}
	// convert()'s body is a single conversion and nothing else, so a site
	// attributed to it is by construction a conversion recorded as a call —
	// whatever name the extractor invented for it.
	convertIdx := -1
	for i, n := range nodes {
		if n.Name == "convert" {
			convertIdx = i
		}
	}
	if convertIdx < 0 {
		t.Fatal("the fixture's convert() was not extracted; this guard would be vacuous")
	}
	for _, s := range sites {
		if s.EnclosingIdx == convertIdx {
			t.Errorf("the conversion []byte(s) was recorded as a %s site %q.%q", s.Kind, s.Qualifier, s.Callee)
		}
	}
	// And the same for the package-level conversion-shaped call, so the guard
	// covers both walk entry points.
	if s := findSite(sites, topology.CallSiteCall, "", "byte"); s != nil {
		t.Errorf("the conversion []byte(s) was recorded as a call to %q", s.Callee)
	}
}

// TestCallSites_EnclosingIndexAddressesTheReturnedNodes pins the contract the
// persistence layer depends on: EnclosingIdx indexes the SAME nodes slice the
// call returned. An index that addressed some other slice would silently attach
// every call to the wrong declaration.
func TestCallSites_EnclosingIndexAddressesTheReturnedNodes(t *testing.T) {
	nodes, _, sites, err := New().ExtractWithCallSites(context.Background(), "app/app.go", []byte(callSiteFixture))
	if err != nil {
		t.Fatal(err)
	}
	s := findSite(sites, topology.CallSiteCall, "fmtpkg", "Do")
	if s == nil {
		t.Fatal("fmtpkg.Do() missing")
	}
	if s.EnclosingIdx < 0 || s.EnclosingIdx >= len(nodes) {
		t.Fatalf("EnclosingIdx = %d, outside the %d returned nodes", s.EnclosingIdx, len(nodes))
	}
	if got := nodes[s.EnclosingIdx].Name; got != "work" {
		t.Errorf("fmtpkg.Do() attributed to %q, want the enclosing func work", got)
	}
	cmd := findSite(sites, topology.CallSiteField, "cobra.Command", "Use")
	if cmd == nil {
		t.Fatal("Use field missing")
	}
	if got := nodes[cmd.EnclosingIdx].Name; got != "serveCmd" {
		t.Errorf(`Use: "serve" attributed to %q, want the package-level var serveCmd`, got)
	}
}

// TestExtractAgreesWithExtractWithCallSites pins that the two entry points do not
// drift: Extract is documented as the same parse minus the sites.
func TestExtractAgreesWithExtractWithCallSites(t *testing.T) {
	ctx := context.Background()
	n1, e1, err := New().Extract(ctx, "app/app.go", []byte(callSiteFixture))
	if err != nil {
		t.Fatal(err)
	}
	n2, e2, sites, err := New().ExtractWithCallSites(ctx, "app/app.go", []byte(callSiteFixture))
	if err != nil {
		t.Fatal(err)
	}
	if len(n1) != len(n2) || len(e1) != len(e2) {
		t.Errorf("Extract gave %d nodes/%d edges, ExtractWithCallSites gave %d/%d",
			len(n1), len(e1), len(n2), len(e2))
	}
	if len(sites) == 0 {
		t.Error("ExtractWithCallSites returned no sites for a fixture full of calls")
	}
}

// TestCallSites_BodylessDeclarationDoesNotPanic pins a crash this walk really
// had: `func Sqrt(x float64) float64` (an assembly or cgo stub) has a nil Body,
// and a nil *ast.BlockStmt passed as an ast.Node is a NON-nil interface — it
// sails past a nil check and panics on the first field access inside ast.Walk.
// The whole file is then recorded as an extractor error, so one asm stub costs a
// package its symbols.
func TestCallSites_BodylessDeclarationDoesNotPanic(t *testing.T) {
	const src = `package p

func Stub(x float64) float64

func real() { Stub(1) }
`
	nodes, _, sites, err := New().ExtractWithCallSites(context.Background(), "p/p.go", []byte(src))
	if err != nil {
		t.Fatalf("ExtractWithCallSites: %v", err)
	}
	if len(nodes) == 0 {
		t.Fatal("no nodes extracted from a file with a bodyless declaration")
	}
	if findSite(sites, topology.CallSiteCall, "", "Stub") == nil {
		t.Errorf("the call to the stub was lost; sites = %s", summarise(sites))
	}
}

func summarise(sites []topology.CallSite) string {
	var b strings.Builder
	for _, s := range sites {
		b.WriteString("\n  ")
		b.WriteString(string(s.Kind))
		b.WriteString(" ")
		if s.Qualifier != "" {
			b.WriteString(s.Qualifier + ".")
		}
		b.WriteString(s.Callee)
	}
	return b.String()
}
