package config

import (
	"reflect"
	"strings"
	"testing"
)

// project_classification_test.go — the audit, enforced.
//
// The completeness test is the important one. A per-section classification
// written only in prose is a snapshot that rots; walking the Config struct by
// reflection means a field added next year cannot be merged without someone
// answering "is a hostile value safe here?".

// mapKeyPlaceholder is how a map-keyed section appears in the classification
// table: [lsp.<lang>] and [tasks.<lang>] are per-language tables, so their keys
// are recorded once with the placeholder rather than per language.
const mapKeyPlaceholder = "<lang>"

// configLeafKeys walks Config and returns every leaf field's dotted TOML path.
// A struct field recurses; a map of structs recurses once under the placeholder;
// everything else is a leaf.
func configLeafKeys(t *testing.T) []string {
	t.Helper()
	var out []string
	var walk func(rt reflect.Type, prefix string)
	walk = func(rt reflect.Type, prefix string) {
		for i := range rt.NumField() {
			f := rt.Field(i)
			tag, _, _ := strings.Cut(f.Tag.Get("toml"), ",")
			if tag == "" || tag == "-" {
				continue
			}
			key := tag
			if prefix != "" {
				key = prefix + "." + tag
			}
			ft := f.Type
			switch {
			case ft.Kind() == reflect.Struct && ft.Name() != "Duration":
				walk(ft, key)
			case ft.Kind() == reflect.Map && ft.Elem().Kind() == reflect.Struct:
				walk(ft.Elem(), key+"."+mapKeyPlaceholder)
			case ft.Kind() == reflect.Slice && ft.Elem().Kind() == reflect.Struct:
				// [[command]] is classified as a whole: an entry's individual
				// fields are meaningless apart from the argv they compose.
				out = append(out, key)
			default:
				out = append(out, key)
			}
		}
	}
	walk(reflect.TypeOf(Config{}), "")
	return out
}

// TestProjectFieldClasses_CoverEveryConfigField is the guard that makes the
// classification an audit rather than a document. Adding a field to Config
// without recording whether a hostile project value is safe fails here.
func TestProjectFieldClasses_CoverEveryConfigField(t *testing.T) {
	for _, key := range configLeafKeys(t) {
		if _, ok := projectFieldClasses[key]; !ok {
			t.Errorf("config field %q has no entry in projectFieldClasses.\n"+
				"Every field a project's .plumb/config.toml can set must be classified: "+
				"can a cloned repository's value for this field widen access, run a process, "+
				"redirect a write, suppress evidence, or reverse a safety choice the user made globally?\n"+
				"Add it to project_classification.go with the reason — ClassPreference is a fine "+
				"answer, but it has to be an answer.", key)
		}
	}
}

// TestProjectFieldClasses_NoStaleEntries catches the other direction: a key left
// behind after the field it described was renamed or removed, which would leave
// the table asserting a protection that no longer exists.
func TestProjectFieldClasses_NoStaleEntries(t *testing.T) {
	live := make(map[string]bool)
	for _, key := range configLeafKeys(t) {
		live[key] = true
	}
	for key := range projectFieldClasses {
		if !live[key] {
			t.Errorf("projectFieldClasses has an entry for %q, which is not a field of Config — "+
				"remove it, or fix the key if the field was renamed", key)
		}
	}
}

// TestOneWaySafeValue_CoversEveryOneWayBool keeps the two tables in step: a
// ClassOneWay boolean with no recorded safe value would silently fall through
// applyOneWayBools and be honoured verbatim, which is the bug the class exists
// to prevent.
func TestOneWaySafeValue_CoversEveryOneWayBool(t *testing.T) {
	for key, class := range projectFieldClasses {
		if class != ClassOneWay {
			continue
		}
		switch key {
		case "edits.rate_limit_per_minute": // an int budget, resolved by oneWayRateLimit
			continue
		case "tools.profile": // a string, resolved by oneWayToolsProfile
			continue
		}
		if _, ok := oneWaySafeValue[key]; !ok {
			t.Errorf("%q is ClassOneWay but has no oneWaySafeValue entry, so applyOneWayBools "+
				"cannot resolve it", key)
		}
	}
}

// TestOneWayBool_ProjectMayHardenNeverSoften states the rule directly, in both
// directions, for both polarities of safe value.
func TestOneWayBool_ProjectMayHardenNeverSoften(t *testing.T) {
	cases := []struct {
		name                  string
		global, project, safe bool
		want                  bool
	}{
		{"project cannot switch strict off", true, false, true, true},
		{"project may switch strict on", false, true, true, true},
		{"both on stays on", true, true, true, true},
		{"both off stays off", false, false, true, false},
		{"project cannot switch summaries back on", false, true, false, false},
		{"project may switch summaries off", true, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := oneWayBool(tc.global, tc.project, tc.safe); got != tc.want {
				t.Errorf("oneWayBool(global=%v, project=%v, safe=%v) = %v, want %v",
					tc.global, tc.project, tc.safe, got, tc.want)
			}
		})
	}
}

// TestOneWayToolsProfile_ProjectMayOnlyWiden pins the direction: a project may
// ask for more tools to be advertised, never fewer. "auto" is not safe — it
// resolves per client and can resolve to lean, which is the narrowing this
// exists to stop.
func TestOneWayToolsProfile_ProjectMayOnlyWiden(t *testing.T) {
	cases := []struct {
		name            string
		global, project string
		want            string
	}{
		{"project cannot narrow to lean", "full", "lean", "full"},
		{"project cannot narrow an auto global", "auto", "lean", "auto"},
		{"project may widen to full", "auto", "full", "full"},
		{"project cannot launder lean through auto", "full", "auto", "full"},
		{"unset project value keeps global", "lean", "lean", "lean"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := oneWayToolsProfile(tc.global, tc.project); got != tc.want {
				t.Errorf("oneWayToolsProfile(%q, %q) = %q, want %q", tc.global, tc.project, got, tc.want)
			}
		})
	}
}

// TestOneWayRateLimit_ZeroMeansUnlimited pins the case a plain min() gets
// backwards: 0 disables limiting entirely, so it is the WEAKEST value, not the
// strongest. A project asking for 0 must not thereby remove the user's cap.
func TestOneWayRateLimit_ZeroMeansUnlimited(t *testing.T) {
	cases := []struct {
		name            string
		global, project int
		want            int
	}{
		{"project cannot lift the cap with 0", 60, 0, 60},
		{"project cannot raise the cap", 60, 600, 60},
		{"project may lower the cap", 60, 10, 10},
		{"unlimited global takes the project cap", 0, 30, 30},
		{"both unlimited stays unlimited", 0, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := oneWayRateLimit(tc.global, tc.project); got != tc.want {
				t.Errorf("oneWayRateLimit(%d, %d) = %d, want %d", tc.global, tc.project, got, tc.want)
			}
		})
	}
}
