package tools_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/tools"
	"github.com/plumbkit/plumb/internal/topology"
)

// move_symbol_ambiguity_test.go guards the half of move_symbol's ambiguity
// refusal that only exists once the tree-sitter fallback can actually run
// (PLAN-403 review §1). Before the fix the refusal was gated on the language
// server having ANSWERED, so a cold or slow server skipped it entirely and
// topologyNodeByPath's "first node with this name" silently chose one of two
// same-named declarations — and rewrote two files saying nothing about it.
//
// Both directions are asserted in the same build, over the same fallback path:
// it must refuse when the index really is ambiguous, and it must NOT refuse
// when it is not (that direction is what an over-eager guard breaks, and it is
// also covered by the move_symbol case in slow_lsp_fallback_test.go).

// ambiguousMoveSrc has two methods named Foo, distinguished only by receiver —
// the shape a bare name_path cannot address. moveSrc (move_symbol_test.go) is
// the unambiguous counterpart.
const ambiguousMoveSrc = "package demo\n\ntype A struct{}\n\ntype B struct{}\n\n" +
	"func (a A) Foo() int { return 1 }\n\nfunc (b B) Foo() int { return 2 }\n"

// moveViaFallback runs move_symbol for a bare name_path over src, against a
// server that accepts the request and never answers, so the tree-sitter
// fallback is what resolves the target. It returns the tool's result plus the
// on-disk content of both files afterwards.
func moveViaFallback(t *testing.T, src, namePath string) (out, srcAfter, dstAfter string, err error) {
	t.Helper()
	dir := t.TempDir()
	srcPath, srcURI := writeInDir(t, dir, "src.go", src)
	dstPath, dstURI := writeInDir(t, dir, "dst.go", "package demo\n\nfunc Keep() {}\n")
	store := openTopologyStore(t, dir)
	tool := tools.NewMoveSymbol(slowLSP(), slowFallbackBudget).
		WithTopologyFallback(func() *topology.Store { return store })

	out, err = tool.Execute(context.Background(), slowFallbackArgs(t, map[string]any{
		"source_uri": srcURI, "name_path": namePath,
		"destination_uri": dstURI, "dry_run": false,
	}))
	return out, readFileText(t, srcPath), readFileText(t, dstPath), err
}

func readFileText(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}

func TestMoveSymbol_SlowLSPStillRefusesAnAmbiguousBareName(t *testing.T) {
	const srcBefore = ambiguousMoveSrc
	const dstBefore = "package demo\n\nfunc Keep() {}\n"

	out, srcAfter, dstAfter, err := moveViaFallback(t, srcBefore, "Foo")

	if err == nil {
		t.Fatalf("two declarations named Foo must be refused however the target was resolved; "+
			"the tree-sitter fallback moved one of them instead:\n%s", out)
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("the refusal must be the same ambiguity refusal a healthy server produces, got: %v", err)
	}
	// The refusal is worth nothing if the write happened anyway.
	if srcAfter != srcBefore {
		t.Errorf("a refused move must leave the source untouched:\n%s", srcAfter)
	}
	if dstAfter != dstBefore {
		t.Errorf("a refused move must leave the destination untouched:\n%s", dstAfter)
	}
}

func TestMoveSymbol_SlowLSPStillMovesAnUnambiguousBareName(t *testing.T) {
	// The other direction, in the same build: a guard that refuses everything
	// would pass the test above and break every real fallback move.
	out, srcAfter, dstAfter, err := moveViaFallback(t, moveSrc, "Foo")
	if err != nil {
		t.Fatalf("one declaration named Foo is not ambiguous and must still move via the fallback: %v", err)
	}
	if !strings.Contains(out, "topology fallback") {
		t.Errorf("expected the tree-sitter fallback to have resolved the target:\n%s", out)
	}
	if strings.Contains(srcAfter, "func Foo() int { return 1 }") {
		t.Errorf("the declaration was not removed from the source:\n%s", srcAfter)
	}
	if !strings.Contains(dstAfter, "func Foo() int { return 1 }") {
		t.Errorf("the declaration did not reach the destination:\n%s", dstAfter)
	}
}

// TestMoveSymbol_AmbiguityRefusalSurvivesAnAbsentServer covers the other way
// the fallback is reached — a server that errors outright rather than one that
// is merely slow. It is the pre-existing half of the same silent guess.
func TestMoveSymbol_AmbiguityRefusalSurvivesAnAbsentServer(t *testing.T) {
	dir := t.TempDir()
	srcPath, srcURI := writeInDir(t, dir, "src.go", ambiguousMoveSrc)
	_, dstURI := writeInDir(t, dir, "dst.go", "package demo\n\nfunc Keep() {}\n")
	store := openTopologyStore(t, dir)
	tool := tools.NewMoveSymbol(&mockLSP{err: errors.New("lsp unavailable")}, slowFallbackBudget).
		WithTopologyFallback(func() *topology.Store { return store })

	out, err := tool.Execute(context.Background(), slowFallbackArgs(t, map[string]any{
		"source_uri": srcURI, "name_path": "Foo",
		"destination_uri": dstURI, "dry_run": false,
	}))
	if err == nil {
		t.Fatalf("an absent server must not turn an ambiguous name into a silent guess:\n%s", out)
	}
	if got := readFileText(t, srcPath); got != ambiguousMoveSrc {
		t.Errorf("a refused move must leave the source untouched:\n%s", got)
	}
}
