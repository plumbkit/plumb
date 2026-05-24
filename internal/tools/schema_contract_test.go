package tools

import (
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
