package cli

import (
	"testing"
	"time"
)

// TestDaemonStartTime_HasNoMonotonicReading pins the CAPTURE site of the
// daemon's start timestamp. daemon_info defensively re-strips it in its
// constructor, so that tool stays correct either way — but the web dashboard's
// uptimeSeconds reads this value in-process through web.Deps, and the TUI
// anchors its uptime widgets on it, so a monotonic reading here silently
// under-reports uptime by however long the machine was suspended (the original
// symptom: a 22 h-old daemon reporting 5 h 7 m).
//
// time.Time's == compares the monotonic reading, so equality with its own
// Round(0) proves none is present.
func TestDaemonStartTime_HasNoMonotonicReading(t *testing.T) {
	got := daemonStartTime()
	if got != got.Round(0) {
		t.Error("daemonStartTime carries a monotonic clock reading; uptime would exclude system-suspend time")
	}
	if time.Since(got) < 0 {
		t.Errorf("daemonStartTime is in the future: %v", got)
	}
}
