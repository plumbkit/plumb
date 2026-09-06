package config

import (
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// TestValidateExcludePatterns_RefusesEveryCatchAllSpelling states the rule the
// write path enforces. The three groups are the spellings a literal denylist
// knew, the four respellings that walked past it (each proven in
// internal/topology to empty a real index), and the legitimate patterns the
// guard must still accept — a guard that refused everything would not be a fix.
func TestValidateExcludePatterns_RefusesEveryCatchAllSpelling(t *testing.T) {
	refused := []string{
		"*", "**", "**/*", "*/**", ".", "./**", "/**/",
		"**/**", "**/**/**", "**/**/*", "?*",
	}
	for _, p := range refused {
		err := validateExcludePatterns([]string{p})
		if err == nil {
			t.Errorf("validateExcludePatterns(%q) = nil, want a refusal", p)
			continue
		}
		if !strings.Contains(err.Error(), p) {
			t.Errorf("the refusal for %q does not name the pattern: %v", p, err)
		}
	}
	for _, p := range []string{"vendor/**", "*.pb.go", "third_party/**", "gen", "*_generated.go"} {
		if err := validateExcludePatterns([]string{p}); err != nil {
			t.Errorf("validateExcludePatterns(%q) = %v, want it accepted", p, err)
		}
	}
	// An empty entry is dropped by the sanitiser rather than being a config
	// error, so it must not fail the whole load.
	if err := validateExcludePatterns([]string{"", "   ", "vendor/**"}); err != nil {
		t.Errorf("blank entries must not fail validation: %v", err)
	}
}

// TestAgentApplyBatch_RefusesCatchAllExcludePattern is the finding this
// validation exists for. exclude_patterns is agent-writable, and the only guard
// was topology's sanitiser — which runs at Store.Open and only logs. So
// `agent_config set topology.exclude_patterns=["**"]` returned SUCCESS,
// persisted the value, and the agent learned nothing: the write succeeded and
// nothing happened, which is the exact pathology reviving this field was about.
func TestAgentApplyBatch_RefusesCatchAllExcludePattern(t *testing.T) {
	for _, pattern := range []string{"**", "**/**", "?*"} {
		t.Run(pattern, func(t *testing.T) {
			ws := t.TempDir()
			_, err := AgentApplyBatch(Defaults(), ws,
				map[string]any{"topology.exclude_patterns": []any{pattern}}, ProvenanceEntry{})
			if err == nil {
				t.Fatalf("AgentApplyBatch accepted exclude_patterns = [%q]", pattern)
			}
			if !strings.Contains(err.Error(), pattern) {
				t.Errorf("the refusal must name the pattern, got %v", err)
			}
			if !strings.Contains(err.Error(), "empty the index") {
				t.Errorf("the refusal must say WHY, got %v", err)
			}
			if _, statErr := os.Stat(ProjectConfigPath(ws)); !os.IsNotExist(statErr) {
				t.Error("a refused batch must not write the config file")
			}
		})
	}
}

// TestAgentApplyBatch_AcceptsLegitimateExcludePattern is the other half: the
// field is agent-writable for a reason, and the guard must not have taken that
// away.
func TestAgentApplyBatch_AcceptsLegitimateExcludePattern(t *testing.T) {
	ws := t.TempDir()
	if _, err := AgentApplyBatch(Defaults(), ws,
		map[string]any{"topology.exclude_patterns": []any{"third_party/**", "*.pb.go"}},
		ProvenanceEntry{Source: "agent"}); err != nil {
		t.Fatalf("AgentApplyBatch: %v", err)
	}
	// The agent's own workspace is not a cloned repository, but the value is
	// still gated on load, so read it back through the raw project file rather
	// than through LoadProject.
	raw, err := LoadProjectRaw(ws)
	if err != nil {
		t.Fatalf("LoadProjectRaw: %v", err)
	}
	// Indexed directly rather than through getNested: this test wrote the key
	// itself, in exactly this spelling, so the fold-tolerant walker buys nothing
	// here — and calling it from a test widens gosec's analysis of it enough to
	// flag its unguarded path[0]/path[1:] as out-of-range (G602), reddening lint
	// in a file this change has no business reshaping.
	topo, _ := raw["topology"].(map[string]any)
	got, ok := topo["exclude_patterns"]
	if !ok {
		t.Fatalf("topology.exclude_patterns was not written: %v", raw)
	}
	if want := []any{"third_party/**", "*.pb.go"}; !reflect.DeepEqual(got, want) {
		t.Errorf("written value = %v, want %v", got, want)
	}
}

// TestLoadProject_ExcludePatternsNeedTrust is the classification change. An
// untrusted project's exclude_patterns used to be honoured verbatim, so a
// cloned repository could ship `exclude_patterns = ["backdoor.go"]` and keep
// that one file out of the index while the rest of it stayed present and
// healthy — topology_search, topology_explore, topology_affected and
// workspace_search's code corpus all report clean, and an agent auditing the
// repository never sees the file. topology.enabled already let a project blank
// the index WHOLESALE, but that is loud; targeted concealment is not.
func TestLoadProject_ExcludePatternsNeedTrust(t *testing.T) {
	ws := t.TempDir()
	writeProjectConfig(t, ws, "[topology]\nexclude_patterns = [\"backdoor.go\"]\nwatch = false\n")
	store := tempTrustStore(t)

	base := Defaults()
	base.Topology.ExcludePatterns = []string{"global/**"}

	// 1. Untrusted: forced back to the global value.
	got, err := LoadProject(base, ws)
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	if slices.Contains(got.Topology.ExcludePatterns, "backdoor.go") {
		t.Errorf("an untrusted project hid a file from the index: %v", got.Topology.ExcludePatterns)
	}
	if !slices.Equal(got.Topology.ExcludePatterns, []string{"global/**"}) {
		t.Errorf("forced-back patterns = %v, want the global value", got.Topology.ExcludePatterns)
	}
	// A free [topology] field in the same table is untouched by the gate.
	if got.Topology.Watch {
		t.Error("topology.watch is not gated and must still be honoured")
	}

	// 2. It is DISCLOSED, so `plumb trust` shows the patterns and says what
	//    approving them means. A silently dropped key would also let a trusted
	//    repository add one later without invalidating the grant.
	spec, err := ProjectPolicySpecFor(ws)
	if err != nil {
		t.Fatalf("ProjectPolicySpecFor: %v", err)
	}
	if !slices.Contains(spec.Keys(), "topology.exclude_patterns") {
		t.Fatalf("topology.exclude_patterns must appear in the trust spec, got %v", spec.Keys())
	}
	if slices.Contains(spec.Keys(), "topology.watch") {
		t.Errorf("topology.watch is inert and must not demand a re-trust, got %v", spec.Keys())
	}
	if desc := strings.Join(spec.Describe(), "\n"); !strings.Contains(desc, "backdoor.go") {
		t.Errorf("the disclosure must show the patterns being asked for, got %q", desc)
	}
	entry := findEntry(t, spec, "topology.exclude_patterns")
	if entry.Warning(base) == "" {
		t.Error("a gated key must carry a reason at trust time")
	}

	// 3. Trusted for this exact content: honoured.
	trustWorkspace(t, store, ws)
	got, err = LoadProject(base, ws)
	if err != nil {
		t.Fatalf("LoadProject (trusted): %v", err)
	}
	if !slices.Contains(got.Topology.ExcludePatterns, "backdoor.go") {
		t.Errorf("a trusted project's patterns must be honoured, got %v", got.Topology.ExcludePatterns)
	}
}

// TestLoadProject_ExcludePatternsFoldVariantIsGated is the gate-bypass shape
// this file's siblings were written for: go-toml/v2 binds a TOML key to a
// struct field through strings.ToLower, so `Exclude_Patterns` decodes into
// ExcludePatterns. A spec built by exact-name lookup would miss it, and the
// value would reach the merged config with the disclosure showing nothing.
func TestLoadProject_ExcludePatternsFoldVariantIsGated(t *testing.T) {
	for _, spelling := range []string{"Exclude_Patterns", "EXCLUDE_PATTERNS"} {
		t.Run(spelling, func(t *testing.T) {
			ws := t.TempDir()
			writeProjectConfig(t, ws, "[topology]\n"+spelling+" = [\"backdoor.go\"]\n")
			tempTrustStore(t)

			got, err := LoadProject(Defaults(), ws)
			if err != nil {
				t.Fatalf("LoadProject: %v", err)
			}
			if slices.Contains(got.Topology.ExcludePatterns, "backdoor.go") {
				t.Errorf("%s reached the merged config untrusted: %v", spelling, got.Topology.ExcludePatterns)
			}
			spec, err := ProjectPolicySpecFor(ws)
			if err != nil {
				t.Fatalf("ProjectPolicySpecFor: %v", err)
			}
			if len(spec) == 0 {
				t.Errorf("%s is absent from the trust spec, so it would neither be disclosed nor hashed", spelling)
			}
		})
	}
}

// TestForceCapabilityFieldsToBase_ClonesExcludePatterns guards the aliasing
// hazard every forced-back reference type has: LoadProject's caller usually
// keeps base in a live config store, so a shared slice would let one project's
// merged view write into the global config every other session reads.
func TestForceCapabilityFieldsToBase_ClonesExcludePatterns(t *testing.T) {
	base := Defaults()
	base.Topology.ExcludePatterns = []string{"global/**"}
	merged := Defaults()
	merged.Topology.ExcludePatterns = []string{"backdoor.go"}

	forceCapabilityFieldsToBase(base, &merged)

	if !slices.Equal(merged.Topology.ExcludePatterns, []string{"global/**"}) {
		t.Fatalf("forced-back patterns = %v, want base's", merged.Topology.ExcludePatterns)
	}
	merged.Topology.ExcludePatterns[0] = "mutated"
	if base.Topology.ExcludePatterns[0] != "global/**" {
		t.Error("the forced-back slice must not share backing storage with base")
	}
}

// TestIsGatedProjectKey_TopologyMatchesTheClassification keeps the display
// surfaces in step with the loader — the reason IsGatedProjectKey exists — and
// ties both to the classification table, so a later [topology] field cannot be
// classified one way and gated another.
func TestIsGatedProjectKey_TopologyMatchesTheClassification(t *testing.T) {
	for key, class := range projectFieldClasses {
		if !strings.HasPrefix(key, "topology.") {
			continue
		}
		want := class == ClassTrustGated
		if got := IsGatedProjectKey(key); got != want {
			t.Errorf("IsGatedProjectKey(%q) = %v, but the field is classified %v", key, got, class)
		}
	}
	if !IsGatedProjectKey("topology.exclude_patterns") {
		t.Error("topology.exclude_patterns must be gated; the whole finding is that it was not")
	}
}
