package cli

import (
	"os"
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
		wantShape   allowlistShape
		wantFound   string
		wantUnknown []string
		wantMissing []string
	}{
		{name: "null", raw: nil, want: allowlistDegenerate, wantShape: shapeNull, wantFound: "null"},
		{name: "empty list", raw: []any{}, want: allowlistDegenerate, wantShape: shapeEmpty, wantFound: "an empty list"},
		{name: "a string", raw: "a_tool", want: allowlistDegenerate, wantShape: shapeWrongType, wantFound: "a string"},
		{name: "a number", raw: 1.0, want: allowlistDegenerate, wantShape: shapeWrongType, wantFound: "a number"},
		// go-toml decodes `enabled_tools = 3` to an int64, not a float64: the same
		// hand-edit that JSON reports as "a number" used to fall through to
		// "a value plumb does not recognise" for Codex alone.
		{name: "a TOML integer", raw: int64(3), want: allowlistDegenerate, wantShape: shapeWrongType, wantFound: "a number"},
		{name: "a boolean", raw: true, want: allowlistDegenerate, wantShape: shapeWrongType, wantFound: "a boolean"},
		{name: "an object", raw: map[string]any{"a_tool": true}, want: allowlistDegenerate, wantShape: shapeWrongType, wantFound: "an object"},
		{name: "only empty strings", raw: []any{"", "  "}, want: allowlistDegenerate, wantShape: shapeEmpty, wantFound: "a list holding no tool name"},
		{name: "only nulls", raw: []any{nil}, want: allowlistDegenerate, wantShape: shapeEmpty, wantFound: "a list holding no tool name"},
		{name: "only numbers", raw: []any{1.0, 2.0, 3.0}, want: allowlistDegenerate, wantShape: shapeEmpty, wantFound: "a list holding no tool name"},

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
				t.Errorf("shape = %d (found %q), want %d", got.shape, got.found, tc.wantShape)
			}
			if got.found != tc.wantFound {
				t.Errorf("found = %q, want %q", got.found, tc.wantFound)
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

			res, ok := leanHintAt(kimiLeanClient, path)
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

		res, ok := leanHintAt(kimiLeanClient, path)
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

// TestKimiDegenerateAllowlist_MessageIsPerShape pins what doctor may CLAIM about
// each unusable value. One sentence covered all three before, and it asserted a
// client behaviour only the empty list plausibly has: a `null` option is most
// likely read as "unset" (the full surface), and a wrong-typed value is anyone's
// guess — so telling that user "Kimi loads NO plumb tools at all" was doctor
// stating as fact something it cannot observe. Collapsing the switch back to one
// message turns this red.
func TestKimiDegenerateAllowlist_MessageIsPerShape(t *testing.T) {
	// The strong claim, reserved for the one shape that earns it.
	const inert = "NO plumb tools"

	for _, tc := range []struct {
		name      string
		raw       any
		wantIn    []string
		wantNotIn []string
		wantFixIn string
	}{
		{
			name: "empty list keeps the strong claim", raw: []any{},
			wantIn:    []string{"an empty list", inert},
			wantFixIn: "plumb setup kimi-code --lean",
		},
		{
			name: "a list holding no tool name keeps the strong claim", raw: []any{"", 1.0, nil},
			wantIn:    []string{"a list holding no tool name", inert},
			wantFixIn: "plumb setup kimi-code --lean",
		},
		{
			name: "null reads as no allowlist, not as no tools", raw: nil,
			wantIn:    []string{"null", "full tool surface", "cannot verify"},
			wantNotIn: []string{inert},
			// A key that most likely means "everything" has a different remedy
			// from one that means "nothing": say so unambiguously, or pin lean.
			wantFixIn: "delete the enabledTools key",
		},
		{
			name: "wrong type admits plumb cannot predict the parse", raw: 3.0,
			wantIn:    []string{"a number", "not a list", "cannot verify"},
			wantNotIn: []string{inert},
			wantFixIn: "plumb setup kimi-code --lean",
		},
		{
			name: "an object is named as an object", raw: map[string]any{"read_file": true},
			wantIn:    []string{"an object", "not a list"},
			wantNotIn: []string{inert},
			wantFixIn: "plumb setup kimi-code --lean",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := gradeToolAllowlist(tc.raw, gradeRegistered, gradePinned)
			if g.verdict != allowlistDegenerate {
				t.Fatalf("verdict = %v, want allowlistDegenerate", g.verdict)
			}
			res := degenerateAllowlistResult(kimiLeanClient, g)

			// Every shape stays a non-fatal warning carrying a fix: the value is
			// never one plumb writes, whatever the client makes of it.
			if !res.ok || !res.warn {
				t.Errorf("want a non-fatal warning (ok=true warn=true), got %+v", res)
			}
			for _, want := range tc.wantIn {
				if !strings.Contains(res.detail, want) {
					t.Errorf("detail %q should mention %q", res.detail, want)
				}
			}
			for _, unwanted := range tc.wantNotIn {
				if strings.Contains(res.detail, unwanted) {
					t.Errorf("detail %q must not claim %q — plumb cannot observe that for this shape", res.detail, unwanted)
				}
			}
			if !strings.Contains(res.fix, tc.wantFixIn) {
				t.Errorf("fix %q should mention %q", res.fix, tc.wantFixIn)
			}
			assertNoGoTypeNames(t, res.detail)
		})
	}

	// The claim is not merely absent from two shapes — it is present on the one
	// that earns it, so the assertions above cannot be satisfied by deleting it
	// everywhere.
	var claimed int
	for _, raw := range []any{[]any{}, []any{""}, nil, 3.0, "x", true, map[string]any{}} {
		if strings.Contains(degenerateAllowlistResult(kimiLeanClient, gradeToolAllowlist(raw, gradeRegistered, gradePinned)).detail, inert) {
			claimed++
		}
	}
	if claimed != 2 {
		t.Errorf("%d of the degenerate shapes claim %q; want exactly the two empty-list shapes", claimed, inert)
	}
}

// assertNoGoTypeNames keeps Go's type vocabulary out of a message about the
// user's JSON file. The previous wording formatted the decoded value with %T, so
// a hand-edited number surfaced as "not a list (float64)" and an object as
// "map[string]interface {}" — names from a language the reader is not writing in.
func assertNoGoTypeNames(t *testing.T, detail string) {
	t.Helper()
	for _, leak := range []string{"float64", "interface {}", "map[string]", "[]interface", "%!"} {
		if strings.Contains(detail, leak) {
			t.Errorf("detail %q leaks Go vocabulary %q — the message is about a JSON file", detail, leak)
		}
	}
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

// leanAllowlistFixture writes a config for c whose plumb entry carries the given
// allowlist value verbatim — including shapes plumb would never produce, which
// is exactly what a doctor check has to survive. Pass nil for `value` absent.
func leanAllowlistFixture(t *testing.T, c leanClient, value any, present bool) string {
	t.Helper()
	entry := map[string]any{"command": "/usr/local/bin/plumb", "args": []string{"serve"}}
	if present {
		entry[c.key] = value
	}
	path := filepath.Join(t.TempDir(), filepath.Base(mustPath(t, c.pathFn)))
	if err := c.write(path, map[string]any{c.serversKey: map[string]any{"plumb": entry}}); err != nil {
		t.Fatalf("writing %s fixture: %v", c.name, err)
	}
	return path
}

// TestLeanHintAt_EveryClientEveryState is the parameterised check's contract: the
// same four states, graded the same way, for every client that can hold an
// allowlist. Only the key name and the serialisation differ, which is the whole
// point of the leanClient descriptor — a client added to leanAllowlistClients()
// with no doctor work of its own must land here already graded.
//
// The severities are the ones Kimi shipped with and are deliberate: an absent
// allowlist and an aged snapshot are INFORMATIONAL (a full surface is a valid
// default, and a snapshot still works — a "!" there would mark a healthy machine
// unhealthy), while a list that filters plumb to nothing is a WARNING with a fix,
// because it is this feature's one failure mode a user cannot see from outside.
func TestLeanHintAt_EveryClientEveryState(t *testing.T) {
	lean := tools.LeanToolNames()

	for _, c := range leanAllowlistClients() {
		t.Run(c.name, func(t *testing.T) {
			t.Run("absent: informational, names the command", func(t *testing.T) {
				res, ok := leanHintAt(c, leanAllowlistFixture(t, c, nil, false))
				if !ok {
					t.Fatal("a registration with no allowlist must still say the flag exists")
				}
				if res.subOf != c.name {
					t.Errorf("subOf = %q, want the client row %q — an unlinked sub renders as a stray top-level row", res.subOf, c.name)
				}
				if !res.ok || res.warn {
					t.Errorf("no allowlist is a valid default, not a fault: %+v", res)
				}
				if res.fix != "" {
					t.Errorf("an informational pass must carry no fix line, got %q", res.fix)
				}
				for _, want := range []string{"plumb setup " + c.setupCmd + " --lean", c.key, c.name} {
					if !strings.Contains(res.detail, want) {
						t.Errorf("detail %q should mention %q", res.detail, want)
					}
				}
			})

			t.Run("current: passes in silence", func(t *testing.T) {
				if res, ok := leanHintAt(c, leanAllowlistFixture(t, c, lean, true)); ok {
					t.Errorf("an allowlist equal to the lean set is doing its job — doctor has nothing to say: %+v", res)
				}
			})

			t.Run("stale: informational drift naming the missing name", func(t *testing.T) {
				res, ok := leanHintAt(c, leanAllowlistFixture(t, c, lean[:len(lean)-1], true))
				if !ok {
					t.Fatal("a snapshot that no longer equals the lean set must surface — that is the allowlist's one failure mode")
				}
				if res.subOf != c.name {
					t.Errorf("subOf = %q, want the client row %q", res.subOf, c.name)
				}
				if !res.ok || res.warn {
					t.Errorf("drift is a hint, not a misconfiguration: %+v", res)
				}
				for _, want := range []string{lean[len(lean)-1], "plumb setup " + c.setupCmd + " --lean", c.key} {
					if !strings.Contains(res.detail, want) {
						t.Errorf("detail %q should name %q", res.detail, want)
					}
				}
			})

			t.Run("invalid: warns, naming the offender", func(t *testing.T) {
				res, ok := leanHintAt(c, leanAllowlistFixture(t, c, []string{"not_a_plumb_tool", "nope"}, true))
				if !ok {
					t.Fatal("an allowlist matching no plumb tool leaves the server inert — doctor must not stay silent")
				}
				if res.subOf != c.name {
					t.Errorf("subOf = %q, want the client row %q", res.subOf, c.name)
				}
				if !res.ok || !res.warn {
					t.Errorf("want a non-fatal warning (ok=true warn=true), got %+v", res)
				}
				if res.fix == "" {
					t.Error("a warning must carry a fix line — it is the only part that renders on attention")
				}
				for _, want := range []string{"not_a_plumb_tool", "NO plumb tools", c.name} {
					if !strings.Contains(res.detail, want) {
						t.Errorf("detail %q should mention %q", res.detail, want)
					}
				}
			})

			t.Run("invalid name among valid ones is still named", func(t *testing.T) {
				mixed := append(append([]string{}, lean...), "not_a_plumb_tool")
				res, ok := leanHintAt(c, leanAllowlistFixture(t, c, mixed, true))
				if !ok {
					t.Fatal("a list naming a tool plumb does not register must surface, however many real ones surround it")
				}
				if !strings.Contains(res.detail, "not_a_plumb_tool") {
					t.Errorf("detail %q must name the entry that filters to nothing", res.detail)
				}
			})

			t.Run("degenerate: a scalar is named in the user's vocabulary", func(t *testing.T) {
				// int64 is what go-toml hands back for `enabled_tools = 3`; JSON
				// yields float64 for the same edit. Both must read as "a number".
				for _, scalar := range []any{3, int64(3), 3.5, "read_file", true} {
					res, ok := leanHintAt(c, leanAllowlistFixture(t, c, scalar, true))
					if !ok {
						t.Fatalf("%v: a non-list allowlist must surface", scalar)
					}
					if strings.Contains(res.detail, "does not recognise") {
						t.Errorf("%v: detail %q falls through to the unrecognised default", scalar, res.detail)
					}
					assertNoGoTypeNames(t, res.detail)
				}
			})

			t.Run("degenerate: empty list warns", func(t *testing.T) {
				res, ok := leanHintAt(c, leanAllowlistFixture(t, c, []string{}, true))
				if !ok {
					t.Fatal("an empty allowlist disables every plumb tool — doctor must not stay silent")
				}
				if res.subOf != c.name {
					t.Errorf("subOf = %q, want the client row %q", res.subOf, c.name)
				}
				if !res.ok || !res.warn || res.fix == "" {
					t.Errorf("want a non-fatal warning carrying a fix, got %+v", res)
				}
				if !strings.Contains(res.detail, "an empty list") {
					t.Errorf("detail %q should name the shape", res.detail)
				}
			})

			t.Run("a config without a plumb entry stays silent", func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "cfg")
				if err := c.write(path, map[string]any{c.serversKey: map[string]any{
					"other": map[string]any{"command": "/bin/other"},
				}}); err != nil {
					t.Fatalf("seeding config: %v", err)
				}
				if _, ok := leanHintAt(c, path); ok {
					t.Error("the check must not fire when plumb is not registered")
				}
			})

			t.Run("an absent config stays silent and is not created", func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, "nested", "cfg")
				if _, ok := leanHintAt(c, path); ok {
					t.Error("the check must not fire for an absent config")
				}
				if _, err := os.Stat(filepath.Join(dir, "nested")); !os.IsNotExist(err) {
					t.Error("a doctor check must not create directories while inspecting")
				}
			})
		})
	}
}

// TestCheckMCPClientsGradesEveryLeanClient pins the doctor CALL SITE for all
// three clients. Every subtest above drives leanHintAt directly, so they stay
// green if checkLeanAllowlists is dropped from checkMCPClients — and the one
// failure mode a user cannot see from the outside vanishes from `plumb doctor`
// with nothing failing. The fixture uses the degenerate case deliberately.
func TestCheckMCPClientsGradesEveryLeanClient(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KIMI_CODE_HOME", filepath.Join(home, ".kimi-code"))
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))

	for _, c := range leanAllowlistClients() {
		path := mustPath(t, c.pathFn)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("creating %s config dir: %v", c.name, err)
		}
		if err := c.write(path, map[string]any{c.serversKey: map[string]any{
			"plumb": map[string]any{"command": "/usr/local/bin/plumb", c.key: []string{}},
		}}); err != nil {
			t.Fatalf("seeding %s config: %v", c.name, err)
		}
	}

	seen := map[string]checkResult{}
	for _, r := range checkMCPClients() {
		seen[r.name] = r
	}
	for _, c := range leanAllowlistClients() {
		r, ok := seen[c.checkName()]
		if !ok {
			t.Errorf("checkMCPClients must surface %q — it is the only path that reports a degenerate %s allowlist",
				c.checkName(), c.key)
			continue
		}
		if !r.warn || r.fix == "" {
			t.Errorf("%s: an empty allowlist must raise attention with a fix: %+v", c.name, r)
		}
	}
}
