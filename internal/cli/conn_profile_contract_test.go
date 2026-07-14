package cli

// conn_profile_contract_test.go consolidates the client-adapter contract
// matrix's tool-profile-negotiation coverage: for a representative spread of
// client/config combinations, resolveToolProfile must report the documented
// (profile, reason) pair, every bootstrap tool must stay visible regardless of
// profile, and the visible set must be exactly LeanTools ∪ BootstrapTools under
// lean (today == LeanTools, since bootstrap ⊆ lean — see TestBootstrapToolsAreLean
// in internal/tools/profile_test.go) or admit everything under full.
//
// This file only asserts behaviour already proven correct by conn_profile_test.go
// (TestResolveToolProfile, TestAutoProfileFor, TestToolVisible_*) — it is a
// consolidated contract view over that existing coverage, not a duplicate of it.

import (
	"testing"

	"github.com/plumbkit/plumb/internal/clientcaps"
	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/tools"
)

// nonLeanSample is a small, representative set of registered commodity tools
// that are NOT in tools.LeanTools. Checking these stay hidden under lean (and
// visible under full) is what proves the admitted set is exactly LeanTools —
// not merely a superset of it.
var nonLeanSample = []string{"copy_file", "list_files", "list_directory", "call_hierarchy", "workspace_search"}

func init() {
	// Guard the fixture: if one of these is ever promoted into LeanTools, the
	// "exactness" assertions below would silently stop proving anything useful.
	for _, n := range nonLeanSample {
		if tools.IsLean(n) {
			panic("conn_profile_contract_test: nonLeanSample entry " + n + " is now in tools.LeanTools — update the sample")
		}
	}
}

// assertLeanExact asserts s admits exactly LeanTools ∪ BootstrapTools: every
// lean tool is visible, and the representative non-lean sample is hidden.
func assertLeanExact(t *testing.T, s *connSession) {
	t.Helper()
	for name := range tools.LeanTools {
		if !s.toolVisible(name) {
			t.Errorf("lean profile must admit lean tool %q", name)
		}
	}
	for _, name := range nonLeanSample {
		if s.toolVisible(name) {
			t.Errorf("lean profile must hide commodity tool %q", name)
		}
	}
}

// assertFullAdmitsEverything asserts s admits both the lean set and the
// non-lean sample — the full profile hides nothing.
func assertFullAdmitsEverything(t *testing.T, s *connSession) {
	t.Helper()
	for name := range tools.LeanTools {
		if !s.toolVisible(name) {
			t.Errorf("full profile must admit lean tool %q", name)
		}
	}
	for _, name := range nonLeanSample {
		if !s.toolVisible(name) {
			t.Errorf("full profile must admit commodity tool %q", name)
		}
	}
}

// assertBootstrapAlwaysVisible asserts every bootstrap tool is visible on s,
// independent of the resolved profile.
func assertBootstrapAlwaysVisible(t *testing.T, s *connSession) {
	t.Helper()
	for name := range tools.BootstrapTools {
		if !s.toolVisible(name) {
			t.Errorf("bootstrap tool %q must always be visible", name)
		}
	}
}

// TestClientProfileContractMatrix walks the card's client/config test matrix:
// auto+unknown, auto+codex, auto+claude-code, explicit lean, client-override
// lean, and a synthetic verified-deferred-discovery client.
func TestClientProfileContractMatrix(t *testing.T) {
	t.Run("auto + unknown client", func(t *testing.T) {
		s := newProfileSession(t, config.ToolsConfig{Profile: "auto"}, "a-client-nobody-registered")
		profile, reason := s.resolveToolProfile()
		if profile != "full" || reason != "unknown-deferred-discovery" {
			t.Fatalf("resolveToolProfile() = (%q, %q), want (\"full\", \"unknown-deferred-discovery\")", profile, reason)
		}
		assertBootstrapAlwaysVisible(t, s)
		assertFullAdmitsEverything(t, s)
	})

	t.Run("auto + codex", func(t *testing.T) {
		s := newProfileSession(t, config.ToolsConfig{Profile: "auto"}, "codex")
		profile, reason := s.resolveToolProfile()
		if profile != "full" || reason != "unverified-deferred-discovery" {
			t.Fatalf("resolveToolProfile() = (%q, %q), want (\"full\", \"unverified-deferred-discovery\")", profile, reason)
		}
		assertBootstrapAlwaysVisible(t, s)
		assertFullAdmitsEverything(t, s)
	})

	t.Run("auto + claude-code", func(t *testing.T) {
		s := newProfileSession(t, config.ToolsConfig{Profile: "auto"}, "claude-code")
		profile, reason := s.resolveToolProfile()
		if profile != "full" || reason != "schema-discovery-only-client" {
			t.Fatalf("resolveToolProfile() = (%q, %q), want (\"full\", \"schema-discovery-only-client\")", profile, reason)
		}
		assertBootstrapAlwaysVisible(t, s)
		assertFullAdmitsEverything(t, s)
	})

	t.Run("explicit lean", func(t *testing.T) {
		s := newProfileSession(t, config.ToolsConfig{Profile: "lean"}, "claude-code")
		profile, reason := s.resolveToolProfile()
		if profile != "lean" || reason != "explicit-config" {
			t.Fatalf("resolveToolProfile() = (%q, %q), want (\"lean\", \"explicit-config\")", profile, reason)
		}
		assertBootstrapAlwaysVisible(t, s)
		assertLeanExact(t, s)
	})

	t.Run("client-override lean", func(t *testing.T) {
		s := newProfileSession(t, config.ToolsConfig{Profile: "full", ClientProfiles: map[string]string{"claude-code": "lean"}}, "claude-code")
		profile, reason := s.resolveToolProfile()
		if profile != "lean" || reason != "client-override" {
			t.Fatalf("resolveToolProfile() = (%q, %q), want (\"lean\", \"client-override\")", profile, reason)
		}
		assertBootstrapAlwaysVisible(t, s)
		assertLeanExact(t, s)
	})

	t.Run("synthetic verified-deferred-discovery client", func(t *testing.T) {
		// No REAL client in the clientcaps registry has ReliableDeferredToolDiscovery
		// set yet (see clientcaps.go's registry — Codex and Gemini both leave it
		// false pending integration coverage), so this outcome cannot be reached
		// through a registered client name today. Drive the pure policy function
		// directly with a synthetic Capabilities value, matching TestAutoProfileFor.
		profile, reason := autoProfileFor(clientcaps.Capabilities{Name: "future-verified-client", ReliableDeferredToolDiscovery: true})
		if profile != "lean" || reason != "verified-deferred-discovery" {
			t.Fatalf("autoProfileFor(verified) = (%q, %q), want (\"lean\", \"verified-deferred-discovery\")", profile, reason)
		}
		// Exercise toolVisible under the SAME resolved profile via an explicit "lean"
		// config — the admitted-set behaviour depends only on the resolved profile,
		// not on the auto-mode reasoning that reached it, and no registered client
		// can reach "lean" through auto mode yet.
		s := newProfileSession(t, config.ToolsConfig{Profile: profile}, "future-verified-client")
		assertBootstrapAlwaysVisible(t, s)
		assertLeanExact(t, s)
	})
}
