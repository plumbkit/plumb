package render_test

import (
	"testing"
	"time"

	"github.com/golimpio/plumb/internal/render"
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

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		b    int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1 << 20, "1.0 MiB"},
		{3 * (1 << 20), "3.0 MiB"},
	}
	for _, tc := range cases {
		if got := render.HumanBytes(tc.b); got != tc.want {
			t.Errorf("HumanBytes(%d) = %q, want %q", tc.b, got, tc.want)
		}
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
