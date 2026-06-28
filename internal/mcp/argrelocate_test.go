package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

// editLikeSchema mirrors edit_file: a file_path, an edits[] array of objects, and
// top-level expected_mtime / confirm parameters.
const editLikeSchema = `{
  "type":"object",
  "properties":{
    "file_path":{"type":"string"},
    "edits":{"type":"array","items":{"type":"object",
      "properties":{"old_string":{"type":"string"},"new_string":{"type":"string"}},
      "required":["new_string"],"additionalProperties":false}},
    "expected_mtime":{"type":"string"},
    "confirm":{"type":"boolean"}
  },
  "required":["file_path","edits"],
  "additionalProperties":false
}`

func resolveOK(t *testing.T, schema, args string) (map[string]any, []string) {
	t.Helper()
	sh := mustShape(t, schema)
	out, warnings, err := resolveArgs(sh, json.RawMessage(args), "edit_like")
	if err != nil {
		t.Fatalf("resolveArgs(%s) unexpected error: %v", args, err)
	}
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	return obj, warnings
}

// TestRelocate_WrapTopLevelEdit wraps scattered top-level edit keys into the
// absent edits[] array.
func TestRelocate_WrapTopLevelEdit(t *testing.T) {
	obj, warnings := resolveOK(t, editLikeSchema, `{"file_path":"/x","old_string":"a","new_string":"b"}`)

	edits, ok := obj["edits"].([]any)
	if !ok || len(edits) != 1 {
		t.Fatalf("expected one synthesised edit, got %#v", obj["edits"])
	}
	e := edits[0].(map[string]any)
	if e["old_string"] != "a" || e["new_string"] != "b" {
		t.Errorf("wrapped edit lost fields: %#v", e)
	}
	if _, stillTop := obj["old_string"]; stillTop {
		t.Error("old_string should have moved off the top level")
	}
	if !hasWarning(warnings, "wrapped") {
		t.Errorf("expected a wrap warning, got %v", warnings)
	}
}

// TestRelocate_HoistFromElement lifts a top-level parameter mistakenly placed
// inside an edits[] item back to the top level.
func TestRelocate_HoistFromElement(t *testing.T) {
	obj, warnings := resolveOK(t, editLikeSchema,
		`{"file_path":"/x","edits":[{"new_string":"b","expected_mtime":"2026-01-01T00:00:00Z"}]}`)

	if obj["expected_mtime"] != "2026-01-01T00:00:00Z" {
		t.Errorf("expected_mtime not hoisted to top level: %#v", obj["expected_mtime"])
	}
	e := obj["edits"].([]any)[0].(map[string]any)
	if _, stillNested := e["expected_mtime"]; stillNested {
		t.Error("expected_mtime should have moved out of the edit item")
	}
	if !hasWarning(warnings, "moved") {
		t.Errorf("expected a hoist warning, got %v", warnings)
	}
}

// TestRelocate_UnknownStaysRejected proves relocation moves only DECLARED
// parameters: a key that exists at no level is still rejected, never invented
// into the array or hoisted. (Relocation is exact-name, so it needs no safety
// gate — it can only place a key where the schema already declares it.)
func TestRelocate_UnknownStaysRejected(t *testing.T) {
	sh := mustShape(t, editLikeSchema)
	_, _, err := resolveArgs(sh, json.RawMessage(
		`{"file_path":"/x","edits":[{"new_string":"b","bogus":1}]}`), "edit_like")
	if err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("a key declared at no level must stay rejected, got: %v", err)
	}
}

// TestRelocate_NoWrapWhenArrayPresent: a stray top-level edit key is a genuine
// error when the array is already supplied — no guessing.
func TestRelocate_NoWrapWhenArrayPresent(t *testing.T) {
	sh := mustShape(t, editLikeSchema)
	_, _, err := resolveArgs(sh, json.RawMessage(
		`{"file_path":"/x","edits":[{"new_string":"b"}],"old_string":"stray"}`), "edit_like")
	if err == nil || !strings.Contains(err.Error(), "old_string") {
		t.Fatalf("a stray top-level key with edits present must be rejected, got: %v", err)
	}
}

// TestRelocate_NoArrayNoWrap: a tool without an array-of-objects parameter never
// triggers the wrap; an unknown key is still rejected.
func TestRelocate_NoArrayNoWrap(t *testing.T) {
	sh := mustShape(t, `{"type":"object","properties":{"a":{"type":"string"}},"required":["a"],"additionalProperties":false}`)
	_, _, err := resolveArgs(sh, json.RawMessage(`{"a":"x","b":"y"}`), "no_array")
	if err == nil || !strings.Contains(err.Error(), "unknown parameter \"b\"") {
		t.Fatalf("unknown key should still be rejected without an array to wrap into, got: %v", err)
	}
}

// TestRelocate_PublishedSchemaEnablesWrap locks in the cross-file contract: the
// PUBLISHED schema must drop the edits array from required, or a host rejects a
// to-be-wrapped call (no edits) before the daemon can rebuild it.
func TestRelocate_PublishedSchemaEnablesWrap(t *testing.T) {
	var got struct {
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(publishSchema(json.RawMessage(editLikeSchema)), &got); err != nil {
		t.Fatalf("unmarshal published schema: %v", err)
	}
	for _, r := range got.Required {
		if r == "edits" {
			t.Fatalf("published required still includes the wrappable array %q: %v", "edits", got.Required)
		}
	}
}

func hasWarning(warnings []string, sub string) bool {
	for _, w := range warnings {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}
