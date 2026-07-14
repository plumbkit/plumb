package tools

import (
	"strings"
	"testing"
	"time"
)

// The "LSP is ready" recommended-start line surfaces a non-default diagnostics
// mode (pull / hybrid / pull-requested-but-unavailable) so an agent knows the
// connection negotiated something other than the push default. A push (or empty)
// mode stays quiet.
func TestSessionStart_ReadyLineSurfacesDiagMode(t *testing.T) {
	cases := []struct {
		name       string
		mode       string
		wantSubstr string
		quiet      bool // the ready line must NOT mention diagnostics
	}{
		{"pull is surfaced", "pull", "diagnostics: pull", false},
		{"hybrid is surfaced", "hybrid", "diagnostics: hybrid", false},
		{"pull-unavailable is surfaced", "pull-requested-but-unavailable", "diagnostics: pull-requested-but-unavailable", false},
		{"push stays quiet", "push", "", true},
		{"empty stays quiet", "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ss := (&SessionStart{}).
				WithLSPLanguage(func() string { return "go" }).
				WithLSPWarmup(func() (bool, time.Duration) { return false, 0 }).
				WithLSPDiagMode(func() string { return c.mode })
			var sb strings.Builder
			ss.writeSessionRecommendedStart(&sb, false, "Go", "go")
			out := sb.String()
			if !strings.Contains(out, "LSP is ready") {
				t.Fatalf("expected the ready line: %q", out)
			}
			if c.quiet {
				if strings.Contains(out, "diagnostics:") {
					t.Errorf("push/empty mode must stay quiet, got: %q", out)
				}
				return
			}
			if !strings.Contains(out, c.wantSubstr) {
				t.Errorf("missing %q in ready line: %q", c.wantSubstr, out)
			}
		})
	}
}
