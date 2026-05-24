package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// shape is the parsed argument contract for one object level of a tool's JSON
// Schema: its declared properties (in declaration order), the required set,
// whether undeclared properties are rejected, and the nested shapes of any
// object / array-of-object properties. Built once at registration and never
// mutated, so it is safe to read concurrently.
type shape struct {
	props       map[string]struct{}
	order       []string // declaration order, for deterministic messages
	required    []string
	rejectExtra bool              // only when the schema sets additionalProperties:false
	children    map[string]*shape // property → nested object shape (arrays use their element shape)
}

// parseShape builds the top-level shape for a tool schema. It returns ok=false
// (fail-open — the tool is left unguarded) when the schema is not a parseable
// object schema, so a quirky schema can never block its tool.
func parseShape(schema json.RawMessage) (*shape, bool) {
	return parseObjectShape(schema)
}

func parseObjectShape(schema json.RawMessage) (*shape, bool) {
	var raw struct {
		Type                 string          `json:"type"`
		Properties           json.RawMessage `json:"properties"`
		Required             []string        `json:"required"`
		AdditionalProperties json.RawMessage `json:"additionalProperties"`
	}
	if err := json.Unmarshal(schema, &raw); err != nil || raw.Type != "object" {
		return nil, false
	}
	order, propSchemas, err := objectProps(raw.Properties)
	if err != nil {
		return nil, false
	}
	sh := &shape{
		props:       make(map[string]struct{}, len(order)),
		order:       order,
		required:    raw.Required,
		rejectExtra: bytes.Equal(bytes.TrimSpace(raw.AdditionalProperties), []byte("false")),
		children:    map[string]*shape{},
	}
	for _, k := range order {
		sh.props[k] = struct{}{}
		if child, ok := childShape(propSchemas[k]); ok {
			sh.children[k] = child
		}
	}
	return sh, true
}

// childShape returns the object shape to descend into for a property: the
// object's own shape, or for an array its element object shape. nil/false when
// the property is a scalar or an array of scalars.
func childShape(propSchema json.RawMessage) (*shape, bool) {
	var raw struct {
		Type  string          `json:"type"`
		Items json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(propSchema, &raw); err != nil {
		return nil, false
	}
	switch raw.Type {
	case "object":
		return parseObjectShape(propSchema)
	case "array":
		if len(bytes.TrimSpace(raw.Items)) > 0 {
			return parseObjectShape(raw.Items)
		}
	}
	return nil, false
}

// objectProps returns a JSON object's keys in declaration order plus each key's
// raw sub-schema. An empty/absent object yields empty results.
func objectProps(obj json.RawMessage) ([]string, map[string]json.RawMessage, error) {
	out := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(obj)) == 0 {
		return nil, out, nil
	}
	dec := json.NewDecoder(bytes.NewReader(obj))
	tok, err := dec.Token()
	if err != nil {
		return nil, nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, nil, fmt.Errorf("properties is not an object")
	}
	var order []string
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, nil, err
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, nil, fmt.Errorf("non-string property key")
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, nil, err
		}
		order = append(order, key)
		out[key] = v
	}
	return order, out, nil
}

// resolveArgs rewrites recognised parameter aliases to their canonical names
// (recursively, schema-guided) and then validates the rewritten arguments
// against the contract. It returns the rewritten raw arguments (the original
// bytes when nothing was aliased), a human-readable warning per applied alias,
// and a validation error. Deeper value validation (types, lengths, patterns)
// is intentionally left to each tool.
func resolveArgs(sh *shape, raw json.RawMessage) (json.RawMessage, []string, error) {
	if sh == nil {
		return raw, nil, nil
	}
	obj, err := decodeArgsObject(raw)
	if err != nil {
		return raw, nil, err
	}

	var warnings []string
	changed := rewriteObject(sh, obj, "", &warnings)
	if err := validateObject(sh, obj, ""); err != nil {
		return raw, nil, err
	}
	if !changed {
		return raw, nil, nil // common path: preserve original bytes exactly
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return raw, nil, err
	}
	return out, warnings, nil
}

// decodeArgsObject parses raw arguments into a top-level map, preserving numeric
// fidelity (UseNumber) so re-marshalling after an alias rewrite never reshapes
// untouched values. Absent/empty/null arguments decode to an empty map.
func decodeArgsObject(raw json.RawMessage) (map[string]any, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return map[string]any{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("arguments must be a JSON object")
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("arguments must be a JSON object")
	}
	return obj, nil
}

// rewriteObject renames recognised alias keys to their canonical names at this
// level and recurses into nested object/array-of-object properties. Returns
// true if any key was renamed.
func rewriteObject(sh *shape, obj map[string]any, path string, warnings *[]string) bool {
	changed := false
	type rename struct{ from, to string }
	var renames []rename
	for key := range obj {
		if _, ok := sh.props[key]; ok {
			continue
		}
		if canon, ok := canonicalFor(key, sh, obj); ok {
			renames = append(renames, rename{from: key, to: canon})
		}
	}
	sort.Slice(renames, func(i, j int) bool { return renames[i].from < renames[j].from })
	for _, r := range renames {
		obj[r.to] = obj[r.from]
		delete(obj, r.from)
		*warnings = append(*warnings, fmt.Sprintf("interpreted %q as %q", joinPath(path, r.from), r.to))
		changed = true
	}
	for key, child := range sh.children {
		if v, ok := obj[key]; ok && descend(child, v, joinPath(path, key), warnings) {
			changed = true
		}
	}
	return changed
}

// descend applies child to a property value: an object, or each object element
// of an array. Returns true if any nested key was renamed.
func descend(child *shape, v any, path string, warnings *[]string) bool {
	switch t := v.(type) {
	case map[string]any:
		return rewriteObject(child, t, path, warnings)
	case []any:
		changed := false
		for _, e := range t {
			if m, ok := e.(map[string]any); ok && rewriteObject(child, m, path+"[]", warnings) {
				changed = true
			}
		}
		return changed
	}
	return false
}

// validateObject checks one object level: no undeclared properties (when this
// level rejects extras), every required property present, then recurses into
// declared object/array children.
func validateObject(sh *shape, obj map[string]any, path string) error {
	if sh.rejectExtra {
		if unknown := firstUnknown(sh, obj); unknown != "" {
			return unknownErr(sh, joinPath(path, unknown))
		}
	}
	for _, req := range sh.required {
		if _, ok := obj[req]; !ok {
			return fmt.Errorf("missing required parameter %q (required: %s)", joinPath(path, req), strings.Join(sh.required, ", "))
		}
	}
	for key, child := range sh.children {
		v, ok := obj[key]
		if !ok {
			continue
		}
		if err := validateChild(child, v, joinPath(path, key)); err != nil {
			return err
		}
	}
	return nil
}

func validateChild(child *shape, v any, path string) error {
	switch t := v.(type) {
	case map[string]any:
		return validateObject(child, t, path)
	case []any:
		for _, e := range t {
			if m, ok := e.(map[string]any); ok {
				if err := validateObject(child, m, path+"[]"); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// firstUnknown returns the alphabetically-first key not declared at this level,
// or "" when every key is known. Sorting makes the choice deterministic despite
// Go's randomised map iteration.
func firstUnknown(sh *shape, obj map[string]any) string {
	var unknown []string
	for k := range obj {
		if _, ok := sh.props[k]; !ok {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) == 0 {
		return ""
	}
	sort.Strings(unknown)
	return unknown[0]
}

func unknownErr(sh *shape, key string) error {
	if len(sh.order) == 0 {
		return fmt.Errorf("unknown parameter %q: this tool accepts no parameters", key)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "unknown parameter %q", key)
	if suggestion := closest(baseName(key), sh.order); suggestion != "" {
		fmt.Fprintf(&b, "; did you mean %q?", suggestion)
	}
	fmt.Fprintf(&b, " valid parameters: %s", strings.Join(sh.order, ", "))
	return errors.New(b.String())
}

// joinPath builds a readable dotted path for nested keys (e.g. edits[].old_str).
func joinPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// baseName returns the final segment of a dotted path, for typo suggestions.
func baseName(path string) string {
	if i := strings.LastIndex(path, "."); i >= 0 {
		return path[i+1:]
	}
	return path
}
