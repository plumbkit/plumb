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

// countInputSchemaDecls counts "InputSchema() json.RawMessage" method
// declarations across this package's non-test .go files — the same
// source-scan technique TestInputSchemasDeclareAdditionalProperties uses.
// Shared so the nested-schema walk below can assert its coverage map has one
// entry per declared tool, not just per file.
func countInputSchemaDecls(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		total += strings.Count(string(b), "InputSchema() json.RawMessage")
	}
	return total
}

// allToolSchemas returns every registered tool's Name() and InputSchema(),
// called directly on a nil pointer of each concrete tool type: every
// InputSchema/Name implementation in this package is receiver-independent
// (a package-level schema var, a literal, or a fmt.Sprintf over package-level
// constants — never a struct field), so this is safe and needs no
// constructor dependencies.
//
// internal/cli holds the live MCP registry (registerAllTools), but this test
// file is package tools (an internal test) and internal/cli imports
// internal/tools — importing it back here would be a compile-time cycle. So
// the tool set is enumerated by hand below, one line per tool, and guarded
// against drift by the coverage assertion in TestNestedSchemasRejectUnknownProperties:
// adding a tool file without adding its line here fails that count check
// immediately, rather than the walk silently skipping it.
func allToolSchemas() map[string]json.RawMessage {
	return map[string]json.RawMessage{
		(*AgentConfig)(nil).Name():          (*AgentConfig)(nil).InputSchema(),
		(*CallHierarchy)(nil).Name():        (*CallHierarchy)(nil).InputSchema(),
		(*CopyFile)(nil).Name():             (*CopyFile)(nil).InputSchema(),
		(*daemonInfo)(nil).Name():           (*daemonInfo)(nil).InputSchema(),
		(*DeleteFile)(nil).Name():           (*DeleteFile)(nil).InputSchema(),
		(*deleteMemoryTool)(nil).Name():     (*deleteMemoryTool)(nil).InputSchema(),
		(*Diagnostics)(nil).Name():          (*Diagnostics)(nil).InputSchema(),
		(*EditFile)(nil).Name():             (*EditFile)(nil).InputSchema(),
		(*ExecuteShellCommand)(nil).Name():  (*ExecuteShellCommand)(nil).InputSchema(),
		(*ExplainSymbol)(nil).Name():        (*ExplainSymbol)(nil).InputSchema(),
		(*FileDiff)(nil).Name():             (*FileDiff)(nil).InputSchema(),
		(*FileOutline)(nil).Name():          (*FileOutline)(nil).InputSchema(),
		(*FileStatus)(nil).Name():           (*FileStatus)(nil).InputSchema(),
		(*FindFiles)(nil).Name():            (*FindFiles)(nil).InputSchema(),
		(*findReplaceTool)(nil).Name():      (*findReplaceTool)(nil).InputSchema(),
		(*FindReferences)(nil).Name():       (*FindReferences)(nil).InputSchema(),
		(*FindSymbol)(nil).Name():           (*FindSymbol)(nil).InputSchema(),
		(*GetDefinition)(nil).Name():        (*GetDefinition)(nil).InputSchema(),
		(*Git)(nil).Name():                  (*Git)(nil).InputSchema(),
		(*GitInit)(nil).Name():              (*GitInit)(nil).InputSchema(),
		(*InsertAfterSymbol)(nil).Name():    (*InsertAfterSymbol)(nil).InputSchema(),
		(*InsertBeforeSymbol)(nil).Name():   (*InsertBeforeSymbol)(nil).InputSchema(),
		(*LeaveNote)(nil).Name():            (*LeaveNote)(nil).InputSchema(),
		(*ListDirectory)(nil).Name():        (*ListDirectory)(nil).InputSchema(),
		(*ListFiles)(nil).Name():            (*ListFiles)(nil).InputSchema(),
		(*listMemoriesTool)(nil).Name():     (*listMemoriesTool)(nil).InputSchema(),
		(*ListSymbols)(nil).Name():          (*ListSymbols)(nil).InputSchema(),
		(*ReadFile)(nil).Name():             (*ReadFile)(nil).InputSchema(),
		(*readMemoryTool)(nil).Name():       (*readMemoryTool)(nil).InputSchema(),
		(*ReadMultipleFiles)(nil).Name():    (*ReadMultipleFiles)(nil).InputSchema(),
		(*ReadSymbol)(nil).Name():           (*ReadSymbol)(nil).InputSchema(),
		(*relevantMemoriesTool)(nil).Name(): (*relevantMemoriesTool)(nil).InputSchema(),
		(*RenameFile)(nil).Name():           (*RenameFile)(nil).InputSchema(),
		(*renameSession)(nil).Name():        (*renameSession)(nil).InputSchema(),
		(*RenameSymbol)(nil).Name():         (*RenameSymbol)(nil).InputSchema(),
		(*ReplaceSymbolBody)(nil).Name():    (*ReplaceSymbolBody)(nil).InputSchema(),
		(*RunCommand)(nil).Name():           (*RunCommand)(nil).InputSchema(),
		(*SafeDeleteSymbol)(nil).Name():     (*SafeDeleteSymbol)(nil).InputSchema(),
		(*searchMemoriesTool)(nil).Name():   (*searchMemoriesTool)(nil).InputSchema(),
		(*SearchInFiles)(nil).Name():        (*SearchInFiles)(nil).InputSchema(),
		(*SessionStart)(nil).Name():         (*SessionStart)(nil).InputSchema(),
		(*ShareFindings)(nil).Name():        (*ShareFindings)(nil).InputSchema(),
		(*ShareIntent)(nil).Name():          (*ShareIntent)(nil).InputSchema(),
		(*StructuralQuery)(nil).Name():      (*StructuralQuery)(nil).InputSchema(),
		(*Tasks)(nil).Name():                (*Tasks)(nil).InputSchema(),
		(*TopologyAffected)(nil).Name():     (*TopologyAffected)(nil).InputSchema(),
		(*TopologyExplore)(nil).Name():      (*TopologyExplore)(nil).InputSchema(),
		(*TopologyImpact)(nil).Name():       (*TopologyImpact)(nil).InputSchema(),
		(*TopologyRoutes)(nil).Name():       (*TopologyRoutes)(nil).InputSchema(),
		(*TopologySearch)(nil).Name():       (*TopologySearch)(nil).InputSchema(),
		(*TopologyStatus)(nil).Name():       (*TopologyStatus)(nil).InputSchema(),
		(*TransactionApply)(nil).Name():     (*TransactionApply)(nil).InputSchema(),
		(*TypeHierarchy)(nil).Name():        (*TypeHierarchy)(nil).InputSchema(),
		(*UndoEdit)(nil).Name():             (*UndoEdit)(nil).InputSchema(),
		(*versionTool)(nil).Name():          (*versionTool)(nil).InputSchema(),
		(*WorkspaceSearch)(nil).Name():      (*WorkspaceSearch)(nil).InputSchema(),
		(*WorkspaceSessions)(nil).Name():    (*WorkspaceSessions)(nil).InputSchema(),
		(*WorkspaceSymbols)(nil).Name():     (*WorkspaceSymbols)(nil).InputSchema(),
		(*WriteFile)(nil).Name():            (*WriteFile)(nil).InputSchema(),
		(*writeMemoryTool)(nil).Name():      (*writeMemoryTool)(nil).InputSchema(),
	}
}

// nestedSchemaExemptions lists dotted tool.path locations that are
// *intentionally* open objects — a free-form map by design, not an oversight.
// Add an entry here only with a one-line justification; never to silence a
// real gap in a fixed-shape schema.
var nestedSchemaExemptions = map[string]string{
	// agent_config's "set" param is a free-form dotted-config-key -> value
	// batch (agentConfigArgs.Set is map[string]any, validated downstream by
	// the config allowlist, not by the schema). It declares no "properties",
	// so additionalProperties:false would reject every legitimate key.
	"agent_config.set": "free-form key/value config batch, no fixed property set",
}

// TestNestedSchemasRejectUnknownProperties guards the nested-args contract
// for every registered tool: every object level of its InputSchema —
// including array element schemas — must declare "additionalProperties":
// false, so the dispatch-path argument guard (internal/mcp/argguard.go, which
// recurses into nested objects) rejects an unknown *nested* key (e.g. an
// edits[].old_str typo) instead of silently dropping it. The file-level
// count in TestInputSchemasDeclareAdditionalProperties only proves top-level
// coverage; this walks each tool's assembled schema recursively, so a future
// tool that adds a nested object/array-of-object parameter and forgets
// "additionalProperties": false on it fails here automatically — either the
// walk catches the missing marker, or (if the tool itself is missing from
// allToolSchemas) the coverage assertion below does.
func TestNestedSchemasRejectUnknownProperties(t *testing.T) {
	schemas := allToolSchemas()
	if got, want := len(schemas), countInputSchemaDecls(t); got != want {
		t.Fatalf("allToolSchemas() has %d entries but the package declares %d "+
			"InputSchema() methods — a tool was added or removed without updating "+
			"the coverage map in allToolSchemas (schema_contract_test.go), so the "+
			"nested-schema walk below would silently skip it", got, want)
	}
	for name, schema := range schemas {
		var root any
		if err := json.Unmarshal(schema, &root); err != nil {
			t.Fatalf("%s: schema is not valid JSON: %v", name, err)
		}
		assertObjectsRejectExtra(t, name, root)
	}
}

// assertObjectsRejectExtra walks a JSON Schema value and fails for any object
// schema (type:object) that omits "additionalProperties": false, recursing
// through declared "properties" and array "items". path is a dotted
// tool-name-rooted JSON path (e.g. "edit_file.edits[].old_string") reported
// verbatim in failures, and checked against nestedSchemaExemptions before
// being flagged.
func assertObjectsRejectExtra(t *testing.T, path string, node any) {
	t.Helper()
	m, ok := node.(map[string]any)
	if !ok {
		return
	}
	switch m["type"] {
	case "object":
		if ap, ok := m["additionalProperties"].(bool); !ok || ap {
			if reason, exempt := nestedSchemaExemptions[path]; exempt {
				t.Logf("%s: additionalProperties:false intentionally omitted (%s)", path, reason)
			} else {
				t.Errorf("%s: object schema must set \"additionalProperties\": false", path)
			}
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
