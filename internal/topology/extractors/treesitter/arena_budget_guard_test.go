package treesitter

import (
	"runtime"
	"strings"
	"testing"

	tsg "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"
)

// largeMarkdown builds a synthetic Markdown document of roughly targetKB. The
// Markdown grammar is GLR-heavy (~200 nodes/byte), so a few-hundred-KB document
// drives a large parse arena unless the per-parse memory budget bounds it.
func largeMarkdown(targetKB int) []byte {
	var b strings.Builder
	block := "## Section heading\n\nSome *emphasised* and **strong** text with a [link](http://x) " +
		"and `inline code`, plus a list:\n\n- one\n- two\n- three\n\n> a quote\n\n```go\nfunc f() {}\n```\n\n"
	for b.Len() < targetKB*1024 {
		b.WriteString(block)
	}
	return []byte(b.String())
}

// markdownParseArenaBytes parses src with the given per-parse memory budget (MB;
// "" leaves gotreesitter's default) and returns the live HeapInuse delta while
// the parse tree is still alive — the per-file transient that drives the daemon's
// heap high-water.
func markdownParseArenaBytes(t *testing.T, src []byte, budgetMB string) int64 {
	t.Helper()
	if budgetMB == "" {
		t.Setenv("GOT_PARSE_MEMORY_BUDGET_MB", "0") // disabled: unbounded baseline
	} else {
		t.Setenv("GOT_PARSE_MEMORY_BUDGET_MB", budgetMB)
	}
	tsg.ResetParseEnvConfigCacheForTests()
	t.Cleanup(tsg.ResetParseEnvConfigCacheForTests)

	tsg.DrainArenaPools()
	runtime.GC()
	runtime.GC()
	var m0 runtime.MemStats
	runtime.ReadMemStats(&m0)

	tree, err := tsg.NewParser(grammars.MarkdownLanguage()).Parse(src)
	if err != nil || tree == nil {
		t.Fatalf("parse markdown: tree=%v err=%v", tree, err)
	}
	var m1 runtime.MemStats
	runtime.ReadMemStats(&m1)
	tree.Release()

	return int64(m1.HeapInuse) - int64(m0.HeapInuse)
}

// TestParseMemoryBudgetBoundsLargeMarkdown is the regression guard for the daemon
// memory work: it proves a per-parse memory budget actually bounds the transient
// arena of a large GLR-heavy file. Without a budget a ~300 KB Markdown document
// allocates hundreds of MB for a single parse; with the daemon's 128 MB default
// the same parse is materially smaller. The assertion is relative (budgeted vs
// unbudgeted in the same run) so it is robust across machines and grammar
// versions — the absolute numbers vary, the bounding effect does not.
//
// The package runs no parallel tests and the gotreesitter env cache is process
// global; this test resets it on cleanup so siblings observe the restored env.
func TestParseMemoryBudgetBoundsLargeMarkdown(t *testing.T) {
	src := largeMarkdown(300)

	unbounded := markdownParseArenaBytes(t, src, "")
	bounded := markdownParseArenaBytes(t, src, "128")

	const mb = 1024 * 1024
	t.Logf("large markdown (%d KB): unbudgeted=%.0f MB, budget=128 → %.0f MB",
		len(src)/1024, float64(unbounded)/mb, float64(bounded)/mb)

	// Sanity: the unbudgeted parse must be genuinely large, else the test proves
	// nothing (e.g. a future grammar that no longer blows up).
	if unbounded < 200*mb {
		t.Skipf("unbudgeted parse only %d MB — grammar no longer pathological; guard not meaningful", unbounded/mb)
	}
	// The 128 MB budget must bound the transient well below the unbudgeted size.
	if bounded >= unbounded {
		t.Fatalf("budget did not bound the parse: budgeted=%d MB >= unbudgeted=%d MB — GOT_PARSE_MEMORY_BUDGET_MB not applied",
			bounded/mb, unbounded/mb)
	}
	if bounded > 350*mb {
		t.Fatalf("budgeted parse still %d MB (>350 MB) — per-parse budget is not effectively bounding arena growth", bounded/mb)
	}
}
