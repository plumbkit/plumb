package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestResolveArgs_ExpandedAliases covers the empirically-driven aliases added for
// the alias-tolerant-schema work: each rewrites on a tool that declares the
// canonical, and the eligibility rule keeps it a no-op where the alias name is
// itself the canonical parameter.
func TestResolveArgs_ExpandedAliases(t *testing.T) {
	tests := []struct {
		name        string
		schema      string
		args        string
		wantWarn    string // "" => expect no rewrite
		wantArgsSub string
	}{
		{
			name:        "pattern → query (search-by-name tool)",
			schema:      `{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}`,
			args:        `{"pattern":"Foo"}`,
			wantWarn:    `interpreted "pattern" as "query"`,
			wantArgsSub: `"query":"Foo"`,
		},
		{
			name:        "query → pattern (search_in_files family) still works",
			schema:      `{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string"}},"required":["pattern"],"additionalProperties":false}`,
			args:        `{"query":"Foo"}`,
			wantWarn:    `interpreted "query" as "pattern"`,
			wantArgsSub: `"pattern":"Foo"`,
		},
		{
			name:        "is_regex → use_regex",
			schema:      `{"type":"object","properties":{"pattern":{"type":"string"},"use_regex":{"type":"boolean"}},"required":["pattern"],"additionalProperties":false}`,
			args:        `{"pattern":"x","is_regex":true}`,
			wantWarn:    `interpreted "is_regex" as "use_regex"`,
			wantArgsSub: `"use_regex":true`,
		},
		{
			name:        "path → root (list_files)",
			schema:      `{"type":"object","properties":{"root":{"type":"string"},"pattern":{"type":"string"}},"additionalProperties":false}`,
			args:        `{"path":"/dir"}`,
			wantWarn:    `interpreted "path" as "root"`,
			wantArgsSub: `"root":"/dir"`,
		},
		{
			name:        "file_paths → paths (read_multiple_files)",
			schema:      `{"type":"object","properties":{"paths":{"type":"array","items":{"type":"string"}}},"required":["paths"],"additionalProperties":false}`,
			args:        `{"file_paths":["/a","/b"]}`,
			wantWarn:    `interpreted "file_paths" as "paths"`,
			wantArgsSub: `"paths":["/a","/b"]`,
		},
		{
			name:        "workspace_path → workspace (session_start)",
			schema:      `{"type":"object","properties":{"workspace":{"type":"string"}},"additionalProperties":false}`,
			args:        `{"workspace_path":"/w"}`,
			wantWarn:    `interpreted "workspace_path" as "workspace"`,
			wantArgsSub: `"workspace":"/w"`,
		},
		{
			name:        "find/replace → pattern/replacement (find_replace)",
			schema:      `{"type":"object","properties":{"path":{"type":"string"},"pattern":{"type":"string"},"replacement":{"type":"string"}},"required":["path","pattern","replacement"],"additionalProperties":false}`,
			args:        `{"path":"/d","find":"a","replace":"b"}`,
			wantWarn:    `interpreted "find" as "pattern"`,
			wantArgsSub: `"replacement":"b"`,
		},
		{
			name:        "source/destination → from/to (rename_file)",
			schema:      `{"type":"object","properties":{"from":{"type":"string"},"to":{"type":"string"}},"required":["from","to"],"additionalProperties":false}`,
			args:        `{"source":"/a","destination":"/b"}`,
			wantWarn:    `interpreted "source" as "from"`,
			wantArgsSub: `"to":"/b"`,
		},
		{
			name:        "symbol → symbol_name (position tool)",
			schema:      `{"type":"object","properties":{"uri":{"type":"string"},"symbol_name":{"type":"string"}},"required":["uri"],"additionalProperties":false}`,
			args:        `{"uri":"file:///x.go","symbol":"Foo"}`,
			wantWarn:    `interpreted "symbol" as "symbol_name"`,
			wantArgsSub: `"symbol_name":"Foo"`,
		},
		{
			name:        "eligibility no-op: pattern stays canonical on its own tool",
			schema:      `{"type":"object","properties":{"pattern":{"type":"string"}},"required":["pattern"],"additionalProperties":false}`,
			args:        `{"pattern":"x"}`,
			wantWarn:    "",
			wantArgsSub: `"pattern":"x"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sh := mustShape(t, tc.schema)
			out, warnings, err := resolveArgs(sh, json.RawMessage(tc.args), "tool")
			if err != nil {
				t.Fatalf("resolveArgs error: %v", err)
			}
			joined := strings.Join(warnings, "; ")
			if tc.wantWarn == "" {
				if len(warnings) != 0 {
					t.Errorf("expected no rewrite, got warnings: %s", joined)
				}
			} else if !strings.Contains(joined, tc.wantWarn) {
				t.Errorf("warnings = %q, want substring %q", joined, tc.wantWarn)
			}
			if !strings.Contains(string(out), tc.wantArgsSub) {
				t.Errorf("rewritten args = %s, want substring %q", out, tc.wantArgsSub)
			}
		})
	}
}
