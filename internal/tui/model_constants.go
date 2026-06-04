package tui

import (
	"time"

	"github.com/golimpio/plumb/internal/stats"
)

const (
	defaultLeftWidth          = 30
	minLeftWidth              = 26
	minPopupLeftWidth         = 28 // enough for " > ● ✓ 05-12 00:00:00 000ms"
	sectionMenuWidth          = 22
	pollInterval              = 2 * time.Second
	activityInterval          = 10 * time.Second
	topoStatusInterval        = 5 * time.Second
	dashBucketRefreshInterval = 30 * time.Second
	activityBuckets           = 16
	bodyStartRow              = 4
)

var sectionMenuItems = []string{"Dashboard", "Sessions", "Memory", "Logs", "Settings"}

// panelFocus identifies which panel / section consumes navigation keys.
type panelFocus int

const (
	focusSessions    panelFocus = iota // j/k moves the session cursor (default); in Memory it moves the memories list
	focusToolStats                     // j/k moves the Tool Statistics cursor
	focusStats                         // j/k moves the Recent calls cursor
	focusDetails                       // j/k scrolls the Details panel
	focusDiagnostics                   // j/k scrolls the Diagnostics panel
	focusLogs                          // j/k scrolls the log viewer (Logs section)
	focusWorkspaces                    // j/k moves the Workspaces cursor (Memory section only)
)

type callKey struct {
	sessionID  string
	calledAtMs int64
}

func (k callKey) zero() bool { return k.sessionID == "" && k.calledAtMs == 0 }

func selectedCallKey(calls []stats.RecentCall, idx int) callKey {
	if idx < 0 || idx >= len(calls) {
		return callKey{}
	}
	return callKey{sessionID: calls[idx].SessionID, calledAtMs: calls[idx].CalledAt.UnixMilli()}
}

type popupDetailCache struct {
	sessionID  string
	calledAt   int64
	inputJSON  string
	outputText string
	loaded     bool
}
