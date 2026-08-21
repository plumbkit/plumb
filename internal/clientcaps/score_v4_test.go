package clientcaps

import "testing"

// TestScoreModelV4Version pins the bump this card makes: v3 → v4.
func TestScoreModelV4Version(t *testing.T) {
	if ModelVersion != 4 {
		t.Fatalf("ModelVersion = %d, want 4", ModelVersion)
	}
}

// TestScoreModelV4PlainRangedReadExcludedForNativeClient is the headline
// behaviour change: a client with its own native ranged read (claude-code)
// gets NO efficiency credit for a plain read_file ranged read — that saving
// is reproducible with the client's own tools, so crediting it measured the
// agent's restraint, not anything plumb contributed.
func TestScoreModelV4PlainRangedReadExcludedForNativeClient(t *testing.T) {
	got := Score("read_file", "claude-code", 350, 3500, 0, true)
	if got.Total() != 0 {
		t.Errorf("read_file ranged read, native client = %+v, want zero", got)
	}
	// find_files carries the same plain-listing mechanism and is excluded too.
	gotFind := Score("find_files", "claude-code", 350, 3500, 0, true)
	if gotFind.Total() != 0 {
		t.Errorf("find_files, native client = %+v, want zero", gotFind)
	}
}

// TestScoreModelV4PlainRangedReadStillCreditsThinClient proves the exclusion
// is native-capability-gated, not a blanket removal: a thin client with no
// native read at all still earns the delivered context as capability — the
// two honest wins the card requires stay (Do NOT: remove the token story
// entirely).
func TestScoreModelV4PlainRangedReadStillCreditsThinClient(t *testing.T) {
	got := Score("read_file", "claude-desktop", 350, 3500, 0, true)
	if got.Capability != 100 || got.Efficiency != 0 {
		t.Errorf("read_file ranged read, thin client = %+v, want capability=100 efficiency=0", got)
	}
}

// TestScoreModelV4NamedReadKeepsCreditForNativeClient is the other half of
// the split: read_symbol resolves a symbol NAME to the relevant bytes — a
// mechanism no native ranged Read reproduces without first reading the whole
// file to find the symbol — so a capable client still earns the efficiency
// delta v4 removed from the plain read_file case above.
func TestScoreModelV4NamedReadKeepsCreditForNativeClient(t *testing.T) {
	got := Score("read_symbol", "claude-code", 350, 3500, 0, true)
	if got.Efficiency != 900 || got.Capability != 0 {
		t.Errorf("read_symbol, native client = %+v, want efficiency=900", got)
	}
	thin := Score("read_symbol", "claude-desktop", 350, 3500, 0, true)
	if thin.Capability != 100 || thin.Efficiency != 0 {
		t.Errorf("read_symbol, thin client = %+v, want capability=100", thin)
	}
}

// TestScoreModelV4SemanticToolsUnaffected pins that catSemantic tools
// (file_outline, find_references, ...) are untouched by the v4 change — their
// mechanism (reconstruction cost) was never the plain-ranged-read arithmetic
// this card is correcting.
func TestScoreModelV4SemanticToolsUnaffected(t *testing.T) {
	got := Score("file_outline", "claude-code", 35, 0, 0, true)
	if got.Efficiency != 790 || got.Capability != 0 {
		t.Errorf("file_outline, native client = %+v, want efficiency=790", got)
	}
}

// TestSurchargeSumsOnlyVisibleTools proves ProfileSurcharge totals just the
// tools a predicate marks visible, and ignores everything else in the
// registered set — the lean-vs-full distinction the banner line depends on.
func TestSurchargeSumsOnlyVisibleTools(t *testing.T) {
	bytes := map[string]int{
		"read_file":  370,  // visible
		"edit_file":  1000, // visible
		"git":        2000, // hidden under this profile
		"topology_x": 4000, // hidden
	}
	visible := func(name string) bool { return name == "read_file" || name == "edit_file" }

	got := ProfileSurcharge(bytes, visible)
	if got.ToolCount != 2 {
		t.Errorf("ToolCount = %d, want 2", got.ToolCount)
	}
	if got.TotalBytes != 1370 {
		t.Errorf("TotalBytes = %d, want 1370", got.TotalBytes)
	}
	// 1370 / 3.7 = 370.27... rounds to 370.
	if got.Tokens != 370 {
		t.Errorf("Tokens = %d, want 370", got.Tokens)
	}
}

// TestSurchargeNilVisibleMeansEverything pins the full-profile shortcut: a
// nil predicate counts every registered tool, matching "full" profile
// behaviour without callers having to pass an always-true closure.
func TestSurchargeNilVisibleMeansEverything(t *testing.T) {
	bytes := map[string]int{"a": 37, "b": 37}
	got := ProfileSurcharge(bytes, nil)
	if got.ToolCount != 2 || got.TotalBytes != 74 {
		t.Errorf("got %+v, want ToolCount=2 TotalBytes=74", got)
	}
}

// TestSurchargeEmptyRegistryIsZero guards against a divide/format surprise
// when nothing is registered yet (e.g. mid-construction in a test harness).
func TestSurchargeEmptyRegistryIsZero(t *testing.T) {
	got := ProfileSurcharge(nil, nil)
	if got.Tokens != 0 || got.ToolCount != 0 || got.TotalBytes != 0 {
		t.Errorf("empty registry = %+v, want all zero", got)
	}
}

// TestSurchargePlausibleAgainstPLAN323 sanity-checks the conversion against
// the PLAN-323 measurement this methodology reuses: 59 tools, 106,341 chars,
// measured as ~28,700 tokens (notes-system-improvements-2026-08-15.md). The
// tolerance is loose — this is a methodology cross-check, not a pin of the
// live registry's exact current size, which drifts as tools are added.
func TestSurchargePlausibleAgainstPLAN323(t *testing.T) {
	got := ProfileSurcharge(map[string]int{"whole-surface": 106341}, nil)
	const want = 28700
	diff := got.Tokens - want
	if diff < 0 {
		diff = -diff
	}
	if diff > 200 {
		t.Errorf("Tokens = %d, want ~%d (±200) reproducing the PLAN-323 figure", got.Tokens, want)
	}
}
