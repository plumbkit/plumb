package tools

import (
	"strings"
	"testing"
)

// TestSessionStart_EpisodicBlock covers the session_start surfacing seam: when
// the episodic accessor returns a summary, session_start renders a "Last
// session" block; otherwise it renders nothing.
func TestSessionStart_EpisodicBlock(t *testing.T) {
	t.Run("present", func(t *testing.T) {
		var sb strings.Builder
		s := &SessionStart{episodicFn: func(string) (string, bool) {
			return "In your last session you modified `a.go` (1 write).", true
		}}
		s.writeSessionEpisodic(&sb, "/ws")
		out := sb.String()
		if !strings.Contains(out, "## Last session") || !strings.Contains(out, "modified `a.go`") {
			t.Errorf("expected a Last session block, got: %q", out)
		}
	})

	t.Run("absent", func(t *testing.T) {
		var sb strings.Builder
		s := &SessionStart{episodicFn: func(string) (string, bool) { return "", false }}
		s.writeSessionEpisodic(&sb, "/ws")
		if sb.Len() != 0 {
			t.Errorf("no block expected when episodicFn returns false, got: %q", sb.String())
		}
	})

	t.Run("nil accessor", func(t *testing.T) {
		var sb strings.Builder
		(&SessionStart{}).writeSessionEpisodic(&sb, "/ws")
		if sb.Len() != 0 {
			t.Errorf("no block expected when episodicFn is nil, got: %q", sb.String())
		}
	})
}
