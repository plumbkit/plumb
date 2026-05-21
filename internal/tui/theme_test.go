package tui

import (
	"image/color"
	"reflect"
	"testing"
)

// TestTheme_AllFieldsSet verifies that every field in every built-in theme is
// populated. This catches the common mistake of adding a new field to Theme
// but forgetting to set it in one of the theme literals.
func TestTheme_AllFieldsSet(t *testing.T) {
	colorType := reflect.TypeFor[color.Color]()
	stringType := reflect.TypeFor[string]()
	for name, th := range AvailableThemes {
		t.Run(name, func(t *testing.T) {
			v := reflect.ValueOf(th)
			typ := v.Type()
			for i := range v.NumField() {
				field := typ.Field(i)
				val := v.Field(i)

				switch field.Type {
				case colorType:
					if val.IsNil() {
						t.Errorf("field %s is nil", field.Name)
					}
				case stringType:
					if val.String() == "" {
						t.Errorf("field %s is empty string", field.Name)
					}
				}
			}
		})
	}
}

// TestThemeNames_Sorted verifies ThemeNames returns a deterministic sorted slice.
func TestThemeNames_Sorted(t *testing.T) {
	names := ThemeNames()
	if len(names) == 0 {
		t.Fatal("ThemeNames returned empty slice")
	}
	for i := 1; i < len(names); i++ {
		if names[i] < names[i-1] {
			t.Errorf("ThemeNames not sorted: %q before %q", names[i-1], names[i])
		}
	}
}

// TestTheme_AvailableThemesContainsNordico ensures the default theme key is present.
func TestTheme_AvailableThemesContainsNordico(t *testing.T) {
	if _, ok := AvailableThemes["nordico"]; !ok {
		t.Error("AvailableThemes does not contain \"nordico\"")
	}
}

// TestTheme_ChromaStylesNonEmpty verifies every theme has a non-empty ChromaStyle.
func TestTheme_ChromaStylesNonEmpty(t *testing.T) {
	for name, th := range AvailableThemes {
		if th.ChromaStyle == "" {
			t.Errorf("theme %q: ChromaStyle is empty", name)
		}
	}
}

// TestIsLightTheme_Classification verifies the heuristic classifies known themes correctly.
func TestIsLightTheme_Classification(t *testing.T) {
	cases := []struct {
		name  string
		light bool
	}{
		{"nordico", false},
		{"darcula", false},
		{"dracula", false},
		{"gruvbox", false},
		{"github-light", true},
		{"solarized-light", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			th, ok := AvailableThemes[tc.name]
			if !ok {
				t.Skipf("theme %q not in AvailableThemes", tc.name)
			}
			if got := isLightTheme(th); got != tc.light {
				t.Errorf("isLightTheme(%q) = %v, want %v", tc.name, got, tc.light)
			}
		})
	}
}

// TestTheme_AllSixThemesRegistered guards against accidental omissions.
func TestTheme_AllSixThemesRegistered(t *testing.T) {
	want := []string{"darcula", "dracula", "github-light", "gruvbox", "nordico", "solarized-light"}
	names := ThemeNames()
	if len(names) != len(want) {
		t.Errorf("len(ThemeNames()) = %d, want %d; got %v", len(names), len(want), names)
		return
	}
	for i, w := range want {
		if names[i] != w {
			t.Errorf("ThemeNames()[%d] = %q, want %q", i, names[i], w)
		}
	}
}
