package textfmt

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPlural(t *testing.T) {
	cases := []struct {
		n         int
		one, many string
		want      string
	}{
		{0, "file", "files", "files"},
		{1, "file", "files", "file"},
		{2, "file", "files", "files"},
		{-1, "file", "files", "files"},
		{1, "entry", "entries", "entry"},
		{3, "entry", "entries", "entries"},
	}
	for _, c := range cases {
		if got := Plural(c.n, c.one, c.many); got != c.want {
			t.Errorf("Plural(%d, %q, %q) = %q, want %q", c.n, c.one, c.many, got, c.want)
		}
	}

	// The int64 instantiation is the whole reason this is generic: the TUI
	// counts rows as int64 and could not share the tools package's int helper.
	if got := Plural(int64(1), "tool call", "tool calls"); got != "tool call" {
		t.Errorf("Plural(int64(1)) = %q, want %q", got, "tool call")
	}
	if got := Plural(int64(2), "tool call", "tool calls"); got != "tool calls" {
		t.Errorf("Plural(int64(2)) = %q, want %q", got, "tool calls")
	}
}

func TestEllipsis(t *testing.T) {
	cases := []struct {
		name string
		s    string
		n    int
		want string
	}{
		{"fits exactly", "abcde", 5, "abcde"},
		{"fits with room", "abc", 10, "abc"},
		{"trims", "abcdef", 5, "abcd…"},
		{"budget one", "abcdef", 1, "…"},
		{"budget zero", "abcdef", 0, ""},
		{"budget negative", "abcdef", -3, ""},
		{"empty input", "", 5, ""},
		// A multi-byte string whose byte length exceeds the budget but whose
		// rune count does not must come back untouched. Byte-slicing here is
		// exactly the bug this replaces.
		{"multibyte fits by runes", "日本語テスト", 6, "日本語テスト"},
		{"multibyte trims", "日本語テスト", 4, "日本語…"},
		{"emoji trims", "🌍🌎🌏🌐", 3, "🌍🌎…"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Ellipsis(c.s, c.n)
			if got != c.want {
				t.Errorf("Ellipsis(%q, %d) = %q, want %q", c.s, c.n, got, c.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("Ellipsis(%q, %d) produced invalid UTF-8: %q", c.s, c.n, got)
			}
			if n := utf8.RuneCountInString(got); c.n > 0 && n > c.n {
				t.Errorf("Ellipsis(%q, %d) = %q is %d runes, over budget", c.s, c.n, got, n)
			}
		})
	}
}

// TestEllipsis_NeverExceedsBudget sweeps every budget against multi-byte input
// so the "result fits a column n cells wide" contract is checked, not assumed.
func TestEllipsis_NeverExceedsBudget(t *testing.T) {
	const s = "aé日🌍bé日🌍cé日🌍"
	for n := range 20 {
		got := Ellipsis(s, n)
		if !utf8.ValidString(got) {
			t.Fatalf("Ellipsis(s, %d) produced invalid UTF-8: %q", n, got)
		}
		if c := utf8.RuneCountInString(got); c > n {
			t.Fatalf("Ellipsis(s, %d) = %q is %d runes, over budget", n, got, c)
		}
	}
}

func TestTruncateBytes(t *testing.T) {
	cases := []struct {
		name string
		s    string
		n    int
		want string
	}{
		{"fits", "abc", 10, "abc"},
		{"exact", "abc", 3, "abc"},
		{"trims ascii", "abcdef", 3, "abc"},
		{"zero budget", "abc", 0, ""},
		{"negative budget", "abc", -1, ""},
		// "é" is two bytes: a budget of 1 must back off to empty rather than
		// return a lone continuation byte.
		{"backs off one byte", "é", 1, ""},
		{"keeps whole rune", "aé", 3, "aé"},
		{"drops partial rune", "aé", 2, "a"},
		{"cjk boundary", "日本語", 4, "日"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := TruncateBytes(c.s, c.n)
			if got != c.want {
				t.Errorf("TruncateBytes(%q, %d) = %q, want %q", c.s, c.n, got, c.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("TruncateBytes(%q, %d) produced invalid UTF-8: %q", c.s, c.n, got)
			}
			if len(got) > max(c.n, 0) {
				t.Errorf("TruncateBytes(%q, %d) = %q is %d bytes, over budget", c.s, c.n, got, len(got))
			}
		})
	}
}

func TestClampBytes(t *testing.T) {
	cases := []struct {
		name   string
		s      string
		budget int
		want   string
	}{
		{"fits", "abc", 10, "abc"},
		{"zero budget", "abc", 0, "abc"},
		{"negative budget", "abc", -1, "abc"},
		{"trims ascii", "abcdefghij", 6, "abc…"},
		// The ellipsis itself is three bytes, so a budget at or under three
		// leaves no room for it and the marker is dropped.
		{"no room for marker", "abcdef", 3, "abc"},
		{"multibyte", "日本語テスト", 9, "日本…"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ClampBytes(c.s, c.budget)
			if got != c.want {
				t.Errorf("ClampBytes(%q, %d) = %q, want %q", c.s, c.budget, got, c.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("ClampBytes(%q, %d) produced invalid UTF-8: %q", c.s, c.budget, got)
			}
			if c.budget > 0 && len(got) > c.budget {
				t.Errorf("ClampBytes(%q, %d) = %q is %d bytes, over budget", c.s, c.budget, got, len(got))
			}
		})
	}
}

// TestClampBytes_NeverExceedsBudget is the property the *_bytes config knobs
// promise: whatever the input, the result fits the stated byte budget.
func TestClampBytes_NeverExceedsBudget(t *testing.T) {
	inputs := []string{
		"plain ascii text that is quite long indeed",
		"日本語のテキストはマルチバイトです",
		"mixed é ascii 日本語 🌍 emoji",
		strings.Repeat("🌍", 30),
	}
	for _, s := range inputs {
		for budget := 1; budget <= len(s)+2; budget++ {
			got := ClampBytes(s, budget)
			if len(got) > budget {
				t.Fatalf("ClampBytes(%q, %d) = %q is %d bytes, over budget", s, budget, got, len(got))
			}
			if !utf8.ValidString(got) {
				t.Fatalf("ClampBytes(%q, %d) produced invalid UTF-8: %q", s, budget, got)
			}
		}
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		b    int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1023, "1023 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1 << 20, "1.0 MiB"},
		{3 * (1 << 20), "3.0 MiB"},
		{1 << 30, "1.0 GiB"},
		{5 * (1 << 30), "5.0 GiB"},
	}
	for _, c := range cases {
		if got := HumanBytes(c.b); got != c.want {
			t.Errorf("HumanBytes(%d) = %q, want %q", c.b, got, c.want)
		}
	}
}

func TestHumanBytesCompact(t *testing.T) {
	// The compact form drops the fractional digit below GiB so several byte
	// columns fit side by side; GiB keeps it because the range is wide.
	cases := []struct {
		b    uint64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1 KiB"},
		{42 * 1024 * 1024, "42 MiB"},
		{1536 * 1024 * 1024, "1.5 GiB"},
	}
	for _, c := range cases {
		if got := HumanBytesCompact(c.b); got != c.want {
			t.Errorf("HumanBytesCompact(%d) = %q, want %q", c.b, got, c.want)
		}
	}
}

// TestHumanBytes_AcceptsEveryByteCountKind pins the generic instantiation: the
// copies this replaced took int64 and uint64 separately, which is what kept
// them apart.
func TestHumanBytes_AcceptsEveryByteCountKind(t *testing.T) {
	if got := HumanBytes(int64(2048)); got != "2.0 KiB" {
		t.Errorf("HumanBytes(int64) = %q", got)
	}
	if got := HumanBytes(uint64(2048)); got != "2.0 KiB" {
		t.Errorf("HumanBytes(uint64) = %q", got)
	}
	if got := HumanBytes(2048); got != "2.0 KiB" {
		t.Errorf("HumanBytes(int) = %q", got)
	}
}
