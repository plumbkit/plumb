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
			name:        "path → root (a root-canonical shape)",
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
			// call_hierarchy's real shape after RC3 2a: uri + line + character +
			// symbol_name. The alias picks symbol_name over the (absent-here) name/
			// query candidates, so `symbol` reaches the by-name path.
			name:        "symbol → symbol_name (call_hierarchy shape)",
			schema:      `{"type":"object","properties":{"uri":{"type":"string"},"line":{"type":"integer"},"character":{"type":"integer"},"symbol_name":{"type":"string"},"direction":{"type":"string"}},"required":["uri"],"additionalProperties":false}`,
			args:        `{"uri":"file:///x.go","symbol":"Foo"}`,
			wantWarn:    `interpreted "symbol" as "symbol_name"`,
			wantArgsSub: `"symbol_name":"Foo"`,
		},
		{
			name:        "task → slot (run_task)",
			schema:      `{"type":"object","properties":{"slot":{"type":"string"},"target":{"type":"string"}},"required":["slot"],"additionalProperties":false}`,
			args:        `{"task":"lint"}`,
			wantWarn:    `interpreted "task" as "slot"`,
			wantArgsSub: `"slot":"lint"`,
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

// TestResolveArgs_TransformAliases covers the value-transform aliases: the
// invert-bool, constant, and scalar→singleton-array transforms fire only when
// the value fits, never override an explicitly-set canonical (the eligibility
// rule), and say what they did in the warning.
func TestResolveArgs_TransformAliases(t *testing.T) {
	const searchSchema = `{"type":"object","properties":{"pattern":{"type":"string"},"case_sensitive":{"type":"boolean"},"use_regex":{"type":"boolean"},"dry_run":{"type":"boolean"}},"required":["pattern"],"additionalProperties":false}`
	const kindsSchema = `{"type":"object","properties":{"query":{"type":"string"},"kinds":{"type":"array","items":{"type":"string"}}},"required":["query"],"additionalProperties":false}`
	const urisSchema = `{"type":"object","properties":{"uris":{"type":"array","items":{"type":"string"}}},"additionalProperties":false}`

	tests := []struct {
		name        string
		schema      string
		args        string
		wantErr     string // "" => expect success
		wantWarn    string // "" => expect no rewrite
		wantArgsSub string
	}{
		{
			name:        "-i:true → case_sensitive:false (invert-bool)",
			schema:      searchSchema,
			args:        `{"pattern":"x","-i":true}`,
			wantWarn:    `interpreted "-i" as "case_sensitive" (inverted value)`,
			wantArgsSub: `"case_sensitive":false`,
		},
		{
			name:        "-i:false → case_sensitive:true (inversion honours the value)",
			schema:      searchSchema,
			args:        `{"pattern":"x","-i":false}`,
			wantWarn:    `interpreted "-i" as "case_sensitive" (inverted value)`,
			wantArgsSub: `"case_sensitive":true`,
		},
		{
			name:        "ignore_case → case_sensitive (invert-bool)",
			schema:      searchSchema,
			args:        `{"pattern":"x","ignore_case":true}`,
			wantWarn:    `interpreted "ignore_case" as "case_sensitive" (inverted value)`,
			wantArgsSub: `"case_sensitive":false`,
		},
		{
			name:     "transform does not fire when the canonical is explicitly set",
			schema:   searchSchema,
			args:     `{"pattern":"x","case_sensitive":false,"-i":true}`,
			wantErr:  `unknown parameter "-i"`,
			wantWarn: "",
		},
		{
			name:        "preview:true → dry_run:true (constant)",
			schema:      searchSchema,
			args:        `{"pattern":"x","preview":true}`,
			wantWarn:    `interpreted "preview" as "dry_run" (forced true)`,
			wantArgsSub: `"dry_run":true`,
		},
		{
			name:     "preview:false does not fit the constant — left for validation",
			schema:   searchSchema,
			args:     `{"pattern":"x","preview":false}`,
			wantErr:  `unknown parameter "preview"`,
			wantWarn: "",
		},
		{
			name:        "regex:true → use_regex:true (constant beats the pattern rename)",
			schema:      searchSchema,
			args:        `{"pattern":"x","regex":true}`,
			wantWarn:    `interpreted "regex" as "use_regex" (forced true)`,
			wantArgsSub: `"use_regex":true`,
		},
		{
			name:        "regex:<string> still renames to pattern (existing behaviour)",
			schema:      searchSchema,
			args:        `{"regex":"foo.*"}`,
			wantWarn:    `interpreted "regex" as "pattern"`,
			wantArgsSub: `"pattern":"foo.*"`,
		},
		{
			name:        "kind → kinds wraps the scalar",
			schema:      kindsSchema,
			args:        `{"query":"f","kind":"function"}`,
			wantWarn:    `interpreted "kind" as "kinds" (wrapped in a single-element array)`,
			wantArgsSub: `"kinds":["function"]`,
		},
		{
			name:        "kind with an array value is a plain rename, not a wrap",
			schema:      kindsSchema,
			args:        `{"query":"f","kind":["function","type"]}`,
			wantWarn:    `interpreted "kind" as "kinds"`,
			wantArgsSub: `"kinds":["function","type"]`,
		},
		{
			name:        "path → uris wraps the scalar",
			schema:      urisSchema,
			args:        `{"path":"/a.go"}`,
			wantWarn:    `interpreted "path" as "uris" (wrapped in a single-element array)`,
			wantArgsSub: `"uris":["/a.go"]`,
		},
		{
			name:        "file with an array value is a plain rename to uris",
			schema:      urisSchema,
			args:        `{"file":["/a.go","/b.go"]}`,
			wantWarn:    `interpreted "file" as "uris"`,
			wantArgsSub: `"uris":["/a.go","/b.go"]`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sh := mustShape(t, tc.schema)
			out, warnings, err := resolveArgs(sh, json.RawMessage(tc.args), "tool")
			joined := strings.Join(warnings, "; ")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want substring %q (warnings: %s)", err, tc.wantErr, joined)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveArgs error: %v", err)
			}
			if !strings.Contains(joined, tc.wantWarn) {
				t.Errorf("warnings = %q, want substring %q", joined, tc.wantWarn)
			}
			if !strings.Contains(string(out), tc.wantArgsSub) {
				t.Errorf("rewritten args = %s, want substring %q", out, tc.wantArgsSub)
			}
		})
	}
}

// TestResolveArgs_ExpandedTable covers the phase-2 plain-rename table rows
// against synthetic shapes (the rows whose real tools need heavy fixtures —
// topology, git — are covered here; the rest also get real-schema round-trips
// in argalias_realschema_test.go).
func TestResolveArgs_ExpandedTable(t *testing.T) {
	tests := []struct {
		name        string
		schema      string
		args        string
		wantWarn    string
		wantArgsSub string
	}{
		{
			name:        "depth → max_depth (find_files)",
			schema:      `{"type":"object","properties":{"root":{"type":"string"},"max_depth":{"type":"integer"}},"additionalProperties":false}`,
			args:        `{"root":"/d","depth":2}`,
			wantWarn:    `interpreted "depth" as "max_depth"`,
			wantArgsSub: `"max_depth":2`,
		},
		{
			name:        "max_depth → depth (topology tools)",
			schema:      `{"type":"object","properties":{"name":{"type":"string"},"depth":{"type":"integer"}},"required":["name"],"additionalProperties":false}`,
			args:        `{"name":"f","max_depth":3}`,
			wantWarn:    `interpreted "max_depth" as "depth"`,
			wantArgsSub: `"depth":3`,
		},
		{
			name:        "command → subcommand (git)",
			schema:      `{"type":"object","properties":{"subcommand":{"type":"string"},"args":{"type":"array","items":{"type":"string"}}},"required":["subcommand"],"additionalProperties":false}`,
			args:        `{"command":"status"}`,
			wantWarn:    `interpreted "command" as "subcommand"`,
			wantArgsSub: `"subcommand":"status"`,
		},
		{
			name:        "ext → extension (find_files)",
			schema:      `{"type":"object","properties":{"pattern":{"type":"string"},"extension":{"type":"string"}},"additionalProperties":false}`,
			args:        `{"pattern":"*","ext":"go"}`,
			wantWarn:    `interpreted "ext" as "extension"`,
			wantArgsSub: `"extension":"go"`,
		},
		{
			name:        "sort → sort_by (find_files)",
			schema:      `{"type":"object","properties":{"path":{"type":"string"},"sort_by":{"type":"string"}},"additionalProperties":false}`,
			args:        `{"path":"/d","sort":"size"}`,
			wantWarn:    `interpreted "sort" as "sort_by"`,
			wantArgsSub: `"sort_by":"size"`,
		},
		{
			name:        "order_by → sort_by",
			schema:      `{"type":"object","properties":{"path":{"type":"string"},"sort_by":{"type":"string"}},"additionalProperties":false}`,
			args:        `{"path":"/d","order_by":"modified"}`,
			wantWarn:    `interpreted "order_by" as "sort_by"`,
			wantArgsSub: `"sort_by":"modified"`,
		},
		{
			name:        "hidden → include_hidden",
			schema:      `{"type":"object","properties":{"pattern":{"type":"string"},"include_hidden":{"type":"boolean"}},"additionalProperties":false}`,
			args:        `{"pattern":"x","hidden":true}`,
			wantWarn:    `interpreted "hidden" as "include_hidden"`,
			wantArgsSub: `"include_hidden":true`,
		},
		{
			name:        "data → content (write_file)",
			schema:      `{"type":"object","properties":{"file_path":{"type":"string"},"content":{"type":"string"}},"required":["file_path"],"additionalProperties":false}`,
			args:        `{"file_path":"/f","data":"hello"}`,
			wantWarn:    `interpreted "data" as "content"`,
			wantArgsSub: `"content":"hello"`,
		},
		{
			name:        "changes → edits (edit_file)",
			schema:      `{"type":"object","properties":{"file_path":{"type":"string"},"edits":{"type":"array","items":{"type":"object","properties":{"new_string":{"type":"string"}},"required":["new_string"]}}},"required":["file_path"],"additionalProperties":false}`,
			args:        `{"file_path":"/f","changes":[{"new_string":"x"}]}`,
			wantWarn:    `interpreted "changes" as "edits"`,
			wantArgsSub: `"edits":[{"new_string":"x"}]`,
		},
		{
			name:        "replacements → edits",
			schema:      `{"type":"object","properties":{"file_path":{"type":"string"},"edits":{"type":"array","items":{"type":"object","properties":{"new_string":{"type":"string"}},"required":["new_string"]}}},"required":["file_path"],"additionalProperties":false}`,
			args:        `{"file_path":"/f","replacements":[{"new_string":"x"}]}`,
			wantWarn:    `interpreted "replacements" as "edits"`,
			wantArgsSub: `"edits":[{"new_string":"x"}]`,
		},
		{
			name:        "n_lines → limit (read_file)",
			schema:      `{"type":"object","properties":{"file_path":{"type":"string"},"limit":{"type":"integer"}},"required":["file_path"],"additionalProperties":false}`,
			args:        `{"file_path":"/f","n_lines":10}`,
			wantWarn:    `interpreted "n_lines" as "limit"`,
			wantArgsSub: `"limit":10`,
		},
		{
			name:        "num_lines → max_results (search tool without limit)",
			schema:      `{"type":"object","properties":{"pattern":{"type":"string"},"max_results":{"type":"integer"}},"required":["pattern"],"additionalProperties":false}`,
			args:        `{"pattern":"x","num_lines":5}`,
			wantWarn:    `interpreted "num_lines" as "max_results"`,
			wantArgsSub: `"max_results":5`,
		},
		{
			name:        "max_lines → recent_limit (sessions-style cap)",
			schema:      `{"type":"object","properties":{"recent_limit":{"type":"integer"}},"additionalProperties":false}`,
			args:        `{"max_lines":3}`,
			wantWarn:    `interpreted "max_lines" as "recent_limit"`,
			wantArgsSub: `"recent_limit":3`,
		},
		{
			name:        "line_count → max_matches (read_file search cap)",
			schema:      `{"type":"object","properties":{"pattern":{"type":"string"},"max_matches":{"type":"integer"}},"additionalProperties":false}`,
			args:        `{"pattern":"x","line_count":7}`,
			wantWarn:    `interpreted "line_count" as "max_matches"`,
			wantArgsSub: `"max_matches":7`,
		},
		{
			name:        "limit → max_results where limit is not declared",
			schema:      `{"type":"object","properties":{"pattern":{"type":"string"},"max_results":{"type":"integer"}},"required":["pattern"],"additionalProperties":false}`,
			args:        `{"pattern":"x","limit":5}`,
			wantWarn:    `interpreted "limit" as "max_results"`,
			wantArgsSub: `"max_results":5`,
		},
		{
			name:        "limit stays canonical where declared (no rewrite)",
			schema:      `{"type":"object","properties":{"limit":{"type":"integer"},"max_results":{"type":"integer"}},"additionalProperties":false}`,
			args:        `{"limit":5}`,
			wantWarn:    "",
			wantArgsSub: `"limit":5`,
		},
		{
			name:        "max_matches → max_results where max_matches is not declared",
			schema:      `{"type":"object","properties":{"pattern":{"type":"string"},"max_results":{"type":"integer"}},"required":["pattern"],"additionalProperties":false}`,
			args:        `{"pattern":"x","max_matches":9}`,
			wantWarn:    `interpreted "max_matches" as "max_results"`,
			wantArgsSub: `"max_results":9`,
		},
		{
			name:        "max_count → max_matches",
			schema:      `{"type":"object","properties":{"pattern":{"type":"string"},"max_matches":{"type":"integer"}},"additionalProperties":false}`,
			args:        `{"pattern":"x","max_count":4}`,
			wantWarn:    `interpreted "max_count" as "max_matches"`,
			wantArgsSub: `"max_matches":4`,
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
