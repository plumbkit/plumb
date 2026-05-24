package tools

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestInputSchemasDeclareAdditionalProperties guards the MCP server's opt-in
// argument guard: every tool's InputSchema must declare
// "additionalProperties": false so the server rejects unknown parameters and
// the advertised schema matches the runtime contract.
//
// The check counts InputSchema methods against additionalProperties markers per
// file rather than parsing the assembled schema, so it tolerates both styles in
// this package — schemas held in a package var (return xSchema) and schemas
// built inline by string concatenation (symbol_edits.go).
func TestInputSchemasDeclareAdditionalProperties(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		src := string(b)
		schemas := strings.Count(src, "InputSchema() json.RawMessage")
		if schemas == 0 {
			continue
		}
		if markers := strings.Count(src, "additionalProperties"); markers < schemas {
			t.Errorf("%s: %d InputSchema method(s) but only %d additionalProperties marker(s); "+
				"every tool schema must declare \"additionalProperties\": false", name, schemas, markers)
		}
	}
}

// TestNestedSchemasRejectUnknownProperties guards the nested-args contract for
// the two tools that take object / array-of-object parameters: every object
// level of their InputSchema — including array element schemas — must declare
// "additionalProperties": false, so the dispatch-path argument guard
// (internal/mcp/argguard.go, which recurses into nested objects) rejects an
// unknown *nested* key (e.g. an edits[].old_str typo) instead of silently
// dropping it. The file-level count above only proves top-level coverage; this
// walks the assembled schema so removing a nested marker fails loudly.
func TestNestedSchemasRejectUnknownProperties(t *testing.T) {
	for name, schema := range map[string]json.RawMessage{
		"edit_file":         editFileSchema,
		"transaction_apply": transactionApplySchema,
	} {
		var root any
		if err := json.Unmarshal(schema, &root); err != nil {
			t.Fatalf("%s: schema is not valid JSON: %v", name, err)
		}
		assertObjectsRejectExtra(t, name, root)
	}
}

// assertObjectsRejectExtra walks a JSON Schema value and fails for any object
// schema (type:object) that omits "additionalProperties": false, recursing
// through declared "properties" and array "items".
func assertObjectsRejectExtra(t *testing.T, path string, node any) {
	t.Helper()
	m, ok := node.(map[string]any)
	if !ok {
		return
	}
	switch m["type"] {
	case "object":
		if ap, ok := m["additionalProperties"].(bool); !ok || ap {
			t.Errorf("%s: object schema must set \"additionalProperties\": false", path)
		}
		if props, ok := m["properties"].(map[string]any); ok {
			for k, v := range props {
				assertObjectsRejectExtra(t, path+"."+k, v)
			}
		}
	case "array":
		if items, ok := m["items"]; ok {
			assertObjectsRejectExtra(t, path+"[]", items)
		}
	}
}
