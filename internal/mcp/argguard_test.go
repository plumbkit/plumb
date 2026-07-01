package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

const nameSchema = `{
  "type": "object",
  "properties": {"name": {"type": "string"}},
  "required": ["name"],
  "additionalProperties": false
}`

const editsSchema = `{
  "type": "object",
  "properties": {
    "edits": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {"old_string": {"type": "string"}, "new_string": {"type": "string"}},
        "required": ["old_string", "new_string"]
      }
    }
  },
  "required": ["edits"],
  "additionalProperties": false
}`

// editsRejectSchema mirrors editsSchema but sets additionalProperties:false on
// the nested edit item too — the production shape of edit_file/transaction_apply
// after the nested-args guard fix — so the guard rejects an unknown *nested* key.
const editsRejectSchema = `{
  "type": "object",
  "properties": {
    "edits": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {"old_string": {"type": "string"}, "new_string": {"type": "string"}},
        "required": ["old_string", "new_string"],
        "additionalProperties": false
      }
    }
  },
  "required": ["edits"],
  "additionalProperties": false
}`

func mustShape(t *testing.T, schema string) *shape {
	t.Helper()
	sh, ok := parseShape(json.RawMessage(schema))
	if !ok {
		t.Fatalf("parseShape(%s): not guardable", schema)
	}
	return sh
}

func TestResolveArgs(t *testing.T) {
	tests := []struct {
		name        string
		schema      string
		args        string
		wantErr     []string // substrings expected in the error (nil = success)
		wantWarn    []string // substrings expected across the warnings
		wantArgsSub []string // substrings expected in the rewritten args
	}{
		{
			name:        "table alias new_name → name (applied, not rejected)",
			schema:      nameSchema,
			args:        `{"new_name": "x"}`,
			wantWarn:    []string{`interpreted "new_name" as "name"`},
			wantArgsSub: []string{`"name":"x"`},
		},
		{
			name:        "normalisation alias startLine → start_line",
			schema:      `{"type":"object","properties":{"start_line":{"type":"integer"}},"additionalProperties":false}`,
			args:        `{"startLine": 5}`,
			wantWarn:    []string{`interpreted "startLine" as "start_line"`},
			wantArgsSub: []string{`"start_line":5`},
		},
		{
			name:        "nested alias edits[].old_str → old_string",
			schema:      editsSchema,
			args:        `{"edits":[{"old_str":"a","new_str":"b"}]}`,
			wantWarn:    []string{`interpreted "edits[].old_str" as "old_string"`, `interpreted "edits[].new_str" as "new_string"`},
			wantArgsSub: []string{`"old_string":"a"`, `"new_string":"b"`},
		},
		{
			name:    "nested unknown key rejected (additionalProperties:false on items)",
			schema:  editsRejectSchema,
			args:    `{"edits":[{"old_string":"a","new_string":"b","foo":1}]}`,
			wantErr: []string{`unknown parameter "edits[].foo"`, `valid parameters: old_string, new_string`},
		},
		{
			// file_path-canonical tool (read_file family): "path" is accepted.
			name:        "alias path → file_path",
			schema:      `{"type":"object","properties":{"file_path":{"type":"string"},"start_line":{"type":"integer"}},"required":["file_path"],"additionalProperties":false}`,
			args:        `{"path":"/tmp/x.go"}`,
			wantWarn:    []string{`interpreted "path" as "file_path"`},
			wantArgsSub: []string{`"file_path":"/tmp/x.go"`},
		},
		{
			// path-canonical tool (read_symbol): "file_path" is accepted in reverse.
			name:        "alias file_path → path",
			schema:      `{"type":"object","properties":{"path":{"type":"string"},"name":{"type":"string"}},"required":["path","name"],"additionalProperties":false}`,
			args:        `{"file_path":"/tmp/x.go","name":"Foo"}`,
			wantWarn:    []string{`interpreted "file_path" as "path"`},
			wantArgsSub: []string{`"path":"/tmp/x.go"`},
		},
		{
			// uri-canonical tool (LSP query/edit tools): a plain "path" reaches "uri".
			name:        "alias path → uri (uri-canonical tool)",
			schema:      `{"type":"object","properties":{"uri":{"type":"string"}},"required":["uri"],"additionalProperties":false}`,
			args:        `{"path":"/tmp/x.go"}`,
			wantWarn:    []string{`interpreted "path" as "uri"`},
			wantArgsSub: []string{`"uri":"/tmp/x.go"`},
		},
		{
			name:        "alias file_path → uri (uri-canonical tool)",
			schema:      `{"type":"object","properties":{"uri":{"type":"string"}},"required":["uri"],"additionalProperties":false}`,
			args:        `{"file_path":"/tmp/x.go"}`,
			wantWarn:    []string{`interpreted "file_path" as "uri"`},
			wantArgsSub: []string{`"uri":"/tmp/x.go"`},
		},
		{
			// file_path-canonical tool (read_file): a "uri" carried over from an LSP
			// tool now reaches "file_path" instead of erroring — the reported gap.
			name:        "alias uri → file_path (file-content tool)",
			schema:      `{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"],"additionalProperties":false}`,
			args:        `{"uri":"/tmp/x.go"}`,
			wantWarn:    []string{`interpreted "uri" as "file_path"`},
			wantArgsSub: []string{`"file_path":"/tmp/x.go"`},
		},
		{
			// path-canonical tool (list_directory): a "uri" reaches "path".
			name:        "alias uri → path (dir tool)",
			schema:      `{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`,
			args:        `{"uri":"/tmp/dir"}`,
			wantWarn:    []string{`interpreted "uri" as "path"`},
			wantArgsSub: []string{`"path":"/tmp/dir"`},
		},
		{
			// root-canonical tool (list_files): a "uri" reaches "root".
			name:        "alias uri → root (list_files)",
			schema:      `{"type":"object","properties":{"root":{"type":"string"}},"required":[],"additionalProperties":false}`,
			args:        `{"uri":"/tmp/dir"}`,
			wantWarn:    []string{`interpreted "uri" as "root"`},
			wantArgsSub: []string{`"root":"/tmp/dir"`},
		},
		{
			name:        "alias symbol → name (read_symbol)",
			schema:      `{"type":"object","properties":{"path":{"type":"string"},"name":{"type":"string"}},"required":["path","name"],"additionalProperties":false}`,
			args:        `{"path":"/tmp/x.go","symbol":"Foo"}`,
			wantWarn:    []string{`interpreted "symbol" as "name"`},
			wantArgsSub: []string{`"name":"Foo"`},
		},
		{
			// distance 2 (transposition) — too far for fuzzy auto-correct, so it is
			// still rejected, but close enough for the "did you mean" suggestion.
			name:    "genuine unknown is rejected with a suggestion",
			schema:  nameSchema,
			args:    `{"naem": "x"}`,
			wantErr: []string{`unknown parameter "naem"`, `did you mean "name"`, `valid parameters: name`},
		},
		{
			// distance 1, unique, eligible — auto-corrected with a typo warning.
			name:        "single-char typo is auto-corrected",
			schema:      nameSchema,
			args:        `{"namex": "Foo"}`,
			wantWarn:    []string{`corrected likely typo "namex" to "name"`},
			wantArgsSub: []string{`"name":"Foo"`},
		},
		{
			// a fuzzy match must never auto-correct to a safety-critical parameter.
			name:    "typo near a guarded param stays rejected",
			schema:  `{"type":"object","properties":{"confirm":{"type":"boolean"}},"required":[],"additionalProperties":false}`,
			args:    `{"confir": true}`,
			wantErr: []string{`unknown parameter "confir"`},
		},
		{
			// two params equidistant from the key — ambiguous, so no auto-correct.
			name:    "ambiguous fuzzy tie stays rejected",
			schema:  `{"type":"object","properties":{"name":{"type":"string"},"mane":{"type":"string"}},"required":[],"additionalProperties":false}`,
			args:    `{"nane": "x"}`,
			wantErr: []string{`unknown parameter "nane"`},
		},
		{
			// short keys are not fuzzy-corrected (distance-1 is coincidental there).
			name:    "short typo stays rejected",
			schema:  `{"type":"object","properties":{"to":{"type":"string"}},"required":[],"additionalProperties":false}`,
			args:    `{"go": "x"}`,
			wantErr: []string{`unknown parameter "go"`},
		},
		{
			name:    "missing required",
			schema:  nameSchema,
			args:    `{}`,
			wantErr: []string{`missing required parameter "name"`, `required: name`},
		},
		{
			name:   "valid args pass unchanged",
			schema: nameSchema,
			args:   `{"name": "build-fix"}`,
		},
		{
			name:    "non-object args",
			schema:  nameSchema,
			args:    `["name"]`,
			wantErr: []string{"arguments must be a JSON object"},
		},
		{
			name:   "empty args with no required pass",
			schema: `{"type":"object","properties":{"verbose":{"type":"boolean"}},"additionalProperties":false}`,
			args:   ``,
		},
		{
			name:   "additionalProperties true tolerates extras",
			schema: `{"type":"object","properties":{"name":{"type":"string"}},"additionalProperties":true}`,
			args:   `{"name":"x","extra":1}`,
		},
		{
			name:   "absent additionalProperties tolerates unknown (opt-in policy)",
			schema: `{"type":"object","properties":{"name":{"type":"string"}}}`,
			args:   `{"zzz":1}`,
		},
		{
			name:    "missing required enforced even without additionalProperties",
			schema:  `{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`,
			args:    `{"zzz":1}`,
			wantErr: []string{`missing required parameter "name"`},
		},
		{
			name:    "arg-less tool rejects any parameter",
			schema:  `{"type":"object","properties":{},"additionalProperties":false}`,
			args:    `{"foo":1}`,
			wantErr: []string{`unknown parameter "foo"`, "this tool accepts no parameters"},
		},
		{
			name:   "arg-less tool accepts empty object",
			schema: `{"type":"object","properties":{},"additionalProperties":false}`,
			args:   `{}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sh := mustShape(t, tt.schema)
			out, warnings, err := resolveArgs(sh, json.RawMessage(tt.args), "test_tool")

			if len(tt.wantErr) > 0 {
				if err == nil {
					t.Fatalf("resolveArgs(%s) = nil error, want error", tt.args)
				}
				for _, sub := range tt.wantErr {
					if !strings.Contains(err.Error(), sub) {
						t.Errorf("error %q missing %q", err.Error(), sub)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveArgs(%s) = %v, want nil", tt.args, err)
			}
			joined := strings.Join(warnings, " | ")
			for _, sub := range tt.wantWarn {
				if !strings.Contains(joined, sub) {
					t.Errorf("warnings %q missing %q", joined, sub)
				}
			}
			if len(tt.wantWarn) == 0 && len(warnings) != 0 {
				t.Errorf("unexpected warnings: %v", warnings)
			}
			compact := strings.ReplaceAll(string(out), " ", "")
			for _, sub := range tt.wantArgsSub {
				if !strings.Contains(compact, sub) {
					t.Errorf("rewritten args %q missing %q", compact, sub)
				}
			}
		})
	}
}

// TestResolveArgs_ToolNamePrefix asserts the tool name is threaded into the
// rejection message so an agent that confused two tools sees which schema was
// consulted, and that an empty tool name suppresses the prefix (in-package /
// test call sites). The table cases above pass "test_tool" but don't assert the
// prefix; this covers that contract directly.
func TestResolveArgs_ToolNamePrefix(t *testing.T) {
	sh := mustShape(t, nameSchema)

	_, _, err := resolveArgs(sh, json.RawMessage(`{"bogus":1}`), "write_file")
	if err == nil {
		t.Fatal("expected error for unknown parameter")
	}
	if !strings.HasPrefix(err.Error(), "write_file: ") {
		t.Errorf("error should be prefixed with the tool name, got: %q", err.Error())
	}

	_, _, err = resolveArgs(sh, json.RawMessage(`{"bogus":1}`), "")
	if err == nil {
		t.Fatal("expected error for unknown parameter")
	}
	if !strings.HasPrefix(err.Error(), "unknown parameter") {
		t.Errorf("empty tool name should not add a prefix, got: %q", err.Error())
	}
}

func TestParseShape_FailOpen(t *testing.T) {
	for _, schema := range []string{
		`{"type":"string"}`,                    // not an object schema
		`{"properties":{}}`,                    // no type
		`not json`,                             // unparseable
		`{"type":"object","properties":["x"]}`, // properties not an object
	} {
		if _, ok := parseShape(json.RawMessage(schema)); ok {
			t.Errorf("parseShape(%s) = guardable, want fail-open", schema)
		}
	}
}

func TestResolveArgs_PreservesDeclarationOrderInError(t *testing.T) {
	sh := mustShape(t, `{
  "type": "object",
  "properties": {"zebra": {"type": "string"}, "alpha": {"type": "string"}, "mike": {"type": "string"}},
  "additionalProperties": false
}`)
	_, _, err := resolveArgs(sh, json.RawMessage(`{"qqqq":1}`), "test_tool")
	if err == nil {
		t.Fatal("expected error for unknown key")
	}
	if !strings.Contains(err.Error(), "valid parameters: zebra, alpha, mike") {
		t.Errorf("declaration order not preserved: %q", err.Error())
	}
}

func TestClosest_NoMisleadingSuggestion(t *testing.T) {
	if got := closest("xq", []string{"name"}); got != "" {
		t.Errorf("closest(\"xq\") = %q, want \"\"", got)
	}
	if got := closest("nme", []string{"name"}); got != "name" {
		t.Errorf("closest(\"nme\") = %q, want \"name\"", got)
	}
}

func TestLevenshtein(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"name", "name", 0},
		{"nme", "name", 1},
		{"new_name", "name", 4},
		{"", "abc", 3},
		{"abc", "", 3},
	}
	for _, c := range cases {
		if got := levenshtein(c.a, c.b); got != c.want {
			t.Errorf("levenshtein(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}
