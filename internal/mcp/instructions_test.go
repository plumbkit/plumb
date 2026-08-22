package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/clienttemplates"
	"github.com/plumbkit/plumb/internal/mcp"
)

// knownInstructionClients are the clientInfo.name values clienttemplates has
// a per-client body for today (internal/clienttemplates.ByClient) — the set
// InstructionsForClient renders a client-specific body for rather than
// falling back to DefaultInstructions.
var knownInstructionClients = []string{"claude-code", "codex", "gemini"}

// TestInstructions_KnownClientsWithinBudget proves every per-client render
// fits the ~1.5 KB channel budget (mcp.MaxInstructionsBytes) — comparable to
// the managed block's own clienttemplates.MaxLines guard, sized for a field
// that competes with the user's own prompt for context.
func TestInstructions_KnownClientsWithinBudget(t *testing.T) {
	for _, client := range knownInstructionClients {
		t.Run(client, func(t *testing.T) {
			got := mcp.InstructionsForClient(client)
			if got == "" {
				t.Fatalf("InstructionsForClient(%q) returned empty", client)
			}
			if n := len(got); n > mcp.MaxInstructionsBytes {
				t.Errorf("InstructionsForClient(%q) is %d bytes, want <= %d", client, n, mcp.MaxInstructionsBytes)
			}
		})
	}
}

// TestInstructions_MatchesSharedTemplate is the substance-parity check: a
// known client's rendered instructions must be EXACTLY its
// internal/clienttemplates body — the single source PLAN-366 draws from,
// shared with internal/setup's managed AGENTS.md/CLAUDE.md/GEMINI.md block
// (PLAN-364). Byte-identical rather than "contains the same ideas" is what
// makes this check structural: it can never silently drift the way two
// independently authored strings would.
func TestInstructions_MatchesSharedTemplate(t *testing.T) {
	for _, client := range knownInstructionClients {
		t.Run(client, func(t *testing.T) {
			want, ok := clienttemplates.ForClient(client)
			if !ok {
				t.Fatalf("clienttemplates has no body for %q — update knownInstructionClients", client)
			}
			got := mcp.InstructionsForClient(client)
			if got != want {
				t.Errorf("InstructionsForClient(%q) diverged from clienttemplates.ForClient(%q):\ngot:  %q\nwant: %q",
					client, client, got, want)
			}
		})
	}
}

// TestInstructions_ClaudeCodeCarriesRequiredContent pins the four doctrine
// pieces the card requires: lane rules, the refuse-to-break-the-build
// pointer (post PLAN-362's fail_on_new_errors), the peer mailbox pointer, and
// the session_start({detail:"brief"}) hint for a subagent that never
// receives this field itself (a subagent shares its parent's already-
// negotiated connection).
func TestInstructions_ClaudeCodeCarriesRequiredContent(t *testing.T) {
	got := mcp.InstructionsForClient("claude-code")
	for _, want := range []string{
		"Edit lane",                       // lane rules
		"fail_on_new_errors",              // refuse-to-break-the-build pointer (PLAN-362)
		"check_messages",                  // mailbox pointer
		`session_start({detail:"brief"})`, // subagent hint
	} {
		if !strings.Contains(got, want) {
			t.Errorf("claude-code instructions missing %q:\n%s", want, got)
		}
	}
}

// TestInstructions_UnknownClientFallsBackToDefault proves a client with no
// clienttemplates body (empty name, or one clientcaps does not recognise)
// gets DefaultInstructions — the same client-agnostic body internal/setup
// falls back to for a shared managed-block file — never an empty or panicking
// render.
func TestInstructions_UnknownClientFallsBackToDefault(t *testing.T) {
	for _, client := range []string{"", "some-agent-nobody-has-heard-of", "claude-desktop"} {
		t.Run(client, func(t *testing.T) {
			got := mcp.InstructionsForClient(client)
			if got != mcp.DefaultInstructions {
				t.Errorf("InstructionsForClient(%q) = %q, want DefaultInstructions", client, got)
			}
		})
	}
}

// TestInstructions_DefaultMatchesSharedFallback proves DefaultInstructions
// itself is sourced from the same place as internal/setup's client-agnostic
// managed-block fallback, not a separately authored constant.
func TestInstructions_DefaultMatchesSharedFallback(t *testing.T) {
	if mcp.DefaultInstructions != clienttemplates.DefaultTemplate {
		t.Errorf("mcp.DefaultInstructions diverged from clienttemplates.DefaultTemplate")
	}
}

// TestInstructions_InitializeRendersPerClient is the end-to-end proof: a real
// initialize exchange carrying a clientInfo.name plumb recognises gets that
// client's own body on the wire, not the generic default.
func TestInstructions_InitializeRendersPerClient(t *testing.T) {
	s := mcp.New(mcp.ServerInfo{Name: "test", Version: "0"})
	var out bytes.Buffer
	req := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"clientInfo":{"name":"claude-code","version":"1.0"}}}` + "\n"
	if err := s.Serve(context.Background(), strings.NewReader(req), &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	var resp map[string]any
	if err := json.NewDecoder(&out).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	result, _ := resp["result"].(map[string]any)
	instr, _ := result["instructions"].(string)
	want, ok := clienttemplates.ForClient("claude-code")
	if !ok {
		t.Fatal("clienttemplates has no claude-code body")
	}
	if instr != want {
		t.Errorf("initialize instructions for claude-code diverged from clienttemplates:\ngot:  %q\nwant: %q", instr, want)
	}
}
