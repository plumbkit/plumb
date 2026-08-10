package tools

import (
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/topology"
)

const genericHeader = "## Tool guidance\n"

// genericGuidance renders the session guidance for clientName at the given
// profile and topology state.
func genericGuidance(clientName, profile string, topologyOn bool) string {
	s := &SessionStart{
		clientNameFn: func() string { return clientName },
		toolProfile:  func() (string, int, string) { return profile, 0, "test" },
	}
	if topologyOn {
		s.topo = func() *topology.Store { return &topology.Store{} }
	}
	var sb strings.Builder
	s.writeSessionGuidance(&sb)
	return sb.String()
}

// TestGenericGuidance_CoversEveryClientWithNoBespokeBlock is the point of the
// block. Eleven of plumb's fourteen setup targets used to fall through
// writeSessionGuidance's switch and receive nothing but the profile note — and
// they are exactly the clients that install no skills, so when the routing left
// the tool descriptions they were left with no steering channel at all.
//
// The negative half pins that the three clients WITH a bespoke block keep it:
// the generic block is a fallback, not an addition, and emitting both would pay
// for the same advice twice in every session.
func TestGenericGuidance_CoversEveryClientWithNoBespokeBlock(t *testing.T) {
	// The eleven `plumb setup` targets with no bespoke block, then the two
	// no-client cases: an empty clientInfo.name and one plumb does not know.
	fallsThrough := []string{
		"codex", "gemini", "cursor", "augment", "qwen",
		"antigravity", "antigravity-desktop", "opencode", "crush", "goose", "hermes",
		"", "some-agent-nobody-has-heard-of",
	}
	for _, name := range fallsThrough {
		t.Run("generic/"+name, func(t *testing.T) {
			if out := genericGuidance(name, "full", true); !strings.Contains(out, genericHeader) {
				t.Errorf("client %q got no guidance block at all:\n%s", name, out)
			}
		})
	}

	bespoke := map[string]string{
		"claude-code": "## Tool guidance (Claude Code)",
		"claude-ai":   "## Tool guidance (Claude Desktop)",
		"kimi-code":   "## Tool guidance (Kimi Code)",
	}
	for name, header := range bespoke {
		t.Run("bespoke/"+name, func(t *testing.T) {
			out := genericGuidance(name, "full", true)
			if !strings.Contains(out, header) {
				t.Errorf("client %q lost its bespoke block:\n%s", name, out)
			}
			if strings.Contains(out, genericHeader) {
				t.Errorf("client %q got the generic block as well as its own:\n%s", name, out)
			}
		})
	}
}

// TestGenericGuidance_RestoresTheRoutingTheDRYPassRemoved is the positive half:
// the block must actually carry the routing that left the tool descriptions,
// not merely exist. Each name here was in a description's comparative-routing
// prose before the DRY pass.
func TestGenericGuidance_RestoresTheRoutingTheDRYPassRemoved(t *testing.T) {
	// Cursor, not Codex: a client that can hold a plumb-written allowlist in its
	// own config takes the lean-names-only branch even on a full profile (see
	// TestGenericGuidance_AllowlistClientNamesLeanToolsOnly). Cursor cannot, so it
	// is the client that exercises the full ladder this test is about.
	out := genericGuidance("cursor", "full", true)
	for _, want := range []string{
		"workspace_search", "search_in_files", "get_definition", "find_references",
		"topology_affected", "read_file", "edit_file", "expected_mtime",
		"rename_symbol", "diagnostics", "run_task",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("generic guidance does not name %q:\n%s", want, out)
		}
	}
}

// TestGenericGuidance_LeanSetOnly holds the same property the Kimi Code block
// does, for the same reason: guidance must never steer an agent at a tool its
// resolved profile hides from tools/list. The forbidden set is derived from
// nonLeanToolSet(), so a tool leaving LeanTools fails here rather than silently
// turning a line of guidance into a broken pointer.
func TestGenericGuidance_LeanSetOnly(t *testing.T) {
	for _, topologyOn := range []bool{true, false} {
		// Cursor isolates the PROFILE trigger: it holds no client-side allowlist,
		// so the restriction here can only come from the resolved lean profile.
		out := genericGuidance("cursor", "lean", topologyOn)
		if !strings.Contains(out, genericHeader) {
			t.Fatalf("topology=%v: generic block missing:\n%s", topologyOn, out)
		}
		for _, tl := range nonLeanToolSet() {
			if strings.Contains(out, tl.Name()) {
				t.Errorf("topology=%v: generic guidance names non-lean tool %q:\n%s",
					topologyOn, tl.Name(), out)
			}
		}
	}
}

// TestGenericGuidance_TopologyOffDropsTheMap pins the off-branch: with no
// topology store the index tools must not be advertised (those calls would
// fail), and the block points at enabling the index instead.
func TestGenericGuidance_TopologyOffDropsTheMap(t *testing.T) {
	out := genericGuidance("cursor", "full", false)
	if !strings.Contains(out, "[topology] enabled = true") {
		t.Fatalf("topology-off block does not point at enabling the index:\n%s", out)
	}
	// The enable tip legitimately names topology_affected — it is describing what
	// turning the index on would buy. What must not happen is the ADVICE offering
	// an index tool as a move to make now, so the ban applies to everything above
	// the tip.
	advice := out[:strings.Index(out, "Tip:")]
	for _, banned := range []string{"topology_affected", "topology_search"} {
		if strings.Contains(advice, banned) {
			t.Errorf("topology is off but the advice offers %q:\n%s", banned, out)
		}
	}
	// The non-index routing must survive the topology-off branch — that is the
	// half the DRY pass removed, and it does not depend on the index.
	for _, want := range []string{"workspace_search", "get_definition", "expected_mtime", "run_task"} {
		if !strings.Contains(out, want) {
			t.Errorf("topology-off block dropped %q:\n%s", want, out)
		}
	}
}

// allowlistClients are the clientInfo.name values that resolve to a clientcaps
// entry declaring ClientSideAllowlist — the clients `plumb setup <client> --lean`
// can write a tool allowlist for. Versioned and alias forms are included because
// that is what a real handshake reports.
var allowlistClients = []string{"codex", "codex/0.5.1", "gemini", "gemini-cli/1.2"}

// TestGenericGuidance_AllowlistClientNamesLeanToolsOnly is the Codex/Gemini
// equivalent of TestKimiCodeGuidance_LeanSetOnly, and it is the property that
// was missing: these clients hold their filter in their OWN config, which plumb
// cannot see, so the server-side profile (always "full" for them) says nothing
// about what is callable. Guidance keyed on the profile therefore steered a
// --lean Codex user at workspace_search and search_in_files — tools their own
// enabled_tools list had removed.
//
// The forbidden set is derived from nonLeanToolSet(), not hand-picked, so a tool
// leaving LeanTools fails here instead of silently becoming a broken pointer.
// Both profiles are exercised: "full" is the one that actually ships for these
// clients, and it is the state the bug lived in.
func TestGenericGuidance_AllowlistClientNamesLeanToolsOnly(t *testing.T) {
	for _, client := range allowlistClients {
		for _, profile := range []string{"full", "lean"} {
			for _, topologyOn := range []bool{true, false} {
				out := genericGuidance(client, profile, topologyOn)
				if !strings.Contains(out, genericHeader) {
					t.Fatalf("%s/%s/topology=%v: generic block missing:\n%s", client, profile, topologyOn, out)
				}
				for _, tl := range nonLeanToolSet() {
					if strings.Contains(out, tl.Name()) {
						t.Errorf("%s/%s/topology=%v: guidance names non-lean tool %q — a client-side "+
							"allowlist would have removed it:\n%s", client, profile, topologyOn, tl.Name(), out)
					}
				}
			}
		}
	}
}

// TestGenericGuidance_AllowlistClientStillGetsRouting is the positive half: the
// restriction must thin the routing, not delete it. A --lean client still needs
// to be told where to start.
func TestGenericGuidance_AllowlistClientStillGetsRouting(t *testing.T) {
	withMap := genericGuidance("codex", "full", true)
	for _, want := range []string{"topology_search", "topology_affected", "get_definition", "find_references"} {
		if !strings.Contains(withMap, want) {
			t.Errorf("topology-on guidance dropped %q:\n%s", want, withMap)
		}
	}
	noMap := genericGuidance("codex", "full", false)
	for _, want := range []string{"workspace_symbols", "get_definition", "expected_mtime", "run_task"} {
		if !strings.Contains(noMap, want) {
			t.Errorf("topology-off guidance dropped %q:\n%s", want, noMap)
		}
	}
}

// TestRecommendedStart_LastResortSearch covers the OTHER place the rule bites:
// the no-language-server, no-index fallbacks, which are the only guidance in
// plumb that names find_files/search_in_files as the move to make now. For a
// client that may have filtered them out, they name no plumb tool at all —
// pointing at the client's own search, which every allowlist-capable client has.
// Every other client keeps them: Claude Desktop in particular has no native
// search to fall back on.
func TestRecommendedStart_LastResortSearch(t *testing.T) {
	render := func(client, lang, lspKey string) string {
		s := &SessionStart{
			clientNameFn: func() string { return client },
			toolProfile:  func() (string, int, string) { return "full", 0, "test" },
		}
		var sb strings.Builder
		s.writeSessionRecommendedStart(&sb, false, lang, lspKey)
		return sb.String()
	}
	routed := func(client string) string {
		s := (&SessionStart{
			clientNameFn: func() string { return client },
			toolProfile:  func() (string, int, string) { return "full", 0, "test" },
		}).WithLSPRouted(func() []string { return []string{"go"} })
		var sb strings.Builder
		s.writeSessionRecommendedStart(&sb, false, "", "")
		return sb.String()
	}

	// namesSearch marks the branches that offer a search of last resort at all.
	// The opt-in-adapter branch names none for any client — it points at enabling
	// the index instead — so it can only be checked for what it must NOT say.
	cases := []struct {
		label, lang, lspKey string
		namesSearch         bool
	}{
		{"unrecognised project", "", "", true},
		{"no adapter for the language", "Elixir", "", true},
		{"default-on server missing", "Go", "go", true},
		{"opt-in adapter", "Kotlin", "kotlin", false},
	}
	for _, client := range allowlistClients {
		for _, c := range cases {
			out := render(client, c.lang, c.lspKey)
			for _, banned := range []string{"search_in_files", "find_files"} {
				if strings.Contains(out, banned) {
					t.Errorf("%s/%s: fallback names %q, which a client-side allowlist removes:\n%s",
						client, c.label, banned, out)
				}
			}
			if c.namesSearch && !strings.Contains(out, "your client's own file search") {
				t.Errorf("%s/%s: fallback leaves the agent with no discovery at all:\n%s", client, c.label, out)
			}
		}
		if out := routed(client); strings.Contains(out, "search_in_files") {
			t.Errorf("%s: the per-file-routing fallback names a tool the allowlist removes:\n%s", client, out)
		}
	}

	// The control: a client that cannot hold an allowlist keeps the plumb tools,
	// which are its only discovery left in this state.
	for _, c := range cases {
		if !c.namesSearch {
			continue
		}
		if out := render("claude-ai", c.lang, c.lspKey); !strings.Contains(out, "search_in_files") &&
			!strings.Contains(out, "find_files") {
			t.Errorf("claude-desktop/%s: lost the plumb search fallback it has no native replacement for:\n%s",
				c.label, out)
		}
	}
}
