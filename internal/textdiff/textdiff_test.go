package textdiff

import (
	"math/rand/v2"
	"strings"
	"testing"
)

func TestApplyRoundTrip(t *testing.T) {
	cases := map[string][2]string{
		"identical":          {"a\nb\n", "a\nb\n"},
		"empty to text":      {"", "a\nb\n"},
		"text to empty":      {"a\nb\n", ""},
		"add final newline":  {"a\nb", "a\nb\n"},
		"drop final newline": {"a\nb\n", "a\nb"},
		"no newline both":    {"a", "b"},
		"crlf edit":          {"a\r\nb\r\n", "a\r\nc\r\n"},
		"crlf to lf":         {"a\r\nb\r\n", "a\nb\n"},
		"only newline":       {"\n", ""},
		"blank lines":        {"\n\n\n", "\n\nx\n\n"},
		"two hunks":          {"1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n11\n", "1\nX\n3\n4\n5\n6\n7\n8\n9\nY\n11\n"},
		"marker-like line":   {NoEOLMarker + "\n", "x\n" + NoEOLMarker + "\n"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			d := Unified(c[0], c[1])
			got, err := Apply(c[0], d)
			if err != nil {
				t.Fatalf("Apply: %v\ndiff:\n%s", err, d)
			}
			if got != c[1] {
				t.Fatalf("Apply(Unified) = %q, want %q\ndiff:\n%s", got, c[1], d)
			}
		})
	}
}

func TestApplyRoundTripRandom(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for i := range 3000 {
		a := randomText(r)
		b := mutateText(r, a)
		d := Unified(a, b)
		got, err := Apply(a, d)
		if err != nil || got != b {
			t.Fatalf("case %d: Apply(Unified(%q,%q)) = %q, %v\ndiff:\n%s", i, a, b, got, err, d)
		}
	}
}

func TestApplyRoundTripBeyondMyersBound(t *testing.T) {
	var a, b strings.Builder
	for i := range MaxMyersDistance + 200 {
		a.WriteString("old " + string(rune('a'+i%26)) + "\n")
		b.WriteString("new " + string(rune('a'+i%26)) + "\n")
	}
	got, err := Apply(a.String(), Unified(a.String(), b.String()))
	if err != nil || got != b.String() {
		t.Fatalf("whole-file fallback did not round-trip: err=%v", err)
	}
}

func TestApplyRejectsAMismatchedBase(t *testing.T) {
	d := Unified("a\nb\n", "a\nc\n")
	// Positive control: the right base applies.
	if _, err := Apply("a\nb\n", d); err != nil {
		t.Fatalf("control: Apply on the right base failed: %v", err)
	}
	if _, err := Apply("x\ny\n", d); err == nil {
		t.Fatal("Apply on a different base succeeded; it must refuse")
	}
}

func TestUnifiedRendersANewlineOnlyChange(t *testing.T) {
	d := Unified("a\nb\n", "a\nb")
	if !strings.Contains(d, NoEOLMarker) {
		t.Fatalf("newline-only change rendered %q; want the %q marker", d, NoEOLMarker)
	}
	if Unified("a\nb\n", "a\nb\n") != "" {
		t.Fatal("identical inputs must render no diff")
	}
}

func TestCountsComeFromTheScript(t *testing.T) {
	added, removed := Counts(ComputeExact("a\nb\nc\n", "a\nB\nc\nd\n"))
	if added != 2 || removed != 1 {
		t.Fatalf("Counts = +%d -%d, want +2 -1", added, removed)
	}
}

func randomText(r *rand.Rand) string {
	alphabet := []string{"a", "b", "c", "", "x\r", "long line here"}
	n := r.IntN(20)
	var sb strings.Builder
	for i := range n {
		sb.WriteString(alphabet[r.IntN(len(alphabet))])
		if i < n-1 || r.IntN(2) == 0 {
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

func mutateText(r *rand.Rand, s string) string {
	lines := strings.SplitAfter(s, "\n")
	for range r.IntN(5) {
		switch op := r.IntN(3); {
		case op == 0 && len(lines) > 0:
			i := r.IntN(len(lines))
			lines = append(lines[:i], lines[i+1:]...)
		case op == 1:
			i := r.IntN(len(lines) + 1)
			lines = append(lines[:i], append([]string{"ins\n"}, lines[i:]...)...)
		case len(lines) > 0:
			lines[r.IntN(len(lines))] = "chg"
		}
	}
	return strings.Join(lines, "")
}
