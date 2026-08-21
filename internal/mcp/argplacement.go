package mcp

import (
	"fmt"
	"strings"
)

// unknownDetail renders the clause following `unknown parameter %q` in
// unknownErr's message (argguard.go): the alias-collision explanation when key
// names a curated alias whose canonical is already present at this level,
// otherwise (PLAN-358) a nested-placement hint when key is an EXACT parameter
// name of one of this level's array-of-objects children, otherwise a "did you
// mean" fuzzy-typo suggestion against this level's own names, or "" when none
// of those apply.
//
// The placement hint is checked before the fuzzy suggestion, not after: an
// exact nested match is a stronger, cheaper-to-trust signal than a fuzzy
// top-level guess, and the two can disagree. edit_file's top-level
// "old_string" is a real parameter one level down (inside each edits[] item)
// but also a Levenshtein-close typo of the top-level "new_string" (anchor
// mode) — checking the fuzzy suggestion first used to win, so the rejection
// said 'did you mean "new_string"?' for a caller who had the right name, just
// the wrong nesting.
func unknownDetail(sh *shape, obj map[string]any, key string, synth aliasSynth) string {
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
			return fmt.Sprintf(": you supplied both %q and %q, which both name %q here — remove one",
				joinPath(parent, via), key, collided)
		}
		return fmt.Sprintf(": you supplied both %q and %q, which name the same parameter here — "+
			"remove %q and keep %q", key, collided, key, collided)
	}
	if hint := placementHint(sh, baseName(key)); hint != "" {
		return hint
	}
	if suggestion := closest(baseName(key), sh.order); suggestion != "" {
		return fmt.Sprintf("; did you mean %q?", suggestion)
	}
	return ""
}

// placementHint names the correct nesting for key when it is not a parameter at
// this level but IS a parameter of one of this level's array-of-objects children
// (e.g. edit_file's top-level "replace_all" belongs inside each edits[] item).
// Returns "" when key matches no child at all, so an ordinary unknown parameter
// is unaffected and falls through to the fuzzy "did you mean" suggestion.
func placementHint(sh *shape, key string) string {
	for _, arrKey := range sh.order {
		if !sh.arrays[arrKey] {
			continue
		}
		child := sh.children[arrKey]
		if child == nil {
			continue
		}
		if _, ok := child.props[key]; !ok {
			continue
		}
		return fmt.Sprintf(": %q belongs inside each %s[] item, not at the top level. Example: %s",
			key, arrKey, minimalNestedExample(arrKey, child, key))
	}
	return ""
}

// minimalNestedExample renders a minimal VALID JSON shape for the array-of-
// objects parameter arrKey — valid meaning a shape the tool's own logic
// accepts, not merely one the JSON Schema does not reject. Field values are
// placeholders ("...") — this is a shape example, not a literal call to copy
// verbatim. See nestedExampleFields for what "valid" pulls in beyond
// child.required.
func minimalNestedExample(arrKey string, child *shape, key string) string {
	fields := nestedExampleFields(child, key)
	parts := make([]string, 0, len(fields))
	for _, f := range fields {
		parts = append(parts, fmt.Sprintf("%q: ...", f))
	}
	return fmt.Sprintf(`{%q: [{%s}]}`, arrKey, strings.Join(parts, ", "))
}

// nestedExampleFields returns the field names for a minimal VALID element of
// the array-of-objects parameter: the schema's required fields, key itself,
// and — when the child declares the old_string/new_string str_replace pair —
// both of them together, old_string first.
//
// old_string is schema-optional (edit_file's range mode omits it in favour of
// start_line/end_line), so child.required alone can under-name a valid
// str_replace item: {"new_string": ..., "replace_all": ...} passes the JSON
// Schema guard (old_string is not in "required") but is a shape edit_file's
// own tool logic rejects outright ("old_string must not be empty" when
// start_line is also unset). Pairing the two whenever both are declared keeps
// the example a shape the tool actually accepts.
func nestedExampleFields(child *shape, key string) []string {
	var fields []string
	if _, hasOld := child.props["old_string"]; hasOld {
		if _, hasNew := child.props["new_string"]; hasNew {
			fields = append(fields, "old_string", "new_string")
		}
	}
	for _, f := range child.required {
		fields = ensureField(fields, f)
	}
	return ensureField(fields, key)
}

// ensureField appends f to fields unless it is already present.
func ensureField(fields []string, f string) []string {
	for _, x := range fields {
		if x == f {
			return fields
		}
	}
	return append(fields, f)
}
