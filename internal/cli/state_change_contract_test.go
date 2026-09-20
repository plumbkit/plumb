package cli

import (
	"slices"
	"testing"

	"github.com/plumbkit/plumb/internal/tools"
)

// stateChanging classifies every MCP tool plumb registers by whether it changes
// state that another agent on the same connection could observe or lose.
//
// This map is the deliverable, not a convenience. The shared-connection write
// gate (refuseSharedStateChange) decides which anonymous calls to refuse by
// asking tools.WriteToolNames — a list built for the peer-awareness DISPLAY
// feed and reused as a security gate, which is why its coverage is arbitrary
// rather than derived. Nothing connected the two, so a mutating tool absent
// from a UI list was silently ungated, and hand-extending that list re-arms the
// same trap the moment the next tool lands.
//
// The universe here is derived from the live registration (srv.ToolNames), so a
// newly registered tool fails this test until someone decides what it is.
// PLAN-440 item 1.
var stateChanging = map[string]string{
	// Changes state a peer could observe or lose.
	"write_file": mutates, "edit_file": mutates, "delete_file": mutates,
	"rename_file": mutates, "copy_file": mutates, "transaction_apply": mutates,
	"find_replace": mutates, "undo_edit": mutates,
	"rename_symbol": mutates, "replace_symbol_body": mutates,
	"insert_before_symbol": mutates, "insert_after_symbol": mutates,
	"safe_delete_symbol": mutates, "move_symbol": mutates,
	"git": mutates, "git_init": mutates,
	"run_command": mutates, "run_task": mutates, "mutation_test": mutates,
	"write_memory": mutates, "delete_memory": mutates, "share_findings": mutates,
	"rename_session": mutates, "agent_config": mutates,
	"share_intent": mutates, "leave_note": mutates, "check_messages": mutates,

	// State-changing, but gated where the change happens rather than at the
	// door — see stateChangingToolNames' note on why the door cannot refuse it.
	"session_start": guardedElsewhere,

	// Reads.
	"read_file": readOnly, "read_symbol": readOnly, "read_multiple_files": readOnly,
	"read_memory": readOnly, "list_memories": readOnly, "search_memories": readOnly,
	"relevant_memories": readOnly,
	"file_outline":      readOnly, "file_status": readOnly, "file_diff": readOnly,
	"minimal_diff_review": readOnly,
	"find_files":          readOnly, "search_in_files": readOnly, "find_references": readOnly,
	"get_definition": readOnly, "explain_symbol": readOnly,
	"call_hierarchy": readOnly, "type_hierarchy": readOnly,
	"workspace_symbols": readOnly, "workspace_search": readOnly,
	"diagnostics":     readOnly,
	"topology_search": readOnly, "topology_explore": readOnly, "topology_impact": readOnly,
	"topology_routes": readOnly, "topology_affected": readOnly, "topology_status": readOnly,
	"structural_query": readOnly,
	"daemon_info":      readOnly, "workspace_sessions": readOnly,
}

const (
	mutates          = "mutates"
	readOnly         = "read-only"
	guardedElsewhere = "guarded-elsewhere"
)

// TestStateChangeGateCoversEveryMutatingTool asserts three things about the
// gate, all derived rather than restated:
//
//  1. Every REGISTERED tool is classified. A new tool cannot arrive ungated and
//     unnoticed, which is the failure mode that produced this card.
//  2. Every tool classified as state-changing is gated.
//  3. No tool classified read-only is gated — a stale entry would refuse calls
//     that are safe to serve anonymously, which fails closed in the wrong place.
//
// The companion to the loop below, and the direction it cannot see. That loop
// walks srv.ToolNames(), so an entry in the GATE or in the classification map
// that names no registered tool — a typo, or a tool since renamed or removed —
// is never visited and the suite stays green while the gate quietly protects
// nothing. Both lists are therefore checked back against the registration.
func TestStateChangeGateNamesOnlyRegisteredTools(t *testing.T) {
	_, srv := buildTestConnSession(t)
	registered := map[string]bool{}
	for _, name := range srv.ToolNames() {
		registered[name] = true
	}
	for _, name := range tools.StateChangingToolNames() {
		if !registered[name] {
			t.Errorf("the write gate names %q, which is not a registered tool — "+
				"a rename or a typo has left the gate guarding nothing under that name", name)
		}
	}
	for name := range stateChanging {
		if !registered[name] {
			t.Errorf("stateChanging classifies %q, which is not a registered tool — "+
				"remove the stale entry so the map keeps describing reality", name)
		}
	}
}

func TestStateChangeGateCoversEveryMutatingTool(t *testing.T) {
	_, srv := buildTestConnSession(t)

	names := srv.ToolNames()
	if len(names) == 0 {
		t.Fatal("no tools registered — the universe this contract derives from is empty")
	}

	gate := tools.StateChangingToolNames()
	for _, name := range names {
		class, classified := stateChanging[name]
		if !classified {
			t.Errorf("tool %q is registered but not classified in stateChanging "+
				"(state_change_contract_test.go) — decide whether it changes state another "+
				"agent could observe, then gate it or record that it is read-only", name)
			continue
		}
		gated := slices.Contains(gate, name)
		switch {
		case class == mutates && !gated:
			t.Errorf("tool %q changes state but is NOT in the shared-connection write gate: "+
				"an anonymous call on a shared connection can run it and be attributed to nobody", name)
		case class == readOnly && gated:
			t.Errorf("tool %q is read-only but IS in the write gate: anonymous callers are "+
				"refused a call that is safe to serve", name)
		case class == guardedElsewhere && gated:
			t.Errorf("tool %q is recorded as guarded elsewhere but IS in the door gate; "+
				"refusing it anonymously makes identity undeclarable on a shared connection", name)
		}
	}
}
