package tui

import (
	"testing"
	"time"

	"github.com/golimpio/plumb/internal/stats"
)

// mkCall builds a RecentCall with the given session id and a CalledAt
// derived from msOffset. Helper kept tiny so test intent stays obvious.
func mkCall(sess string, msOffset int64) stats.RecentCall {
	return stats.RecentCall{
		SessionID: sess,
		CalledAt:  time.UnixMilli(1_000_000_000_000 + msOffset),
	}
}

// Selecting a call and then having newer calls prepend should NOT shift the
// user's selection to a different call — locateCall must follow the original
// row by (session_id, called_at), not by raw index.
func TestLocateCall_PreservesSelectionAcrossPrepend(t *testing.T) {
	before := []stats.RecentCall{
		mkCall("s1", 200),
		mkCall("s1", 150),
		mkCall("s1", 100),
	}
	key := selectedCallKey(before, 1) // user is on the 150ms row

	after := []stats.RecentCall{
		mkCall("s1", 300), // new call prepended
		mkCall("s1", 250), // new call prepended
		mkCall("s1", 200),
		mkCall("s1", 150), // selected row — now at index 3
		mkCall("s1", 100),
	}
	got := locateCall(after, key, 1)
	if got != 3 {
		t.Errorf("locateCall = %d, want 3 (the row at 150ms must still be selected)", got)
	}
}

// When the selected call rolls off the 50-row Recent() limit, locateCall
// falls back to the clamped previous index instead of jumping to 0 —
// otherwise scroll-to-bottom users would snap back up on every refresh.
func TestLocateCall_FallsBackWhenRolledOff(t *testing.T) {
	before := []stats.RecentCall{mkCall("s1", 100), mkCall("s1", 50)}
	key := selectedCallKey(before, 1)
	after := []stats.RecentCall{mkCall("s1", 300)} // 100ms and 50ms gone
	got := locateCall(after, key, 1)
	if got != 0 {
		t.Errorf("locateCall fallback = %d, want 0 (clamped to last index)", got)
	}
}

func TestLocateCall_EmptyList(t *testing.T) {
	got := locateCall(nil, callKey{}, 5)
	if got != 0 {
		t.Errorf("locateCall(nil) = %d, want 0", got)
	}
}

// Two distinct sessions can share the same called_at millisecond — sessionID
// is what disambiguates. locateCall must match on both, not just the time.
func TestLocateCall_DistinguishesSessions(t *testing.T) {
	calls := []stats.RecentCall{
		mkCall("s1", 100),
		mkCall("s2", 100),
	}
	key := callKey{sessionID: "s2", calledAtMs: time.UnixMilli(1_000_000_000_100).UnixMilli()}
	got := locateCall(calls, key, 0)
	if got != 1 {
		t.Errorf("locateCall = %d, want 1 (must match by sessionID, not just time)", got)
	}
}

func TestLocateTool_PreservesSelection(t *testing.T) {
	before := []stats.ToolStat{{Tool: "edit_file"}, {Tool: "read_file"}}
	got := locateTool(before, "read_file", 0)
	if got != 1 {
		t.Errorf("locateTool = %d, want 1", got)
	}
}

func TestLocateTool_RemovedToolClampsToLast(t *testing.T) {
	stats := []stats.ToolStat{{Tool: "edit_file"}}
	got := locateTool(stats, "gone_tool", 3)
	if got != 0 {
		t.Errorf("locateTool with removed tool = %d, want 0 (clamped)", got)
	}
}
