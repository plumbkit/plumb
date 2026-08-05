package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// shape is the parsed argument contract for one object level of a tool's JSON
// Schema: its declared properties (in declaration order), the required set,
// whether undeclared properties are rejected, and the nested shapes of any
// object / array-of-object properties. Built once at registration and never
// mutated, so it is safe to read concurrently.
type shape struct {
	props       map[string]struct{}
	order       []string          // declaration order, for deterministic messages
	types       map[string]string // property → declared JSON Schema type ("integer", "boolean", "array", …)
	required    []string
	rejectExtra bool              // only when the schema sets additionalProperties:false
	children    map[string]*shape // property → nested object shape (arrays use their element shape)
	arrays      map[string]bool   // which children are arrays-of-objects (vs a plain object)
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
		types:       make(map[string]string, len(order)),
		required:    raw.Required,
		rejectExtra: bytes.Equal(bytes.TrimSpace(raw.AdditionalProperties), []byte("false")),
		children:    map[string]*shape{},
		arrays:      map[string]bool{},
	}
	for _, k := range order {
		sh.props[k] = struct{}{}
		sh.types[k] = schemaType(propSchemas[k])
		if child, isArray, ok := childShape(propSchemas[k]); ok {
			sh.children[k] = child
			sh.arrays[k] = isArray
		}
	}
	return sh, true
}

// schemaType extracts the declared "type" of a property schema ("" when absent
// or unparseable). It drives type coercion (coerceTypes) and the wrapScalar
// alias transform, both of which need to know a parameter's scalar kind.
func schemaType(propSchema json.RawMessage) string {
	var raw struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(propSchema, &raw); err != nil {
		return ""
	}
	return raw.Type
}

// childShape returns the object shape to descend into for a property: the
// object's own shape, or for an array its element object shape. isArray reports
// which of the two it was. nil/false when the property is a scalar or an array of
// scalars.
func childShape(propSchema json.RawMessage) (sh *shape, isArray, ok bool) {
	var raw struct {
		Type  string          `json:"type"`
		Items json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(propSchema, &raw); err != nil {
		return nil, false, false
	}
	switch raw.Type {
	case "object":
		s, ok := parseObjectShape(propSchema)
		return s, false, ok
	case "array":
		if len(bytes.TrimSpace(raw.Items)) > 0 {
			s, ok := parseObjectShape(raw.Items)
			return s, true, ok
		}
	}
	return nil, false, false
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
		return nil, nil, errors.New("properties is not an object")
	}
	var order []string
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, nil, err
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, nil, errors.New("non-string property key")
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
func resolveArgs(sh *shape, raw json.RawMessage, toolName string) (json.RawMessage, []string, error) {
	if sh == nil {
		return raw, nil, nil
	}
	obj, err := decodeArgsObject(raw)
	if err != nil {
		return raw, nil, err
	}

	var warnings []string
	// Which canonical names the rewrite SYNTHESISED, so a later rejection can say
	// who supplied what without inventing a key the caller never typed.
	synth := aliasSynth{}
	changed := rewriteObject(sh, obj, "", &warnings, synth)
	// After alias/typo resolution, repair a parameter placed at the wrong level
	// (hoist out of an array element, or wrap scattered top-level keys into an
	// absent array param). Only touches keys validation would otherwise reject.
	if relocateMisplaced(sh, obj, &warnings) {
		changed = true
	}
	// Then coerce string values that plainly mean the declared scalar type
	// ("15" → 15 for an integer parameter, "true" → true for a boolean one) —
	// tools decode with plain json.Unmarshal, which rejects these outright.
	if coerceTypes(sh, obj, "", &warnings) {
		changed = true
	}
	if err := validateObject(sh, obj, "", toolName, synth); err != nil {
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
		return nil, errors.New("arguments must be a JSON object")
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("arguments must be a JSON object")
	}
	return obj, nil
}

// aliasSynth records every canonical parameter name the rewrite SYNTHESISED
// from a supplied alias: keyed by the canonical's full dotted path (the same
// path scheme validation walks), valued by the key the caller actually typed.
// Validation reads it so a collision rejection can never claim the caller
// supplied a name they never wrote — in write_file({body, text}) both keys mean
// "content" and neither of them IS "content".
type aliasSynth map[string]string

// rewriteObject renames recognised alias keys to their canonical names at this
// level and recurses into nested object/array-of-object properties, recording
// each synthesised canonical in synth. Returns true if any key was renamed.
func rewriteObject(sh *shape, obj map[string]any, path string, warnings *[]string, synth aliasSynth) bool {
	changed := false
	type rename struct {
		from, to string
		xf       transform
		fuzzy    bool
	}
	var renames []rename
	targets := map[string]bool{} // canonical names already claimed this level
	claimed := map[string]bool{} // unknown keys already given a rename
	// Both passes walk the unknown keys in sorted order, never Go's randomised
	// map order: with one canonical reachable from two supplied aliases only the
	// first claimant may take it, so the iteration order decides the outcome and
	// must be stable across runs.
	unknown := unknownKeysSorted(sh, obj)
	// Pass 1: curated/exact alias resolution, including value-transform aliases.
	// targets doubles as the claimed-canonical set handed to canonicalFor, so a
	// second alias of an already-taken canonical falls through to validation
	// rather than silently overwriting the first one's value.
	for _, key := range unknown {
		if cand, ok := canonicalFor(key, sh, obj, targets); ok {
			renames = append(renames, rename{from: key, to: cand.name, xf: cand.xf})
			targets[cand.name] = true
			claimed[key] = true
		}
	}
	// Pass 2: high-confidence typo correction for any key no alias claimed, never
	// stealing a target an alias already took.
	for _, key := range unknown {
		if claimed[key] {
			continue
		}
		if canon, ok := fuzzyCanonical(key, sh, obj); ok && !targets[canon] {
			renames = append(renames, rename{from: key, to: canon, fuzzy: true})
			targets[canon] = true
		}
	}
	sort.Slice(renames, func(i, j int) bool { return renames[i].from < renames[j].from })
	for _, r := range renames {
		v := obj[r.from]
		if r.xf != noTransform {
			v = r.xf.apply(sh, r.to, v)
		}
		obj[r.to] = v
		delete(obj, r.from)
		synth[joinPath(path, r.to)] = r.from
		*warnings = append(*warnings, renameWarning(joinPath(path, r.from), r.to, r.fuzzy, r.xf))
		changed = true
	}
	for key, child := range sh.children {
		rewrite := func(s *shape, o map[string]any, p string, w *[]string) bool {
			return rewriteObject(s, o, p, w, synth)
		}
		if v, ok := obj[key]; ok && descend(child, v, joinPath(path, key), warnings, rewrite) {
			changed = true
		}
	}
	return changed
}

// unknownKeysSorted returns obj's keys that are not declared at this level, in
// sorted order, so alias resolution is deterministic when two keys compete for
// one canonical.
func unknownKeysSorted(sh *shape, obj map[string]any) []string {
	out := make([]string, 0, len(obj))
	for key := range obj {
		if _, ok := sh.props[key]; !ok {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

// renameWarning describes one applied key rewrite. A fuzzy (edit-distance)
// correction is flagged as an assumed typo so the caller can see it was a guess,
// not a curated alias; a value transform says what it did to the value.
func renameWarning(from, to string, fuzzy bool, xf transform) string {
	if fuzzy {
		return fmt.Sprintf("corrected likely typo %q to %q", from, to)
	}
	if d := xf.describe(); d != "" {
		return fmt.Sprintf("interpreted %q as %q (%s)", from, to, d)
	}
	return fmt.Sprintf("interpreted %q as %q", from, to)
}

// descend applies fn to a property value: the object itself, or each object
// element of an array. Returns true if fn changed anything.
func descend(child *shape, v any, path string, warnings *[]string, fn func(*shape, map[string]any, string, *[]string) bool) bool {
	switch t := v.(type) {
	case map[string]any:
		return fn(child, t, path, warnings)
	case []any:
		changed := false
		for _, e := range t {
			if m, ok := e.(map[string]any); ok && fn(child, m, path+"[]", warnings) {
				changed = true
			}
		}
		return changed
	}
	return false
}

// coerceTypes rewrites values that plainly mean the declared type: a string
// that parses as a number under an integer- or number-typed parameter ("15" →
// 15), "true"/"false" (any case) under a boolean-typed one, and a lone scalar
// under a scalar-element array parameter (uris: "/a.go" → ["/a.go"] — the same
// courtesy the wrapScalar alias transform gives when the caller reaches the
// parameter by an alias, extended to the canonical name). Nothing else — no
// float-to-int, no trimming — and a string that does not parse is left untouched
// for the tool's own decoder to reject. Tools decode with plain json.Unmarshal,
// which fails the whole call on these; the schema tells us exactly which
// rewrites are safe.
func coerceTypes(sh *shape, obj map[string]any, path string, warnings *[]string) bool {
	changed := false
	for _, key := range sh.order {
		v, present := obj[key]
		if !present {
			continue
		}
		declared := sh.types[key]
		// Array parameters take any scalar; the rest need a string to work from.
		if declared == "array" {
			if !wrappableScalar(sh, key, v) {
				continue
			}
			obj[key] = []any{v}
			*warnings = append(*warnings, fmt.Sprintf("wrapped %q in a single-element array", joinPath(path, key)))
			changed = true
			continue
		}
		s, ok := v.(string)
		if !ok {
			continue
		}
		coerced, kind, ok := coerceScalar(declared, s)
		if !ok {
			continue
		}
		obj[key] = coerced
		*warnings = append(*warnings, fmt.Sprintf("coerced %q from string to %s", joinPath(path, key), kind))
		changed = true
	}
	for key, child := range sh.children {
		if v, ok := obj[key]; ok && descend(child, v, joinPath(path, key), warnings, coerceTypes) {
			changed = true
		}
	}
	return changed
}

// coerceScalar converts a string that plainly means the declared scalar type,
// returning the converted value and the type name for the warning. ok is false
// when the declared type takes no coercion or the string does not parse cleanly
// — the value is then left for the tool's own decoder to reject ("abc" for an
// integer, "1.5" for an integer, Inf/NaN for a number).
func coerceScalar(declared, s string) (value any, kind string, ok bool) {
	switch declared {
	case "integer":
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, "", false
		}
		// Re-encode via FormatInt so non-JSON forms like "015" or "+15" come out
		// as a valid JSON number.
		return json.Number(strconv.FormatInt(n, 10)), "integer", true
	case "number":
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
			return nil, "", false
		}
		// 'g' with -1 precision round-trips the shortest exact form, and emits
		// nothing JSON rejects.
		return json.Number(strconv.FormatFloat(f, 'g', -1, 64)), "number", true
	case "boolean":
		b, ok := boolValue(s)
		if !ok {
			return nil, "", false
		}
		return b, "boolean", true
	}
	return nil, "", false
}

// wrappableScalar reports whether v is a lone scalar that may be wrapped into
// the array-typed parameter key. Arrays are already correct; an array OF OBJECTS
// is excluded because a scalar there is nonsense, not a missing pair of brackets
// (relocateMisplaced handles the real misplacement case for those), and so is a
// nested object.
func wrappableScalar(sh *shape, key string, v any) bool {
	if sh.children[key] != nil { // array of objects
		return false
	}
	switch v.(type) {
	case []any, map[string]any, nil:
		return false
	}
	return true
}

// validateObject checks one object level: no undeclared properties (when this
// level rejects extras), every required property present, then recurses into
// declared object/array children.
func validateObject(sh *shape, obj map[string]any, path, toolName string, synth aliasSynth) error {
	if sh.rejectExtra {
		if unknown := firstUnknown(sh, obj); unknown != "" {
			return unknownErr(sh, obj, joinPath(path, unknown), toolName, synth)
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
		if err := validateChild(child, v, joinPath(path, key), toolName, synth); err != nil {
			return err
		}
	}
	return nil
}

func validateChild(child *shape, v any, path, toolName string, synth aliasSynth) error {
	switch t := v.(type) {
	case map[string]any:
		return validateObject(child, t, path, toolName, synth)
	case []any:
		for _, e := range t {
			if m, ok := e.(map[string]any); ok {
				if err := validateObject(child, m, path+"[]", toolName, synth); err != nil {
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

// unknownErr renders the rejection for an undeclared parameter. obj is the
// object the key was found in, so the message can distinguish the two reasons a
// key reaches here — a name the tool never had, and a name it DOES understand
// but could not apply because the caller also supplied its canonical.
func unknownErr(sh *shape, obj map[string]any, key, toolName string, synth aliasSynth) error {
	prefix := ""
	if toolName != "" {
		prefix = toolName + ": "
	}
	if len(sh.order) == 0 {
		return fmt.Errorf("%sunknown parameter %q: this tool accepts no parameters", prefix, key)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%sunknown parameter %q", prefix, key)
	if collided := collidingCanonical(sh, obj, baseName(key)); collided != "" {
		// The alias resolver refused this key on purpose: its canonical was
		// already taken, and rewriting would have silently dropped one of two
		// values the caller explicitly passed. Without saying so the message is
		// actively misleading — it calls a name the tool understands "unknown",
		// and the "did you mean" hint would point at the parameter already
		// present, reading as nonsense.
		//
		// Which of two wordings is TRUE depends on how the canonical got there.
		// The rewrite runs before validation, so an alias-vs-alias collision
		// reaches this point with a canonical the caller never typed: for
		// write_file({body, text}) obj holds "content" only because "body" was
		// rewritten to it, and telling the caller they "supplied content" — let
		// alone to keep it — names a key that is nowhere in their call.
		parent := parentPath(key)
		if via, synthesised := synth[joinPath(parent, collided)]; synthesised {
			fmt.Fprintf(&b, ": you supplied both %q and %q, which both name %q here — remove one",
				joinPath(parent, via), key, collided)
		} else {
			fmt.Fprintf(&b, ": you supplied both %q and %q, which name the same parameter here — "+
				"remove %q and keep %q", key, collided, key, collided)
		}
	} else if suggestion := closest(baseName(key), sh.order); suggestion != "" {
		fmt.Fprintf(&b, "; did you mean %q?", suggestion)
	}
	// The separator is load-bearing: without it the sentence ran straight into
	// its own parameter list ("unknown parameter \"foo\" valid parameters: …"),
	// which reads as one clause and hides where the message ends. A "did you
	// mean" clause already terminates itself, so it takes a space rather than a
	// second stop.
	msg, sep := b.String(), ". "
	if strings.HasSuffix(msg, "?") {
		sep = " "
	}
	return fmt.Errorf("%s%sValid parameters: %s", msg, sep, strings.Join(sh.order, ", "))
}

// collidingCanonical returns the canonical parameter key is a curated alias of
// when that canonical is already PRESENT at this level — the one case where an
// alias is rejected despite being understood. It returns "" for an ordinary
// unknown key, and only ever names a parameter this level actually declares.
//
// Presence is not the same as "the caller supplied it": obj has already been
// through rewriteObject, so the canonical may have been synthesised from a
// different alias. Only aliasSynth can tell the two apart, which is why the
// wording decision lives in unknownErr and not here.
func collidingCanonical(sh *shape, obj map[string]any, key string) string {
	for _, cand := range paramAliases[normaliseKey(key)] {
		if _, declared := sh.props[cand.name]; !declared {
			continue
		}
		if _, present := obj[cand.name]; present {
			return cand.name
		}
	}
	return ""
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

// parentPath returns everything before the final dotted segment ("" for a
// top-level key) — baseName's counterpart, used to rebuild a sibling key's full
// path at the level a rejection happened on.
func parentPath(path string) string {
	if i := strings.LastIndex(path, "."); i >= 0 {
		return path[:i]
	}
	return ""
}
