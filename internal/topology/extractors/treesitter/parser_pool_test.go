package treesitter

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tsg "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"

	"github.com/plumbkit/plumb/internal/topology"
)

// slowSource builds a Markdown document big enough that one parse takes well
// over 100 ms. Markdown is the GLR-heavy grammar (~200 nodes per byte), which is
// what makes the timing margins in these tests comfortable rather than tight: at
// this size a parse runs ~30x longer than the delays used below, so a fast
// machine cannot finish the work the test is trying to interrupt.
func slowSource() []byte {
	var sb strings.Builder
	for i := range 4000 {
		fmt.Fprintf(&sb, "## Heading %d\n\nSome *emphasised* text with a [link](https://example.com/%d) and `code`.\n\n"+
			"- item one\n- item two\n\n> quoted line\n\n", i, i)
	}
	return []byte(sb.String())
}

func markdownNodes(t *testing.T, ctx context.Context, src []byte) ([]topology.Node, error) {
	t.Helper()
	nodes, _, err := NewMarkdown().Extract(ctx, "doc.md", src)
	return nodes, err
}

// TestExtractWith_CancelledContextAbortsParseInFlight is the regression for
// PLAN-1 follow-up (d).
//
// extractWith checked ctx.Err() once on entry and thereafter only ctx.Deadline(),
// so a context cancelled with NO deadline — the ordinary shape of a cancelled
// context — started a parse and ran it to completion. Nothing was waiting for
// the result by then; the indexer's single worker just paid for it. The
// cancellation flag aborts the parse already running, 1–2 ms after the flip.
func TestExtractWith_CancelledContextAbortsParseInFlight(t *testing.T) {
	src := slowSource()

	// Establish that this input really is slow, so a fast return below can only
	// mean the cancellation took effect.
	start := time.Now()
	if _, err := markdownNodes(t, context.Background(), src); err != nil {
		t.Fatalf("baseline parse: %v", err)
	}
	baseline := time.Since(start)
	if baseline < 50*time.Millisecond {
		t.Skipf("baseline parse took %v — too fast for the cancellation margin to mean anything", baseline)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()
	start = time.Now()
	_, err := markdownNodes(t, ctx, src)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a cancelled context must surface an error, not a silently truncated symbol set")
	}
	// The point is not merely that it errors — it must stop EARLY. Running to
	// completion and then reporting the cancellation would waste exactly what
	// this change exists to save.
	if elapsed > baseline/2 {
		t.Errorf("parse took %v against a %v baseline; the cancellation flag did not abort it in flight", elapsed, baseline)
	}
}

// A parse whose context carries a deadline that has not expired must run
// normally: the cancellation wiring must not make the common path fail.
func TestExtractWith_LiveDeadlineStillParses(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	nodes, err := markdownNodes(t, ctx, []byte("# Title\n\nbody\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nodes) == 0 {
		t.Error("expected symbols from a well-formed document")
	}
}

// TestExtractWith_PooledParserDoesNotLeakTimeout is the regression for the bug
// pooling introduces if a borrowed parser is used as-is.
//
// SetTimeoutMicros PERSISTS on the parser — verified directly: a parser that
// timed out once reports the same TimeoutMicros afterwards and stops early on
// its next parse too. Upstream's own pool resets it on checkout; a plain
// sync.Pool does not. So a file indexed under a tight deadline would poison the
// next file that has no deadline at all, returning a silently truncated symbol
// set rather than an error.
//
// The dirty parser is planted in the pool DIRECTLY rather than by running a
// parse that times out. That first attempt looked more realistic and was
// worthless: a big-enough-to-time-out parse allocates tens of MB, the resulting
// GC empties sync.Pool (it is cleared every cycle), and Get then returned a
// clean parser — so the test passed against a deliberately broken
// implementation. Planting the parser and parsing a tiny document keeps
// allocation low enough that the pool survives to hand it back, and a 1 µs stale
// timeout trips any parse at all.
func TestExtractWith_PooledParserDoesNotLeakTimeout(t *testing.T) {
	lang := grammars.MarkdownLanguage()
	dirty := tsg.NewParser(lang)
	dirty.SetTimeoutMicros(1) // 1 µs: shorter than any real parse
	parserPoolFor(lang).Put(dirty)

	nodes, err := markdownNodes(t, context.Background(), []byte("# Title\n\nbody text\n"))
	if err != nil {
		t.Fatalf("a parse with no deadline inherited a pooled parser's stale timeout: %v", err)
	}
	if len(nodes) == 0 {
		t.Error("expected symbols; an inherited timeout truncates the parse to nothing")
	}
}

// TestScrubPooledParser_ClearsEverythingPlumbSets is the deterministic guard for
// the intake/release scrub, and the one that actually holds.
//
// The two planted-parser tests around it are worth keeping as end-to-end checks,
// but neither can be relied on to FAIL when the scrub is removed: they depend on
// sync.Pool handing back the specific parser they planted, and sync.Pool is
// emptied on every GC cycle. Measured on the mutant (scrub deleted): the
// cancellation test fails 3/3 under `-run TestExtractWith_Pooled...` but passes
// 3/3 under a plain `go test ./...` of this package, because the sibling tests
// allocate enough to trigger a collection first. That is exactly how CI runs it,
// so those tests would have waved the regression straight through.
//
// This one touches no pool and no GC: it asserts the contract directly on a
// parser it owns, using the getters the binding exposes.
func TestScrubPooledParser_ClearsEverythingPlumbSets(t *testing.T) {
	parser := tsg.NewParser(grammars.MarkdownLanguage())
	raised := uint32(1)
	parser.SetTimeoutMicros(1)
	parser.SetCancellationFlag(&raised)

	scrubPooledParser(parser)

	if got := parser.TimeoutMicros(); got != 0 {
		t.Errorf("stale timeout survived the scrub: got %dµs, want 0", got)
	}
	if got := parser.CancellationFlag(); got != nil {
		t.Errorf("stale cancellation flag survived the scrub: got %p, want nil", got)
	}
}

// The same hazard for the cancellation flag: a parser returned to the pool still
// pointing at a flag that has been set to 1 would abort the NEXT parse before it
// started, turning one cancelled file into an unbounded run of spurious
// failures.
//
// Retained as an end-to-end check only — see
// TestScrubPooledParser_ClearsEverythingPlumbSets for why this one cannot be
// trusted to fail on its own.
func TestExtractWith_PooledParserDoesNotLeakCancellation(t *testing.T) {
	lang := grammars.MarkdownLanguage()
	dirty := tsg.NewParser(lang)
	raised := uint32(1)
	dirty.SetCancellationFlag(&raised)
	parserPoolFor(lang).Put(dirty)

	nodes, err := markdownNodes(t, context.Background(), []byte("# Title\n\nbody text\n"))
	if err != nil {
		t.Fatalf("a fresh parse inherited a pooled parser's raised cancellation flag: %v", err)
	}
	if len(nodes) == 0 {
		t.Error("expected symbols; an inherited raised flag aborts the parse immediately")
	}
}

// TestExtractWith_PoolReuseIsResultIdentical guards the property the whole
// change rests on: a borrowed parser must produce exactly what a fresh one
// would. Reuse that subtly altered a tree would corrupt the index in a way no
// other test looks for, since every extractor's own tests parse only once.
func TestExtractWith_PoolReuseIsResultIdentical(t *testing.T) {
	src := []byte("# Title\n\n## Section A\n\ntext\n\n## Section B\n\n### Nested\n\nmore text\n")

	key := func(nodes []topology.Node) string {
		var sb strings.Builder
		for _, n := range nodes {
			fmt.Fprintf(&sb, "%s|%s|%d-%d|%d-%d;", n.Kind, n.Name, n.StartLine, n.EndLine, n.StartByte, n.EndByte)
		}
		return sb.String()
	}

	first, err := markdownNodes(t, context.Background(), src)
	if err != nil {
		t.Fatalf("first parse: %v", err)
	}
	want := key(first)
	if want == "" {
		t.Fatal("fixture produced no symbols; the comparison would be vacuous")
	}
	// Several rounds, since the pool only starts handing back a used parser
	// after the first return.
	for i := range 5 {
		got, err := markdownNodes(t, context.Background(), src)
		if err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
		if key(got) != want {
			t.Fatalf("round %d produced a different tree from the first parse:\n got %s\nwant %s", i, key(got), want)
		}
	}
}

// The pool must be per-grammar: handing a Markdown parse a parser built for the
// Python grammar would produce nonsense rather than an error.
func TestParserPoolFor_IsPerGrammar(t *testing.T) {
	md, py := grammars.MarkdownLanguage(), grammars.PythonLanguage()
	mdPool, pyPool := parserPoolFor(md), parserPoolFor(py)
	if mdPool == pyPool {
		t.Fatal("two grammars must not share a parser pool")
	}
	if again := parserPoolFor(md); again != mdPool {
		t.Error("the same grammar must resolve to the same pool, or nothing is ever reused")
	}
}

// releaseParser is the seam that makes reuse safe; assert it actually clears
// both fields rather than trusting the call sites.
func TestReleaseParser_ClearsPerCallState(t *testing.T) {
	lang := grammars.MarkdownLanguage()
	pool := parserPoolFor(lang)
	parser := tsg.NewParser(lang)

	var flag uint32
	parser.SetTimeoutMicros(1234)
	parser.SetCancellationFlag(&flag)
	releaseParser(pool, parser)

	if got := parser.TimeoutMicros(); got != 0 {
		t.Errorf("TimeoutMicros = %d after release, want 0 — a stale timeout would truncate the next parse", got)
	}
	if parser.CancellationFlag() != nil {
		t.Error("CancellationFlag must be nil after release — a stale pointer keeps a dead variable alive " +
			"and could abort the next parse before it starts")
	}
}

// BenchmarkExtractMarkdown_Pooled and its FreshParserPerCall sibling measure the
// change this file exists for, as a matched pair so the comparison needs no
// checkout of the previous revision:
//
//	go test -bench ExtractMarkdown -benchmem ./internal/topology/extractors/treesitter/
//
// Measured (Apple silicon, 200 iterations): pooled 2.07 ms/op, 70 KB/op, 189
// allocs/op against fresh 3.14 ms/op, 1.73 MB/op, 4635 allocs/op — 1.5x wall and
// ~25x allocation.
//
// The comparison is deliberately unfair to the pooled side: it runs the whole
// Extract (parse plus the walk that builds nodes and edges) while the control
// does the bare parse and nothing else. The pooled path therefore does strictly
// more work and still wins, so these figures UNDERSTATE the parser saving rather
// than flattering it.
func BenchmarkExtractMarkdown_Pooled(b *testing.B) {
	src := []byte(strings.Repeat("## Heading\n\ntext with `code` and a [link](https://example.com).\n\n", 40))
	ex := NewMarkdown()
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		if _, _, err := ex.Extract(ctx, "doc.md", src); err != nil {
			b.Fatal(err)
		}
	}
}

// The same work with a fresh parser per call, so the benchmark carries its own
// control instead of relying on a checkout of the previous revision.
func BenchmarkExtractMarkdown_FreshParserPerCall(b *testing.B) {
	src := []byte(strings.Repeat("## Heading\n\ntext with `code` and a [link](https://example.com).\n\n", 40))
	lang := grammars.MarkdownLanguage()
	b.ReportAllocs()
	for b.Loop() {
		parser := tsg.NewParser(lang)
		tree, err := parser.Parse(src)
		if err != nil {
			b.Fatal(err)
		}
		tree.Release()
	}
}
