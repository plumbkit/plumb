package config

import (
	"strings"
	"testing"
)

func TestRegistry_KeysUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, f := range Registry() {
		if seen[f.Key] {
			t.Errorf("duplicate registry key: %q", f.Key)
		}
		seen[f.Key] = true
	}
}

func TestRegistry_EnumFieldsHaveAllowedValues(t *testing.T) {
	// ui.theme is the one enum whose members are supplied dynamically by the tui
	// layer via RegisterEnumValues; it legitimately carries no static values.
	dynamicEnum := map[string]bool{"ui.theme": true}
	for _, f := range Registry() {
		switch f.Type {
		case FieldEnum:
			if len(f.AllowedValues) == 0 && !dynamicEnum[f.Key] {
				t.Errorf("enum field %q has no AllowedValues", f.Key)
			}
		default:
			if len(f.AllowedValues) != 0 {
				t.Errorf("non-enum field %q carries AllowedValues", f.Key)
			}
		}
	}
}

func TestRegistry_PerLanguageTemplatesParse(t *testing.T) {
	for _, f := range Registry() {
		n := strings.Count(f.Key, langPlaceholder)
		if f.PerLanguage && n != 1 {
			t.Errorf("per-language field %q must contain exactly one %q, found %d", f.Key, langPlaceholder, n)
		}
		if !f.PerLanguage && n != 0 {
			t.Errorf("non-per-language field %q must not contain %q", f.Key, langPlaceholder)
		}
	}
}

func TestField_PerLanguageResolution(t *testing.T) {
	cases := []struct {
		key    string
		wantOK bool
	}{
		{"tasks.go.build", true},
		{"tasks.python.test", true},
		{"lsp.go.command", true},
		{"lsp.rust.root_markers", true},
		{"topology.exclude_patterns", true},
		{"ui.theme", true},
		{"git.allow_push", true},
		{"nonsense.key", false},
		{"tasks.go.unknownslot", false},
	}
	for _, c := range cases {
		_, ok := Lookup(c.key)
		if ok != c.wantOK {
			t.Errorf("Lookup(%q) ok=%v, want %v", c.key, ok, c.wantOK)
		}
	}
}

func TestField_PerLanguageResolvesToTemplate(t *testing.T) {
	f, ok := Lookup("tasks.go.build")
	if !ok {
		t.Fatal("tasks.go.build did not resolve")
	}
	if f.Key != "tasks.<lang>.build" {
		t.Errorf("resolved key = %q, want template tasks.<lang>.build", f.Key)
	}
}

func TestReloadTier_String(t *testing.T) {
	for tier, want := range map[ReloadTier]string{
		ReloadLive: "live", ReloadNextSession: "next-session", ReloadRestart: "restart",
	} {
		if got := tier.String(); got != want {
			t.Errorf("ReloadTier(%d).String() = %q, want %q", tier, got, want)
		}
	}
}

func TestEnumValues_DynamicOverridesStatic(t *testing.T) {
	RegisterEnumValues("ui.theme", []string{"plumb", "dracula"})
	f, ok := Lookup("ui.theme")
	if !ok {
		t.Fatal("ui.theme missing")
	}
	got := EnumValues(f)
	if len(got) != 2 || got[0] != "plumb" {
		t.Errorf("EnumValues(ui.theme) = %v, want dynamic [plumb dracula]", got)
	}
}
