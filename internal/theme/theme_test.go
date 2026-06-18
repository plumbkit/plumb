package theme

import (
	"regexp"
	"testing"
)

var hexRE = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// TestPalettes_AllColoursHex guards against a typo'd or empty colour token —
// the web UI feeds these straight into CSS, so a malformed value would silently
// break a theme.
func TestPalettes_AllColoursHex(t *testing.T) {
	for name, p := range palettes {
		fields := map[string]string{
			"Bg": p.Bg, "Card": p.Card, "Card2": p.Card2, "Rule": p.Rule,
			"Text": p.Text, "Soft": p.Soft, "Faint": p.Faint,
			"Acc": p.Acc, "Acc2": p.Acc2, "Grn": p.Grn, "Warn": p.Warn, "Forest": p.Forest,
		}
		for field, v := range fields {
			if !hexRE.MatchString(v) {
				t.Errorf("theme %q field %s: %q is not a #RRGGBB hex", name, field, v)
			}
		}
		if len(p.Cats) == 0 {
			t.Errorf("theme %q: Cats is empty", name)
		}
		for i, c := range p.Cats {
			if !hexRE.MatchString(c) {
				t.Errorf("theme %q Cats[%d]: %q is not a #RRGGBB hex", name, i, c)
			}
		}
		if p.ChromaStyle == "" {
			t.Errorf("theme %q: ChromaStyle is empty", name)
		}
	}
}

func TestGet_FallsBackToDefault(t *testing.T) {
	p, ok := Get("does-not-exist")
	if ok {
		t.Fatal("Get returned ok for an unknown theme")
	}
	if want := palettes[Default]; p.Acc != want.Acc {
		t.Fatalf("Get fallback = %q accent, want default %q", p.Acc, want.Acc)
	}

	if _, ok := Get("plumb"); !ok {
		t.Fatal("Get(plumb) reported unknown")
	}
}

func TestNames_MatchesCatalogue(t *testing.T) {
	if got, want := len(Names()), len(palettes); got != want {
		t.Fatalf("Names() len = %d, want %d", got, want)
	}
}
