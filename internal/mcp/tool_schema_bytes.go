package mcp

// tool_schema_bytes.go — the per-tool wire byte size of the advertised
// tools/list surface, split out from server_handlers.go (PLAN-367) so the
// surcharge-estimate helper has its own small home rather than pushing the
// tools/list handler over the file-size cap.

import "encoding/json"

// ToolSchemaBytes reports, for every REGISTERED tool (profile filter not yet
// applied), the wire byte size of its tools/list entry — name, description,
// and input schema, JSON-marshalled exactly as handleToolsList would encode
// it (minus the optional `_meta` block, which is small and profile-dependent,
// not part of the schema itself). This is the raw material for a per-request
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
	out := make(map[string]int, len(snaps))
	for _, sn := range snaps {
		b, err := json.Marshal(toolDef{Name: sn.name, Description: sn.description, InputSchema: sn.schema})
		if err != nil {
			continue
		}
		out[sn.name] = len(b)
	}
	return out
}
