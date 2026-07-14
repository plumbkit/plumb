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
		{"codex", "codex", true},
		{"gemini-cli", "gemini", true},
		{"totally-unknown-xyz", "unknown", true}, // conservative default
		{"", "unknown", true},
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
// Claude Code is the one flagged true (it builds its tool/ToolSearch list from
// tools/list, so a lean-hidden tool is unreachable); the other CLI agents and the
// unknown fallback are false; lean eligibility is gated separately on
// ReliableDeferredToolDiscovery.
func TestSchemaDiscoveryOnly(t *testing.T) {
	tests := []struct {
		client string
		want   bool
	}{
		{"claude-code", true},
		{"claude-code/1.2.3", true},
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
	// Capable client, ranged read of a 3500-byte file returning 350 bytes of code
	// (claude ratio 3.5): baseline 1000 tokens − returned 100 = 900 efficiency.
	got := Score("read_file", "claude-code", 350, 3500, 0, true)
	if got.Efficiency != 900 || got.Capability != 0 {
		t.Errorf("ranged read = %+v, want efficiency=900", got)
	}
	// A thin client is credited the delivered context regardless of baseline.
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
