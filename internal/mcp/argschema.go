package mcp

import (
	"bytes"
	"encoding/json"
	"sync"
)

// aliasTargetSet is the set of canonical parameter names that some entry in
// paramAliases can stand in for (the union of the table's values). Computed once.
//
// A field that is an alias target may legitimately arrive under a different name,
// so the schema PUBLISHED to clients must not mark it `required`. A host that
// validates required-ness against the advertised schema before transmitting (e.g.
// Claude Code) would otherwise reject an alias-only call — "pattern is Required"
// for search_in_files({query}) — before it ever reaches the daemon, so the
// server-side alias rewrite never runs.
var aliasTargetSet = sync.OnceValue(func() map[string]struct{} {
	out := make(map[string]struct{})
	for _, canons := range paramAliases {
		for _, c := range canons {
			out[c] = struct{}{}
		}
	}
	return out
})

// publishSchema rewrites a tool's JSON Schema into the alias-tolerant form sent to
// clients in tools/list. Top level and recursively through nested object and
// array-of-object schemas (edits[], operations[]) it (1) drops every alias-target
// field from `required` and (2) relaxes `additionalProperties` to true. This only
// widens what a pre-validating host forwards — the daemon keeps validating against
// the ORIGINAL strict schema (parseShape) and rewriting aliases, so the real
// contract (full required set, unknown-parameter "did you mean") is still enforced
// server-side.
//
// Pure and fail-open: a schema that is not a parseable object schema is returned
// byte-for-byte unchanged (mirrors parseShape), so a quirky schema is never
// corrupted.
func publishSchema(schema json.RawMessage) json.RawMessage {
	out, changed := relaxSchema(schema, aliasTargetSet())
	if !changed {
		return schema
	}
	return out
}

// relaxSchema relaxes one schema node. An object schema is relaxed directly; an
// array schema is relaxed through its `items`. Anything else (scalars, arrays of
// scalars) is returned unchanged.
func relaxSchema(schema json.RawMessage, targets map[string]struct{}) (json.RawMessage, bool) {
	var head struct {
		Type  string          `json:"type"`
		Items json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(schema, &head); err != nil {
		return schema, false
	}
	switch head.Type {
	case "object":
		return relaxObject(schema, targets)
	case "array":
		if len(bytes.TrimSpace(head.Items)) == 0 {
			return schema, false
		}
		newItems, changed := relaxSchema(head.Items, targets)
		if !changed {
			return schema, false
		}
		return replaceField(schema, "items", newItems)
	}
	return schema, false
}

// relaxObject drops alias-target fields from `required`, forces
// `additionalProperties:true`, and recurses into declared properties. The schema
// object's own top-level key order is not preserved (it is cosmetic JSON), but the
// declaration order of parameters inside `properties` — which the model reads — is.
func relaxObject(schema json.RawMessage, targets map[string]struct{}) (json.RawMessage, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(schema, &fields); err != nil {
		return schema, false
	}
	if !bytes.Equal(bytes.TrimSpace(fields["type"]), []byte(`"object"`)) {
		return schema, false
	}

	changed := dropPublishedRequired(fields, requiredDropSet(targets, fields["properties"]))

	if raw, ok := fields["additionalProperties"]; !ok || !bytes.Equal(bytes.TrimSpace(raw), []byte("true")) {
		fields["additionalProperties"] = json.RawMessage("true")
		changed = true
	}

	if props, ok := fields["properties"]; ok {
		if relaxed, ch := relaxProperties(props, targets); ch {
			fields["properties"] = relaxed
			changed = true
		}
	}

	if !changed {
		return schema, false
	}
	out, err := json.Marshal(fields)
	if err != nil {
		return schema, false
	}
	return out, true
}

// requiredDropSet is the set of property names to drop from the PUBLISHED
// `required` list: alias targets (the field may arrive under a different name)
// PLUS array-of-object parameters. The daemon can synthesise the latter from
// misplaced top-level keys (the wrap recovery in argrelocate.go), so a client
// must not pre-reject a call for a "missing" edits/operations array before the
// daemon rebuilds it. The server-side strict schema still requires every
// original field, so the real contract is unchanged.
func requiredDropSet(targets map[string]struct{}, props json.RawMessage) map[string]struct{} {
	out := make(map[string]struct{}, len(targets))
	for k := range targets {
		out[k] = struct{}{}
	}
	for name := range arrayObjectPropNames(props) {
		out[name] = struct{}{}
	}
	return out
}

// arrayObjectPropNames returns the names of properties that are arrays whose
// items are objects (the shape the wrap recovery can synthesise).
func arrayObjectPropNames(props json.RawMessage) map[string]struct{} {
	out := map[string]struct{}{}
	order, schemas, err := objectProps(props)
	if err != nil {
		return out
	}
	for _, k := range order {
		var raw struct {
			Type  string          `json:"type"`
			Items json.RawMessage `json:"items"`
		}
		if json.Unmarshal(schemas[k], &raw) != nil || raw.Type != "array" || len(bytes.TrimSpace(raw.Items)) == 0 {
			continue
		}
		var it struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw.Items, &it) == nil && it.Type == "object" {
			out[k] = struct{}{}
		}
	}
	return out
}

// dropPublishedRequired removes the named properties from the `required` array,
// deleting the key entirely when nothing remains. Returns whether it changed
// anything.
func dropPublishedRequired(fields map[string]json.RawMessage, drop map[string]struct{}) bool {
	raw, ok := fields["required"]
	if !ok {
		return false
	}
	var req []string
	if err := json.Unmarshal(raw, &req); err != nil {
		return false
	}
	kept := make([]string, 0, len(req))
	for _, r := range req {
		if _, dropped := drop[r]; !dropped {
			kept = append(kept, r)
		}
	}
	if len(kept) == len(req) {
		return false
	}
	if len(kept) == 0 {
		delete(fields, "required")
		return true
	}
	if b, err := json.Marshal(kept); err == nil {
		fields["required"] = b
	}
	return true
}

// relaxProperties relaxes each nested object/array-of-object property, preserving
// the declaration order of the properties (which the model reads top-to-bottom).
func relaxProperties(props json.RawMessage, targets map[string]struct{}) (json.RawMessage, bool) {
	order, schemas, err := objectProps(props)
	if err != nil {
		return props, false
	}
	changed := false
	for _, k := range order {
		if relaxed, ch := relaxSchema(schemas[k], targets); ch {
			schemas[k] = relaxed
			changed = true
		}
	}
	if !changed {
		return props, false
	}
	return marshalOrdered(order, schemas), true
}

// replaceField re-emits an object schema with one top-level field replaced,
// preserving sibling keys (key order is cosmetic and not preserved).
func replaceField(schema json.RawMessage, key string, value json.RawMessage) (json.RawMessage, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(schema, &fields); err != nil {
		return schema, false
	}
	fields[key] = value
	out, err := json.Marshal(fields)
	if err != nil {
		return schema, false
	}
	return out, true
}

// marshalOrdered builds a JSON object from keys in the given order with raw
// values, so property declaration order survives the round-trip.
func marshalOrdered(order []string, values map[string]json.RawMessage) json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range order {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		b.Write(kb)
		b.WriteByte(':')
		b.Write(values[k])
	}
	b.WriteByte('}')
	return b.Bytes()
}
