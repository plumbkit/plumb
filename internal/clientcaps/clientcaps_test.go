package clientcaps

import "testing"

func TestLookupPrefixSpecificity(t *testing.T) {
	tests := []struct {
		name     string
		wantName string
		wantFile bool // NativeFileRead, distinguishes claude-code from claude-desktop
	}{
		{"claude-code", "claude-code", true},
		{"claude-code/1.2.3", "claude-code", true},
		{"Claude-Code", "claude-code", true}, // case-insensitive
		{"claude-desktop", "claude-desktop", false},
		{"claude-ai", "claude-desktop", false},
		{"claude", "claude-desktop", false}, // bare claude is the thin client
		{"junie-client", "junie", true},
		{"junie", "junie", true},
		{"Junie", "junie", true},
		{"codex", "codex", true},
		{"gemini-cli", "gemini", true},
		{"kimi-code", "kimi-code", true},
		{"kimi-code/1.2", "kimi-code", true},
		{"Kimi-Code", "kimi-code", true},         // case-insensitive
		{"kimi", "kimi-code", true},              // bare alias (Kimi Desktop) resolves to the same entry
		{"kimiko", "kimi-code", true},            // any kimi* client matches the bare alias, by design
		{"kimchi-bot", "unknown", true},          // diverges at the 4th char, so no prefix matches
		{"totally-unknown-xyz", "unknown", true}, // conservative default
		{"", "unknown", true},
		{"zcode", "zcode", true},
		{"zcode/1.0.0", "zcode", true},
		{"ZCode", "zcode", true}, // case-insensitive
	}
	for _, tc := range tests {
		got := Lookup(tc.name)
		if got.Name != tc.wantName {
			t.Errorf("Lookup(%q).Name = %q, want %q", tc.name, got.Name, tc.wantName)
		}
		if got.NativeFileRead != tc.wantFile {
			t.Errorf("Lookup(%q).NativeFileRead = %v, want %v", tc.name, got.NativeFileRead, tc.wantFile)
		}
	}
}

// TestSchemaDiscoveryOnly pins which clients can only invoke advertised tools.
// Claude Code and Kimi Code are flagged true (both build their tool list — and
// Claude Code its ToolSearch list — from tools/list, so a lean-hidden tool is
// unreachable); the other CLI agents and the unknown fallback are false; lean
// eligibility is gated separately on ReliableDeferredToolDiscovery.
func TestSchemaDiscoveryOnly(t *testing.T) {
	tests := []struct {
		client string
		want   bool
	}{
		{"claude-code", true},
		{"claude-code/1.2.3", true},
		{"kimi-code", true},
		{"kimi-code/0.0.0", true},
		{"kimi", true},
		{"codex", false},
		{"gemini-cli", false},
		{"claude-desktop", false},
		{"totally-unknown-xyz", false},
	}
	for _, tc := range tests {
		if got := Lookup(tc.client).SchemaDiscoveryOnly; got != tc.want {
			t.Errorf("Lookup(%q).SchemaDiscoveryOnly = %v, want %v", tc.client, got, tc.want)
		}
	}
}

func TestTokensForRatiosAndFallback(t *testing.T) {
	// 350 code bytes at the Claude code ratio (3.5) → 100 tokens.
	if got := tokensFor(FamilyClaude, ContentCode, 350); got != 100 {
		t.Errorf("tokensFor(claude, code, 350) = %d, want 100", got)
	}
	// Unknown family falls back to the default ratio (4.0): 400 → 100.
	if got := tokensFor(Family("nope"), ContentProse, 400); got != 100 {
		t.Errorf("tokensFor(unknown family) = %d, want 100 (default ratio)", got)
	}
}

// TestKimiCodeIsNotLeanEligible pins the evidence gate: Kimi Code has strong
// native file/search/shell tooling, but that must never be read as proof it can
// invoke an unadvertised tool. Its token relief comes from the client-side
// enabledTools allowlist (`plumb setup kimi-code --lean`), not from the lean
// tools/list profile.
func TestKimiCodeIsNotLeanEligible(t *testing.T) {
	if Lookup("kimi-code").ReliableDeferredToolDiscovery {
		t.Error("kimi-code must not declare ReliableDeferredToolDiscovery without reviewed integration evidence")
	}
}

func TestScoreFailedCallIsZero(t *testing.T) {
	if got := Score("read_file", "claude-desktop", 4000, 0, 0, false); got.Total() != 0 {
		t.Errorf("failed call scored %+v, want zero", got)
	}
}

func TestScoreUnknownToolIsZero(t *testing.T) {
	if got := Score("edit_file", "claude-desktop", 4000, 0, 0, true); got.Total() != 0 {
		t.Errorf("unmodelled tool scored %+v, want zero", got)
	}
}

func TestScoreCapabilityGatedRead(t *testing.T) {
	// Thin client cannot read files natively: the delivered context is capability.
	thin := Score("read_file", "claude-desktop", 350, 350, 0, true)
	if thin.Capability != 100 || thin.Efficiency != 0 {
		t.Errorf("thin client read_file = %+v, want capability=100 efficiency=0", thin)
	}
	// Capable client doing a whole-file read (baseline == output) saves nothing.
	capable := Score("read_file", "claude-code", 350, 350, 0, true)
	if capable.Total() != 0 {
		t.Errorf("capable client whole-file read = %+v, want zero", capable)
	}
}

func TestScoreRangedReadEfficiencyForCapableClient(t *testing.T) {
	// v4: a capable client's PLAIN ranged read of read_file scores zero — its
	// own native ranged Read reproduces the same byte-range saving, so crediting
	// the delta would be scoring the agent's restraint, not plumb's
	// contribution. See TestScoreModelV4NamedReadKeepsCreditForCapableClient for
	// the name-addressed counterpart (read_symbol), which still earns it.
	got := Score("read_file", "claude-code", 350, 3500, 0, true)
	if got.Total() != 0 {
		t.Errorf("capable client ranged read_file = %+v, want zero (v4: native-reproducible)", got)
	}
	// A thin client is still credited the delivered context regardless of baseline.
	thin := Score("read_file", "claude-desktop", 350, 3500, 0, true)
	if thin.Capability != 100 || thin.Efficiency != 0 {
		t.Errorf("thin ranged read = %+v, want capability=100", thin)
	}
}

func TestScoreSemanticAxisDependsOnReconstructAbility(t *testing.T) {
	// find_references reconstruct=800; 35 code bytes / 3.5 = 10 output tokens → 790.
	capable := Score("find_references", "claude-code", 35, 0, 0, true)
	if capable.Efficiency != 790 || capable.Capability != 0 {
		t.Errorf("capable client find_references = %+v, want efficiency=790", capable)
	}
	thin := Score("find_references", "claude-desktop", 35, 0, 0, true)
	if thin.Capability != 790 || thin.Efficiency != 0 {
		t.Errorf("thin client find_references = %+v, want capability=790", thin)
	}
}

func TestScoreSemanticClampsWhenOutputExceedsReconstruct(t *testing.T) {
	// get_definition reconstruct=250; a huge output makes the delta non-positive → 0.
	if got := Score("get_definition", "claude-code", 100000, 0, 0, true); got.Total() != 0 {
		t.Errorf("oversized get_definition = %+v, want zero", got)
	}
}

func TestScoreSearchCapabilityGated(t *testing.T) {
	thin := Score("search_in_files", "claude-desktop", 350, 0, 0, true)
	if thin.Capability != 100 {
		t.Errorf("thin client search = %+v, want capability=100", thin)
	}
	if got := Score("search_in_files", "claude-code", 350, 0, 0, true); got.Total() != 0 {
		t.Errorf("capable client search = %+v, want zero", got)
	}
}

func TestScoreBatchAvoidsPerCallOverhead(t *testing.T) {
	// 5 files in one call saves 4 round trips of perCallOverhead (80) = 320, for every client.
	got := Score("read_multiple_files", "claude-code", 4000, 0, 5, true)
	if got.Efficiency != perCallOverhead*4 || got.Capability != 0 {
		t.Errorf("batch of 5 = %+v, want efficiency=%d", got, perCallOverhead*4)
	}
	if single := Score("read_multiple_files", "claude-code", 4000, 0, 1, true); single.Total() != 0 {
		t.Errorf("batch of 1 = %+v, want zero", single)
	}
}

// TestClientSideAllowlistEntries pins exactly which clients declare a
// client-side tool allowlist. The flag makes plumb's guidance quieter (lean-set
// names only), so setting it on a client that has no such allowlist would
// silently withhold routing, and failing to set it on one that does would put
// broken pointers in front of a --lean user. Either way it is a reviewed data
// change, not something to acquire by accident — and the set must stay in step
// with `plumb setup <client> --lean`, which internal/cli asserts from its end
// (TestLeanClientsDeclareTheirCapability).
func TestClientCapsClientSideAllowlistEntries(t *testing.T) {
	want := map[string]bool{"codex": true, "gemini": true, "kimi-code": true}
	for _, c := range registry {
		if got := c.ClientSideAllowlist; got != want[c.Name] {
			t.Errorf("%s: ClientSideAllowlist = %v, want %v — plumb writes an allowlist for exactly %v",
				c.Name, got, want[c.Name], []string{"codex", "gemini", "kimi-code"})
		}
	}
	if unknownCaps.ClientSideAllowlist {
		t.Error("an unrecognised client must not be assumed to hold a plumb-written allowlist")
	}
	// The guidance that fires for these clients tells them to use their OWN file
	// search in place of plumb's, which their allowlist may have filtered out
	// (session_start's lastResortSearch). That advice is only sound while every
	// allowlist client actually has native search.
	for _, c := range registry {
		if c.ClientSideAllowlist && !c.NativeSearch {
			t.Errorf("%s declares a client-side allowlist but no native search — session_start's "+
				"fallback would leave it with no discovery at all", c.Name)
		}
	}
}

// TestZCodeRegistryEntry pins the PLAN-369 addition: ZCode is a CLI-style
// coding agent (native file/search/shell, like Claude Code/Codex/Kimi Code),
// but carries no ClientSideAllowlist — setup_zcode.go documents that an
// unrecognised key on ZCode's strict server schema drops the plumb entry
// entirely, so there is no --lean allowlist mechanism to declare.
func TestClientCapsZCodeRegistryEntry(t *testing.T) {
	got := Lookup("zcode")
	if got.Name != "zcode" {
		t.Fatalf(`Lookup("zcode").Name = %q, want "zcode"`, got.Name)
	}
	if !got.NativeFileRead || !got.NativeSearch || !got.NativeShell {
		t.Errorf("zcode = %+v, want native file/search/shell all true", got)
	}
	if got.ClientSideAllowlist {
		t.Error("zcode must not declare ClientSideAllowlist — no --lean mechanism exists for it (setup_zcode.go)")
	}
	if got.SchemaDiscoveryOnly {
		t.Error("zcode must not declare SchemaDiscoveryOnly without measured evidence")
	}
	if got.ReliableDeferredToolDiscovery {
		t.Error("zcode must not declare ReliableDeferredToolDiscovery without measured evidence (G8)")
	}
}

// TestClientCapsSupportsMCPInstructionsEntries pins exactly which clients are
// declared to surface the MCP `initialize` response's `instructions` field —
// first-party OBSERVED evidence only, not a CHANGELOG shipping blurb naming a
// client. Claude Code is the one row with that stronger footing: it is
// dogfooded directly on this codebase, so every Claude Code session sees
// DefaultInstructions and visibly acts on it. Claude Desktop and Gemini CLI
// are also named in the `instructions` field's CHANGELOG entry, but that is
// not an observed result for THIS purpose, so they stay false pending a real
// measurement — same discipline as ReliableDeferredToolDiscovery.
func TestClientCapsSupportsMCPInstructionsEntries(t *testing.T) {
	want := map[string]bool{"claude-code": true}
	for _, c := range registry {
		if got := c.SupportsMCPInstructions; got != want[c.Name] {
			t.Errorf("%s: SupportsMCPInstructions = %v, want %v", c.Name, got, want[c.Name])
		}
	}
	if unknownCaps.SupportsMCPInstructions {
		t.Error("an unrecognised client must not be assumed to surface MCP instructions")
	}
}

// TestSupportsAlwaysLoadPinEntries pins the sole proven case: Claude Code is
// the only client with shipped evidence it reads the proprietary
// `_meta["anthropic/alwaysLoad"]=true` tools/list extension PLAN-355
// introduced. Every other client, including its own claude-desktop sibling,
// must stay false absent the same kind of evidence — the flag is declared data
// only in this PR (conn_register.go still pins unconditionally for everyone).
func TestClientCapsSupportsAlwaysLoadPinEntries(t *testing.T) {
	for _, c := range registry {
		want := c.Name == "claude-code"
		if got := c.SupportsAlwaysLoadPin; got != want {
			t.Errorf("%s: SupportsAlwaysLoadPin = %v, want %v", c.Name, got, want)
		}
	}
	if unknownCaps.SupportsAlwaysLoadPin {
		t.Error("an unrecognised client must not be assumed to read the AlwaysLoad pin extension")
	}
}

// measuredDescriptionCapRunes names the registry rows whose DescriptionCapRunes
// is a reviewed measurement, and the value (PLAN-370). claude-code is the one
// entry so far: see the field's doc comment in clientcaps.go for the
// live-truncation evidence. A row absent from this map must stay at zero.
var measuredDescriptionCapRunes = map[string]int{
	"claude-code": 2048,
}

// TestClientCapsDescriptionCapRunesUnmeasured guards the evidence-gate
// DescriptionCapRunes' own doc comment promises: every row's value must equal
// its reviewed measurement (measuredDescriptionCapRunes), or zero if it has
// none — never a guessed number, and never a value that drifted from the
// measurement on record.
func TestClientCapsDescriptionCapRunesUnmeasured(t *testing.T) {
	for _, c := range registry {
		want := measuredDescriptionCapRunes[c.Name]
		if c.DescriptionCapRunes != want {
			t.Errorf("%s: DescriptionCapRunes = %d, want %d — populate only from a reviewed measurement, recorded in measuredDescriptionCapRunes",
				c.Name, c.DescriptionCapRunes, want)
		}
	}
	if unknownCaps.DescriptionCapRunes != 0 {
		t.Errorf("unknownCaps.DescriptionCapRunes = %d, want 0", unknownCaps.DescriptionCapRunes)
	}
}

// TestStrictestDescriptionCapRunes pins the fallback a description-conformance
// check applies to an unmeasured client: the smallest nonzero cap on record,
// currently claude-code's 2048 (the only measured row). Guards against a
// silent widening if a stricter client is measured later without this test
// being revisited, and against StrictestDescriptionCapRunes drifting to 0
// ("nothing to enforce") while a measured row exists.
func TestStrictestDescriptionCapRunes(t *testing.T) {
	const want = 2048
	if got := StrictestDescriptionCapRunes(); got != want {
		t.Errorf("StrictestDescriptionCapRunes() = %d, want %d", got, want)
	}
}

// TestAll_MatchesRegistry pins All() to the registry it copies from, so a
// caller iterating All() is provably iterating every registered client.
func TestAll_MatchesRegistry(t *testing.T) {
	all := All()
	if len(all) != len(registry) {
		t.Fatalf("All() returned %d entries, registry has %d", len(all), len(registry))
	}
	for i, c := range all {
		if c.Name != registry[i].Name {
			t.Errorf("All()[%d].Name = %q, want %q", i, c.Name, registry[i].Name)
		}
	}
}
