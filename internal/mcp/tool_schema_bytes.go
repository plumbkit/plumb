package mcp

// tool_schema_bytes.go — the per-tool wire byte size of the advertised
// tools/list surface, split out from server_handlers.go (PLAN-367) so the
// surcharge-estimate helper has its own small home rather than pushing the
// tools/list handler over the file-size cap.

import "encoding/json"

// ToolSchemaBytes reports, for every REGISTERED tool (profile filter not yet
// applied), the wire byte size of its tools/list entry — name, description,
// and input schema, JSON-marshalled exactly as handleToolsList would encode
// it for THIS connection (minus the optional `_meta` block, which is small and
// profile-dependent, not part of the schema itself). "For this connection"
// includes the identity property DeclareIdentityArg adds to every schema for
// a client that strips undeclared arguments: that client really receives
// those bytes, on every tool. This is the raw material for a per-request
// tool-schema surcharge estimate (see clientcaps.ProfileSurcharge); a caller
// applies its own visibility predicate (ToolFilter) to total only the tools a
// given profile actually serves.
func (s *Server) ToolSchemaBytes() map[string]int {
	type toolDef struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"inputSchema"`
	}
	snaps := s.snapshotTools()
	declareIdentity := s.declaresIdentityArg()
	out := make(map[string]int, len(snaps))
	for _, sn := range snaps {
		b, err := json.Marshal(toolDef{Name: sn.name, Description: sn.description, InputSchema: advertisedSchema(sn.schema, declareIdentity)})
		if err != nil {
			continue
		}
		out[sn.name] = len(b)
	}
	return out
}

// declaresIdentityArg reports whether this connection's tools/list adds the
// identity property to every schema. Resolved per call, like handleToolsList,
// because the client's identity arrives with initialize.
func (s *Server) declaresIdentityArg() bool {
	return s.DeclareIdentityArg != nil && s.DeclareIdentityArg()
}

// advertisedSchema is a tool's inputSchema as tools/list serves it: the
// published schema, with the identity property added when declareIdentity.
// handleToolsList and ToolSchemaBytes both go through it, so the size report
// cannot drift from the payload.
func advertisedSchema(schema json.RawMessage, declareIdentity bool) json.RawMessage {
	if declareIdentity {
		return withIdentityProperty(schema)
	}
	return schema
}
