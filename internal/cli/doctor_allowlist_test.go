package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/tools"
)

// gradeFixture is a stable stand-in for the live sets, so the table below states
// its own facts instead of tracking whatever LeanTools holds today.
var (
	gradeRegistered = []string{"a_tool", "b_tool", "c_tool", "d_tool", "e_tool", "extra_tool"}
	gradePinned     = []string{"a_tool", "b_tool", "c_tool", "d_tool", "e_tool"}
)

// TestGradeToolAllowlist covers the grades a client-side allowlist can earn.
// The cases that matter are the ones a shape-only check called healthy: a list
// of empty strings, a list of numbers, and a list of plausible-but-invented tool
// names are all well-formed non-empty JSON arrays that leave the client with
// zero plumb tools.
func TestGradeToolAllowlist(t *testing.T) {
	for _, tc := range []struct {
		name        string
		raw         any
		want        allowlistVerdict
		wantShape   string
		wantUnknown []string
		wantMissing []string
	}{
		{name: "null", raw: nil, want: allowlistDegenerate, wantShape: "null"},
		{name: "empty list", raw: []any{}, want: allowlistDegenerate, wantShape: "an empty list"},
		{name: "not a list", raw: "a_tool", want: allowlistDegenerate, wantShape: "not a list (string)"},
		{name: "only empty strings", raw: []any{"", "  "}, want: allowlistDegenerate, wantShape: "a list holding no tool name"},
		{name: "only nulls", raw: []any{nil}, want: allowlistDegenerate, wantShape: "a list holding no tool name"},
		{name: "only numbers", raw: []any{1.0, 2.0, 3.0}, want: allowlistDegenerate, wantShape: "a list holding no tool name"},

		{
			name: "no name is a plumb tool", raw: []any{"not_a_plumb_tool", "nope"},
			want: allowlistUnrecognised, wantUnknown: []string{"not_a_plumb_tool", "nope"},
		},

		{name: "exactly the pinned set", raw: anyList(gradePinned...), want: allowlistUsable},
		{
			name: "pinned set plus a deliberate addition", raw: anyList("a_tool", "b_tool", "c_tool", "d_tool", "e_tool", "extra_tool"),
			want: allowlistUsable,
		},
		// missing is still computed here (driftVerdict needs it); it is only
		// REPORTED for a list that reads as plumb's own aged snapshot.
		{
			name: "hand-picked subset is the user's business", raw: anyList("a_tool", "b_tool"),
			want: allowlistUsable, wantMissing: []string{"c_tool", "d_tool", "e_tool"},
		},

		{
			name: "aged snapshot missing a pinned name", raw: anyList("a_tool", "b_tool", "c_tool", "d_tool"),
			want: allowlistStale, wantMissing: []string{"e_tool"},
		},
		{
			name: "aged snapshot naming a retired tool", raw: anyList("a_tool", "b_tool", "c_tool", "d_tool", "e_tool", "retired_tool"),
			want: allowlistStale, wantUnknown: []string{"retired_tool"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := gradeToolAllowlist(tc.raw, gradeRegistered, gradePinned)
			if got.verdict != tc.want {
				t.Fatalf("verdict = %v, want %v (grade %+v)", got.verdict, tc.want, got)
			}
			if got.shape != tc.wantShape {
				t.Errorf("shape = %q, want %q", got.shape, tc.wantShape)
			}
			if !sameNames(got.unknown, tc.wantUnknown) {
				t.Errorf("unknown = %v, want %v", got.unknown, tc.wantUnknown)
			}
			if !sameNames(got.missing, tc.wantMissing) {
				t.Errorf("missing = %v, want %v", got.missing, tc.wantMissing)
			}
		})
	}
}

// TestKimiLeanHintAt_GradesContent is the end-to-end half: the same defects,
// driven through a real config file, must reach `plumb doctor` with the right
// severity. A list that filters plumb to nothing is a WARNING with a fix (the
// user cannot see the breakage any other way); an aged snapshot is
// INFORMATIONAL (it still works, and the user may be pinning it deliberately).
func TestKimiLeanHintAt_GradesContent(t *testing.T) {
	const bin = "/usr/local/bin/plumb"

	for _, tc := range []struct {
		name      string
		allowlist any
		wantIn    string
	}{
		{"empty strings", []any{"", ""}, "NO plumb tools"},
		{"numbers", []any{1.0, 2.0}, "NO plumb tools"},
		{"invented names", []any{"not_a_plumb_tool"}, "not_a_plumb_tool"},
	} {
		t.Run("filters to nothing: "+tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "mcp.json")
			writeKimiAllowlist(t, path, bin, tc.allowlist)

			res, ok := kimiLeanHintAt(path)
			if !ok {
				t.Fatalf("an allowlist matching no plumb tool leaves the server inert — doctor must not stay silent")
			}
			if !res.warn || !res.ok {
				t.Errorf("want a non-fatal warning (ok=true warn=true), got %+v", res)
			}
			if res.fix == "" {
				t.Error("a warning must carry a fix line — it is the only part that renders on attention")
			}
			if !strings.Contains(res.detail, tc.wantIn) {
				t.Errorf("detail %q should mention %q", res.detail, tc.wantIn)
			}
		})
	}

	t.Run("an aged snapshot is an informational drift hint", func(t *testing.T) {
		lean := tools.LeanToolNames()
		stale := anyList(lean[:len(lean)-1]...) // written before the lean set gained its last name
		path := filepath.Join(t.TempDir(), "mcp.json")
		writeKimiAllowlist(t, path, bin, stale)

		res, ok := kimiLeanHintAt(path)
		if !ok {
			t.Fatal("a snapshot that no longer equals the lean set must surface — that is the allowlist's one failure mode")
		}
		if !res.ok || res.warn {
			t.Errorf("drift is a hint, not a misconfiguration: want ok=true warn=false, got %+v", res)
		}
		if res.fix != "" {
			t.Errorf("an informational line must carry no fix, got %q", res.fix)
		}
		for _, want := range []string{lean[len(lean)-1], "plumb setup kimi-code --lean"} {
			if !strings.Contains(res.detail, want) {
				t.Errorf("detail %q should name %q", res.detail, want)
			}
		}
	})
}

// TestRegisteredToolNamesCoverTheLeanSet ties the two name sets the grader
// compares. If a lean name were not in the registered roster, plumb's OWN
// written allowlist would grade as naming no real tool — doctor would warn about
// the file `plumb setup kimi-code --lean` had just produced.
func TestRegisteredToolNamesCoverTheLeanSet(t *testing.T) {
	registered := nameSet(registeredToolNames())
	for _, name := range tools.LeanToolNames() {
		if !registered[name] {
			t.Errorf("lean tool %q is not in the registered tool roster (mcp.SelftestToolNames) — "+
				"doctor would grade plumb's own allowlist as naming no real tool", name)
		}
	}
}

func anyList(names ...string) []any {
	out := make([]any, len(names))
	for i, n := range names {
		out[i] = n
	}
	return out
}

func sameNames(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
