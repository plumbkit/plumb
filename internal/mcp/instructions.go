package mcp

import (
	"encoding/json"

	"github.com/plumbkit/plumb/internal/clientcaps"
	"github.com/plumbkit/plumb/internal/clienttemplates"
)

// DefaultInstructions is the client-agnostic fallback returned in the MCP
// initialize response's "instructions" field for a client InstructionsForClient
// has no per-client body for. Per the MCP spec, clients that surface this
// field show it to the model as a system-prompt-style hint.
//
// It is internal/clienttemplates.DefaultTemplate verbatim. PLAN-364 once also
// wrote that body into a managed block inside the project's own AGENTS.md/
// CLAUDE.md/GEMINI.md; that writer is gone, so this field is now the sole
// channel carrying the doctrine — which is why its substance must stay
// aligned with clienttemplates rather than drifting a second copy.
const DefaultInstructions = clienttemplates.DefaultTemplate

// MaxInstructionsBytes is the size budget every render is expected to fit
// inside, sized for a channel that competes with the user's own prompt for
// context budget. Since the managed-block writer's line budget
// (clienttemplates.MaxLines) went with it, this is the ONLY size guard these
// bodies have — so the budget test covers DefaultInstructions as well as
// every per-client body. Enforced by TestInstructions in internal/mcp's own
// tests, not at render time: every body it can currently select from is
// already well inside it.
const MaxInstructionsBytes = 1536

// InstructionsForClient resolves clientName — the raw MCP clientInfo.name
// reported at initialize — to that client's instruction body
// (internal/clienttemplates), the single source of this doctrine. Content: the edit
// lane, the refuse-to-break-the-build pointer (fail_on_new_errors /
// await_diagnostics, post PLAN-362), the peer mailbox pointer, and — for
// claude-code, the only body that carries it — the session_start({detail:
// "brief"}) hint for a subagent that never sees this field itself (a
// subagent shares its parent's already-negotiated MCP connection, so
// initialize, and this field, never runs again for it).
//
// Detection matches clientcaps.Lookup's canonical name — the same resolution
// internal/cli's autoProfileFor keys client behaviour off — so a client with
// no evidence-gated SupportsMCPInstructions (clientcaps.go) still gets a
// correct render; that flag records observed CONSUMPTION, not eligibility to
// be sent one; an unaware client ignores an unknown response field, exactly
// like today's behaviour before this function existed.
//
// Falls back to DefaultInstructions when clientName resolves to no known
// canonical client, or to one clienttemplates has no per-client body for yet.
func InstructionsForClient(clientName string) string {
	key := clientcaps.Lookup(clientName).Name
	if body, ok := clienttemplates.ForClient(key); ok {
		return body
	}
	return DefaultInstructions
}

// clientInfoNameFromParams extracts the client's self-reported name from the
// initialize params' clientInfo.name field — the same field fireInitParamHooks
// (server_handlers.go) parses for OnClientInfo, re-extracted here (rather than
// threaded through) because handleInitialize needs it before OnClientInfo's
// callback has any chance to run, to pick this response's own "instructions"
// render. Fail-safe like the other init-param extractors: a missing
// clientInfo, wrong type, or malformed JSON yields "", which
// InstructionsForClient treats as an unrecognised client.
func clientInfoNameFromParams(params json.RawMessage) string {
	if params == nil {
		return ""
	}
	var p struct {
		ClientInfo struct {
			Name string `json:"name"`
		} `json:"clientInfo"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return ""
	}
	return p.ClientInfo.Name
}
