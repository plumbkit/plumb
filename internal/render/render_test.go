package render_test

import (
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/plumbkit/plumb/internal/render"
)

func TestContractPath(t *testing.T) {
	t.Run("contracts home prefix", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		// Force UserHomeDir to pick up the new HOME.
		path := home + "/projects/foo"
		// ContractPath calls os.UserHomeDir which reads HOME.
		got := render.ContractPath(path)
		if got != "~/projects/foo" {
			t.Errorf("got %q, want ~/projects/foo", got)
		}
	})

	t.Run("leaves non-home path unchanged", func(t *testing.T) {
		got := render.ContractPath("/usr/local/bin/plumb")
		if got != "/usr/local/bin/plumb" {
			t.Errorf("got %q, want /usr/local/bin/plumb", got)
		}
	})

	t.Run("leaves empty string unchanged", func(t *testing.T) {
		got := render.ContractPath("")
		if got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
}

func TestContractHome(t *testing.T) {
	t.Run("contracts every embedded occurrence", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		got := render.ContractHome("reading " + home + "/a.yaml: parsing " + home + "/a.yaml: bad")
		if got != "reading ~/a.yaml: parsing ~/a.yaml: bad" {
			t.Errorf("got %q, want both occurrences contracted", got)
		}
	})

	t.Run("leaves text without the home dir unchanged", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		const msg = "reading /etc/plumb.yaml: permission denied"
		if got := render.ContractHome(msg); got != msg {
			t.Errorf("got %q, want %q", got, msg)
		}
	})
}

func TestShortenPath(t *testing.T) {
	const deep = "~/Library/Application Support/kimi-desktop/daimon-share/daimon/runtime/kimi-code/home/mcp.json"

	cases := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"a fitting path is untouched", "~/.codex/config.toml", 60, "~/.codex/config.toml"},
		{"exactly at the cap is untouched", "~/abc", 5, "~/abc"},
		{"max 0 disables shortening", deep, 0, deep},
		{
			"elides interior segments, keeping the root and as much tail as fits",
			deep, 60,
			"~/…/daimon-share/daimon/runtime/kimi-code/home/mcp.json",
		},
		{
			"an absolute path keeps its leading separator",
			"/usr/local/share/some/deeply/nested/place/config.json", 30,
			"/…/nested/place/config.json",
		},
		{
			"no usable separator falls back to cutting from the left",
			strings.Repeat("x", 40), 10,
			"…" + strings.Repeat("x", 9),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := render.ShortenPath(tc.in, tc.max)
			if got != tc.want {
				t.Errorf("ShortenPath(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
			}
			if tc.max > 0 && lipgloss.Width(got) > tc.max {
				t.Errorf("result is %d columns, over the %d cap: %q", lipgloss.Width(got), tc.max, got)
			}
		})
	}
}

// Carried over from internal/tui when the strategy moved here, so the edge
// cases the TUI had pinned keep their guard.
func TestTruncatePathLeft(t *testing.T) {
	cases := []struct {
		p    string
		n    int
		want string
	}{
		{"abcde", 10, "abcde"},     // fits
		{"abcde", 5, "abcde"},      // fits exactly
		{"abcdefghij", 5, "…ghij"}, // maxW=5: "…" + last 4
		{"ab", 1, "…"},             // maxW≤1 fallback
		{"a", 1, "a"},              // fits exactly at 1
		{"abc", 2, "…c"},           // maxW=2: "…" + last 1
	}
	for _, tc := range cases {
		if got := render.TruncatePathLeft(tc.p, tc.n); got != tc.want {
			t.Errorf("TruncatePathLeft(%q, %d) = %q, want %q", tc.p, tc.n, got, tc.want)
		}
	}
}

// The cap exists to stop one path setting a whole column's width, so what has
// to hold for every input is the width bound — not any particular elision.
func TestShortenPath_NeverExceedsTheCap(t *testing.T) {
	paths := []string{
		"~/Library/Application Support/kimi-desktop/daimon-share/daimon/runtime/kimi-code/home/mcp.json",
		"/a/b/c/d/e/f/g/h/i/j/k/l/m/n/o/p/q/r/s/t/u/v/w/x/y/z/config.yaml",
		"~/" + strings.Repeat("averylongsegmentname/", 8) + "mcp.json",
		strings.Repeat("x", 200),
		"~",
		"",
	}
	for _, max := range []int{10, 20, 40, 60} {
		for _, p := range paths {
			if got := render.ShortenPath(p, max); lipgloss.Width(got) > max {
				t.Errorf("ShortenPath(%q, %d) = %q (%d columns) — over the cap", p, max, got, lipgloss.Width(got))
			}
		}
	}
}

func TestHumanAge(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		t    time.Time
		want string
	}{
		{"seconds ago", now.Add(-30 * time.Second), "30s ago"},
		{"minutes ago", now.Add(-5 * time.Minute), "5m ago"},
		{"hours ago", now.Add(-3 * time.Hour), "3h ago"},
		{"old date", time.Date(2024, 3, 7, 0, 0, 0, 0, time.UTC), "Mar 7"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := render.HumanAge(tc.t)
			if got != tc.want {
				t.Errorf("HumanAge(%v) = %q, want %q", tc.t, got, tc.want)
			}
		})
	}
}

func TestPadRight(t *testing.T) {
	cases := []struct {
		s     string
		width int
		want  string
	}{
		{"hi", 5, "hi   "},
		{"hello", 5, "hello"},
		{"toolong", 3, "toolong"},
	}
	for _, tc := range cases {
		got := render.PadRight(tc.s, tc.width)
		if got != tc.want {
			t.Errorf("PadRight(%q, %d) = %q, want %q", tc.s, tc.width, got, tc.want)
		}
	}
}

func TestPadLeft(t *testing.T) {
	cases := []struct {
		s     string
		width int
		want  string
	}{
		{"hi", 5, "   hi"},
		{"hello", 5, "hello"},
		{"toolong", 3, "toolong"},
	}
	for _, tc := range cases {
		got := render.PadLeft(tc.s, tc.width)
		if got != tc.want {
			t.Errorf("PadLeft(%q, %d) = %q, want %q", tc.s, tc.width, got, tc.want)
		}
	}
}
