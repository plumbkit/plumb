package mcp

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

// parsedSchema is a minimal view of the bits publishSchema touches.
type parsedSchema struct {
	Type                 string          `json:"type"`
	Properties           json.RawMessage `json:"properties"`
	Required             []string        `json:"required"`
	AdditionalProperties json.RawMessage `json:"additionalProperties"`
	Items                json.RawMessage `json:"items"`
}

func parseSchema(t *testing.T, raw json.RawMessage) parsedSchema {
	t.Helper()
	var p parsedSchema
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("unmarshal published schema: %v\n%s", err, raw)
	}
	return p
}

func TestPublishSchema_DropsAliasTargetFromRequired(t *testing.T) {
	// pattern is an alias target (regex/query -> pattern), path is not required.
	in := json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string"}},"required":["pattern"],"additionalProperties":false}`)
	got := parseSchema(t, publishSchema(in))

	if len(got.Required) != 0 {
		t.Errorf("required = %v, want empty (pattern is an alias target)", got.Required)
	}
	if !bytes.Equal(bytes.TrimSpace(got.AdditionalProperties), []byte("true")) {
		t.Errorf("additionalProperties = %s, want true", got.AdditionalProperties)
	}
}

func TestPublishSchema_KeepsNonAliasRequired(t *testing.T) {
	// content is NOT an alias target, name IS — only name should be dropped.
	in := json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"},"content":{"type":"string"}},"required":["name","content"],"additionalProperties":false}`)
	got := parseSchema(t, publishSchema(in))

	if want := []string{"content"}; !reflect.DeepEqual(got.Required, want) {
		t.Errorf("required = %v, want %v", got.Required, want)
	}
}

func TestPublishSchema_RecursesIntoArrayItems(t *testing.T) {
	// operations[].file_path is an alias target; edits is not. The nested required
	// must be relaxed too, or a host rejects the call before the daemon sees it.
	in := json.RawMessage(`{"type":"object","properties":{"operations":{"type":"array","items":{"type":"object","properties":{"file_path":{"type":"string"},"edits":{"type":"array"}},"required":["file_path","edits"],"additionalProperties":false}}},"required":["operations"]}`)
	got := parseSchema(t, publishSchema(in))

	if want := []string{"operations"}; !reflect.DeepEqual(got.Required, want) {
		t.Errorf("top required = %v, want %v (operations is not an alias target)", got.Required, want)
	}

	var props map[string]json.RawMessage
	if err := json.Unmarshal(got.Properties, &props); err != nil {
		t.Fatalf("unmarshal properties: %v", err)
	}
	opsItems := parseSchema(t, props["operations"])
	items := parseSchema(t, opsItems.Items)
	if want := []string{"edits"}; !reflect.DeepEqual(items.Required, want) {
		t.Errorf("items required = %v, want %v (file_path is an alias target)", items.Required, want)
	}
	if !bytes.Equal(bytes.TrimSpace(items.AdditionalProperties), []byte("true")) {
		t.Errorf("items additionalProperties = %s, want true", items.AdditionalProperties)
	}
}

func TestPublishSchema_FailOpen(t *testing.T) {
	for _, raw := range []string{
		`{"type":"string"}`,       // not an object schema
		`{"type":"object"}`,       // no required, no additionalProperties:false... still relaxed
		`not json at all`,         // unparseable
		`["array","top","level"]`, // not an object
	} {
		out := publishSchema(json.RawMessage(raw))
		// A schema with nothing to relax (string / unparseable / non-object) must be
		// returned byte-for-byte. {"type":"object"} alone gains additionalProperties:true.
		if raw == `{"type":"object"}` {
			got := parseSchema(t, out)
			if !bytes.Equal(bytes.TrimSpace(got.AdditionalProperties), []byte("true")) {
				t.Errorf("bare object: additionalProperties = %s, want true", got.AdditionalProperties)
			}
			continue
		}
		if !bytes.Equal(out, []byte(raw)) {
			t.Errorf("fail-open: input %q mutated to %q", raw, out)
		}
	}
}

func TestPublishSchema_PreservesPropertyOrder(t *testing.T) {
	// zebra/apple/mango is non-alphabetical; the parameter order the model reads
	// must survive even when a nested property is relaxed.
	in := json.RawMessage(`{"type":"object","properties":{"zebra":{"type":"string"},"apple":{"type":"array","items":{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}},"mango":{"type":"string"}},"required":["zebra"]}`)
	got := parseSchema(t, publishSchema(in))

	order, _, err := objectProps(got.Properties)
	if err != nil {
		t.Fatalf("objectProps: %v", err)
	}
	if want := []string{"zebra", "apple", "mango"}; !reflect.DeepEqual(order, want) {
		t.Errorf("property order = %v, want %v", order, want)
	}
}

func TestAliasTargetSet_CoversCanonicals(t *testing.T) {
	targets := aliasTargetSet()
	for _, want := range []string{"pattern", "file_path", "path", "name"} {
		if _, ok := targets[want]; !ok {
			t.Errorf("aliasTargetSet missing %q", want)
		}
	}
}
