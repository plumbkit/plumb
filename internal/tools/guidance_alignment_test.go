package tools_test

// guidance_alignment_test.go proves session_start's live guidance for a client
// PLAN-366 gave a per-client MCP initialize `instructions` body no longer
// restates, at doctrine length, content that body already carries — it keeps
// only live-state material (policy, diagnostics, profile) and short
// decision-point pointers. See internal/tools/session_start_guidance.go's
// trimmed diagnostics bullet and internal/tools/edit_lane.go's doc comment on
// nativeEditLaneWarning for why that warning specifically stays: it is the
// load-bearing recognition text for a harness error the instructions field
// deliberately does not quote (TestSessionStart_EditLaneWarning_ClaudeCode in
// topology_affected_e2e_test.go pins it).

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/tools"
	"github.com/plumbkit/plumb/internal/topology"
	goext "github.com/plumbkit/plumb/internal/topology/extractors/golang"
)

// claudeCodeSessionStartOutput runs a real session_start for a Claude Code
// client with a LIVE topology store wired — the trimmed diagnostics bullet
// (session_start_guidance.go:149) lives inside writeClaudeCodeGuidance's
// topologyActive() branch, so a test that never wires a store exercises the
// OTHER branch instead and would pass whether or not the trim landed. This
// mirrors TestSessionStart_EditLaneWarning_ClaudeCode's topology-on case in
// topology_affected_e2e_test.go.
func claudeCodeSessionStartOutput(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "go.mod"), []byte("module demo\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := topology.Open(ws, config.TopologyConfig{MaxFileSizeBytes: 512 * 1024},
		[]topology.Extractor{goext.New()})
	if err != nil {
		t.Fatalf("topology.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	tool := tools.NewSessionStart(
		func(context.Context) string { return ws }, nil, nil,
		func() bool { return false },
		func() string { return "claude-code" },
		nil,
	).WithTopology(func() *topology.Store { return s })

	out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return out
}

// TestGuidanceAlignment_ClaudeCodeCompileTruthNotDuplicated proves the
// "Compile truth on write" doctrine — which claude-code's MCP initialize
// instructions (mcp.InstructionsForClient) now states in full — is not
// restated at the same length inside session_start's own Claude Code
// guidance, specifically the topology-active branch that carries the trimmed
// bullet. It first confirms the instructions field actually carries the
// doctrine, so a future change to InstructionsForClient that dropped it would
// fail loudly here rather than leaving this test passing for the wrong
// reason.
func TestGuidanceAlignment_ClaudeCodeCompileTruthNotDuplicated(t *testing.T) {
	instr := mcp.InstructionsForClient("claude-code")
	const doctrine = "fail_on_new_errors"
	if !strings.Contains(instr, doctrine) {
		t.Fatalf("claude-code MCP instructions no longer carry the compile-truth doctrine (%q) — "+
			"session_start guidance should not assume it does:\n%s", doctrine, instr)
	}

	out := claudeCodeSessionStartOutput(t)

	const restated = "await_diagnostics on edit_file/write_file returns the authoritative post-write pass"
	if strings.Contains(out, restated) {
		t.Errorf("session_start's Claude Code guidance restates the compile-truth doctrine the MCP "+
			"instructions field already carries in full:\n%s", out)
	}
	// The live-state pointer to diagnostics itself must still be there — only
	// the write-time doctrine sentence was trimmed, not the tool mention.
	if !strings.Contains(out, "**diagnostics**") {
		t.Errorf("session_start's Claude Code guidance dropped the diagnostics pointer entirely:\n%s", out)
	}
}

// TestGuidanceAlignment_ClaudeCodeEditLaneWarningStays is the converse guard:
// the edit-lane warning is NOT trimmed, because the instructions field's
// version of that doctrine deliberately omits the verbatim harness error
// strings a Claude Code agent needs to recognise ("File has not been read
// yet" / "File has been modified since read") — clienttemplates.
// DefaultTemplate's doc comment explains why a shared body must not quote a
// Claude-Code-specific error. TestSessionStart_EditLaneWarning_ClaudeCode
// already pins this from the session_start side; this test pins the other
// half — that the instructions field is not a substitute for it. Uses the
// same topology-active construction as the test above, though the warning is
// written unconditionally before the topology branch splits.
func TestGuidanceAlignment_ClaudeCodeEditLaneWarningStays(t *testing.T) {
	instr := mcp.InstructionsForClient("claude-code")
	if strings.Contains(instr, "File has not been read yet") {
		t.Errorf("claude-code MCP instructions unexpectedly quote the harness error string; "+
			"if this changed intentionally, re-review whether session_start's nativeEditLaneWarning "+
			"is still needed:\n%s", instr)
	}

	out := claudeCodeSessionStartOutput(t)
	if !strings.Contains(out, "File has not been read yet") {
		t.Errorf("session_start's Claude Code guidance must still carry the harness-error recognition "+
			"text, since the MCP instructions field does not:\n%s", out)
	}
}
