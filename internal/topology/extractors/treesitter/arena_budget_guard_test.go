package treesitter

import (
	"strings"
	"testing"

	tsg "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"
)

const mib = 1024 * 1024

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

// parseMarkdownRuntime parses src under the given GOT_PARSE_MEMORY_BUDGET_MB and
// returns gotreesitter's own accounting for that parse.
//
// The library's ParseRuntime is the measurement, deliberately, rather than a
// runtime.MemStats delta around the call. Both MemoryBudgetBytes and
// ArenaBytesAllocated are computed by the parser from the parse itself, so they
// are identical run to run and machine to machine; a HeapInuse delta is a
// sample of the Go heap at two instants and moves with GC timing, core count
// and whatever else the process has done. See the note on the test below.
func parseMarkdownRuntime(t *testing.T, src []byte, budgetMB string) tsg.ParseRuntime {
	t.Helper()
	t.Setenv("GOT_PARSE_MEMORY_BUDGET_MB", budgetMB)
	tsg.ResetParseEnvConfigCacheForTests()
	t.Cleanup(tsg.ResetParseEnvConfigCacheForTests)

	tree, err := tsg.NewParser(grammars.MarkdownLanguage()).Parse(src)
	if err != nil || tree == nil {
		t.Fatalf("parse markdown: tree=%v err=%v", tree, err)
	}
	defer tree.Release()
	return tree.ParseRuntime()
}

// TestParseMemoryBudgetBoundsLargeMarkdown is the regression guard for the daemon
// memory work: a per-parse memory budget must actually reach the parser and must
// bound the transient arena of a large GLR-heavy file.
//
// It asserts two things, both from gotreesitter's own per-parse accounting:
//
//  1. the budget REACHES the parser — MemoryBudgetBytes echoes the env var, so a
//     release that stops honouring GOT_PARSE_MEMORY_BUDGET_MB fails here;
//  2. the budget BOUNDS the arena — ArenaBytesAllocated stays within it.
//
// History, so this is not "simplified" back into a trap. The original guard
// compared two runtime.MemStats HeapInuse deltas, budgeted versus unbudgeted,
// and skipped itself unless the unbudgeted parse exceeded an absolute 200 MB.
// That made it environment-dependent in both directions: it SKIPPED silently on
// machines where the unbudgeted parse came in under the threshold (which is what
// it had been doing on CI), and once a library change pushed the unbudgeted
// figure over the line it FAILED on CI while passing locally, because a HeapInuse
// delta is not comparable across machines. A guard that is skipped is not a
// guard, and one that fails only on the runner cannot be diagnosed. Both
// assertions below are exact and machine-independent; neither can skip.
func TestParseMemoryBudgetBoundsLargeMarkdown(t *testing.T) {
	const budgetMB = 128
	src := largeMarkdown(300)

	rt := parseMarkdownRuntime(t, src, "128")

	t.Logf("large markdown (%d KB): MemoryBudgetBytes=%d (%d MiB), ArenaBytesAllocated=%d (%d MiB), stop=%q",
		len(src)/1024, rt.MemoryBudgetBytes, rt.MemoryBudgetBytes/mib,
		rt.ArenaBytesAllocated, rt.ArenaBytesAllocated/mib, rt.MemoryBudgetStopSource)

	if want := int64(budgetMB) * mib; rt.MemoryBudgetBytes != want {
		t.Fatalf("GOT_PARSE_MEMORY_BUDGET_MB not applied: MemoryBudgetBytes=%d, want %d — "+
			"the per-parse budget did not reach the parser, so nothing bounds the daemon's transient arena",
			rt.MemoryBudgetBytes, want)
	}

	// Guard the accounting itself: a release that stopped populating this field
	// would otherwise satisfy the bound below by reporting zero.
	if rt.ArenaBytesAllocated <= 0 {
		t.Fatalf("ArenaBytesAllocated=%d — the parse reported no arena accounting, so the bound below proves nothing",
			rt.ArenaBytesAllocated)
	}

	if rt.ArenaBytesAllocated > rt.MemoryBudgetBytes {
		t.Fatalf("budget did not bound the parse: arena=%d bytes (%d MiB) exceeds budget=%d bytes (%d MiB)",
			rt.ArenaBytesAllocated, rt.ArenaBytesAllocated/mib, rt.MemoryBudgetBytes, rt.MemoryBudgetBytes/mib)
	}
}

// TestParseMemoryBudgetTracksTheConfiguredValue pins the wiring itself: a
// DIFFERENT budget must produce a different MemoryBudgetBytes. Without this, the
// assertion above could be satisfied by a parser that hardcoded 128 MiB and
// ignored the environment entirely.
func TestParseMemoryBudgetTracksTheConfiguredValue(t *testing.T) {
	src := largeMarkdown(50)
	for _, mb := range []int64{16, 64} {
		rt := parseMarkdownRuntime(t, src, itoa(mb))
		if want := mb * mib; rt.MemoryBudgetBytes != want {
			t.Errorf("GOT_PARSE_MEMORY_BUDGET_MB=%d: MemoryBudgetBytes=%d, want %d", mb, rt.MemoryBudgetBytes, want)
		}
	}
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var digits []byte
	for v > 0 {
		digits = append([]byte{byte('0' + v%10)}, digits...)
		v /= 10
	}
	return string(digits)
}
