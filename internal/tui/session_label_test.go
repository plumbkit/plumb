package tui

import (
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/session"
)

func TestSessionLangLabelPrefersDetectedLanguage(t *testing.T) {
	got := sessionLangLabel(session.Info{
		Language:         "none",
		DetectedLanguage: "java",
	})
	if got != "java" {
		t.Fatalf("sessionLangLabel = %q, want java", got)
	}
}

func TestSessionLangLabelUnknownLanguage(t *testing.T) {
	got := sessionLangLabel(session.Info{Language: "none"})
	if got != "?" {
		t.Fatalf("sessionLangLabel = %q, want ?", got)
	}
}

func TestSessionLanguagesAndBadges(t *testing.T) {
	tests := []struct {
		name      string
		info      session.Info
		wantLangs []string // lower-case names (Details row)
		wantBadge []string // upper-case badge labels (list view)
	}{
		{"single language", session.Info{Language: "go", DetectedLanguage: "go", Adapters: []string{"gopls"}}, []string{"go"}, []string{"GO"}},
		{"no adapters falls back to primary", session.Info{Language: "go", DetectedLanguage: "go"}, []string{"go"}, []string{"GO"}},
		{"go + html", session.Info{Language: "go", DetectedLanguage: "go", Adapters: []string{"gopls", "vscode-html-language-server"}}, []string{"go", "html"}, []string{"GO", "HTML"}},
		{"unknown secondary adapter skipped", session.Info{Language: "go", DetectedLanguage: "go", Adapters: []string{"gopls", "mystery-ls"}}, []string{"go"}, []string{"GO"}},
		// Monorepo root: DetectedLanguage is already the joined "swift, zig" label,
		// and zls is also a listed adapter — must not double-count zig (the pauta bug).
		{"monorepo joined label not double-counted", session.Info{Language: "none", DetectedLanguage: "swift, zig", Adapters: []string{"sourcekit-lsp", "zls"}}, []string{"swift", "zig"}, []string{"SWIFT", "ZIG"}},
		{"unknown project language", session.Info{Language: "", DetectedLanguage: ""}, nil, []string{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := sessionLanguages(tc.info); !equalStrs(got, tc.wantLangs) {
				t.Errorf("sessionLanguages = %v, want %v", got, tc.wantLangs)
			}
			if got := sessionLangs(tc.info); !equalStrs(got, tc.wantBadge) {
				t.Errorf("sessionLangs = %v, want %v", got, tc.wantBadge)
			}
		})
	}
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestSessionIsIdle_ThresholdRespected asserts that sessionIsIdle uses the
// supplied threshold, not the hard-coded const.
func TestSessionIsIdle_ThresholdRespected(t *testing.T) {
	recent := time.Now().Add(-5 * time.Minute)
	old := time.Now().Add(-40 * time.Minute)

	tests := []struct {
		name      string
		lastSeen  time.Time
		threshold time.Duration
		wantIdle  bool
	}{
		{"recent, default threshold (30 m) → not idle", recent, 30 * time.Minute, false},
		{"old, default threshold (30 m) → idle", old, 30 * time.Minute, true},
		{"recent, short threshold (1 m) → idle", recent, time.Minute, true},
		{"old, long threshold (60 m) → not idle", old, 60 * time.Minute, false},
		{"zero LastSeenAt and StartedAt → never idle", time.Time{}, 30 * time.Minute, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := session.Info{LastSeenAt: tc.lastSeen}
			got := sessionIsIdle(s, tc.threshold)
			if got != tc.wantIdle {
				t.Errorf("sessionIsIdle=%v, want %v", got, tc.wantIdle)
			}
		})
	}
}

// TestIdleThreshold_FallsBackToConst asserts that idleThreshold falls back to
// session.IdleSessionThreshold when the configured value is zero or negative.
func TestIdleThreshold_FallsBackToConst(t *testing.T) {
	if got := idleThreshold(0); got != session.IdleSessionThreshold {
		t.Errorf("idleThreshold(0) = %v, want %v", got, session.IdleSessionThreshold)
	}
	if got := idleThreshold(-5); got != session.IdleSessionThreshold {
		t.Errorf("idleThreshold(-5) = %v, want %v", got, session.IdleSessionThreshold)
	}
	if got := idleThreshold(10); got != 10*time.Minute {
		t.Errorf("idleThreshold(10) = %v, want 10m", got)
	}
}

func TestSessionPurposeTag(t *testing.T) {
	tests := []struct {
		name string
		info session.Info
		want string
	}{
		{"empty purpose renders nothing", session.Info{Name: "wild-otter"}, ""},
		{"purpose renders as suffix", session.Info{Name: "wild-otter", Purpose: "deploy-fix"}, " · deploy-fix"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sessionPurposeTag(tt.info); got != tt.want {
				t.Fatalf("sessionPurposeTag = %q, want %q", got, tt.want)
			}
		})
	}
}
