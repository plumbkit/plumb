package cli

import (
	"context"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/tools"
)

// readRecordingToolNames is the set of tool names TestToolWiringParity expects
// to find implementing readDepsTool (== tools.readRecordingTool) via the REAL
// registration path (registerAllTools). read_file's search mode
// (internal/tools/read_file_search.go) is the SAME registered *ReadFile
// instance as read_file, not a separate tool, so it needs no separate entry.
var readRecordingToolNames = []string{"read_file", "read_symbol", "read_multiple_files"}

// readDepsTool mirrors tools.readRecordingTool (internal/tools/read_deps.go)
// locally: internal/cli cannot import that unexported interface, but Go's
// structural typing lets a type assertion here succeed against any tools.*
// value whose exported ReadDeps method matches this shape.
type readDepsTool interface {
	ReadDeps() (tracker, readsFor, writes, client bool)
}

// buildTestConnSession constructs a connSession the same way production's
// handleConn does (newConnSession against a real config.Store and
// workspacePool) and runs the REAL registerAllTools registration path against
// a fresh mcp.Server. Tests probe the tool OBJECTS THIS PRODUCES via
// srv.Lookup/ToolNames and s.toolVisible — never a parallel, hand-built
// stand-in that could silently drift from the real registration (memory
// testing-lock-ordering-callback-probe: assert from within the wired object,
// not a parallel hand-built one).
func buildTestConnSession(t *testing.T) (*connSession, *mcp.Server) {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	pool := detectTestPool()
	s := newConnSession(context.Background(), pool, nil, store, nil, nil, newSharedBudgets())
	t.Cleanup(s.close)
	srv := mcp.New(mcp.ServerInfo{Name: "test", Version: "0"})
	s.registerAllTools(srv, time.Now())
	return s, srv
}

// TestToolWiringParity is the registration-parity guard (PLAN-361, worth-it
// W1-7): every read-recording tool (read_file, read_symbol,
// read_multiple_files) must be registered with its ReadTracker/readsFor
// wiring non-nil, or [edits] strict mode's edit_file gate silently rejects
// every edit that follows a read via that tool — the exact defect PLAN-357
// fixed for read_multiple_files, which shipped registered with NO tracker at
// all (conn_register.go's registration line had 2 setters where
// read_file/read_symbol had 8; ReadTracker.Record is nil-safe, so the gap
// failed silently rather than panicking).
//
// It probes the REAL objects registerAllTools built, via srv.Lookup — not a
// hand-copied fixture list, which would re-create the exact drift this test
// exists to prevent (memory green-but-false-and-peer-claims).
func TestToolWiringParity(t *testing.T) {
	_, srv := buildTestConnSession(t)

	found := map[string]bool{}
	for _, name := range srv.ToolNames() {
		tool, ok := srv.Lookup(name)
		if !ok {
			t.Fatalf("srv.Lookup(%q): reported by ToolNames but not found", name)
		}
		rt, ok := tool.(readDepsTool)
		if !ok {
			continue
		}
		found[name] = true
		tracker, readsFor, writes, client := rt.ReadDeps()
		if !tracker && !readsFor {
			t.Errorf("tool %q: neither tracker nor readsFor is wired at registration — "+
				"ReadTracker.Record never runs for a read via this tool, so [edits] strict "+
				"mode's edit_file gate rejects every edit that follows (the PLAN-357 defect)", name)
		}
		if !writes {
			t.Errorf("tool %q: WriteTracker (writes) not wired at registration — the "+
				"concurrent-edit-on-read warning is silently disabled for this tool", name)
		}
		if !client {
			t.Errorf("tool %q: client-name accessor not wired at registration — the "+
				"edit-lane hint is silently disabled for conflict-prone clients reading via this tool", name)
		}
	}

	for _, name := range readRecordingToolNames {
		if !found[name] {
			t.Errorf("expected %q to be registered and implement ReadDeps (readRecordingTool); "+
				"it was not found among the registered tools", name)
		}
	}
	if len(found) != len(readRecordingToolNames) {
		t.Errorf("found %d tools implementing ReadDeps, want exactly %d (%v); got %v — "+
			"a newly-added read-recording tool must implement ReadDeps (internal/tools/read_deps.go), "+
			"or readRecordingToolNames here is stale",
			len(found), len(readRecordingToolNames), readRecordingToolNames, found)
	}
}

// TestPinMembership guards pin/profile drift (PLAN-361, after PLAN-355): every
// tool name tools.IsPinned reports true for must (1) actually exist in the
// REAL registered tool set (registerAllTools) — catching a rename/typo that
// silently orphans a pin entry in tools.PinnedTools — and (2) be visible
// under the profile actually served to an ordinary connection (toolVisible).
// The second leg matters because handleToolsList filters a tool out BEFORE
// consulting mcp.Server.AlwaysLoad (internal/mcp/server_handlers.go): a
// pinned tool hidden by the served profile never gets its
// _meta[AlwaysLoad]=true annotation at all, silently making the pin a no-op.
func TestPinMembership(t *testing.T) {
	s, srv := buildTestConnSession(t)

	registered := map[string]bool{}
	for _, name := range srv.ToolNames() {
		registered[name] = true
	}

	for name := range tools.PinnedTools {
		if !tools.IsPinned(name) {
			t.Fatalf("tools.PinnedTools[%q] is set but IsPinned(%q) returns false — "+
				"profile.go is inconsistent with itself", name, name)
		}
		if !registered[name] {
			t.Errorf("pinned tool %q is not in the registered tool set (registerAllTools) — "+
				"a rename or typo silently orphaned this pin entry", name)
			continue
		}
		if !s.toolVisible(name) {
			t.Errorf("pinned tool %q is hidden by the served profile (toolVisible) — "+
				"handleToolsList filters before consulting AlwaysLoad, so a hidden pinned tool "+
				"never gets its pin annotation and the pin is a silent no-op", name)
		}
	}
}
