package tui

import (
	"testing"
	"time"

	"github.com/golimpio/plumb/internal/session"
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
