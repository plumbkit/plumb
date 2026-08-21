package mcp

import (
	"fmt"
	"strings"
)

// unknownDetail renders the clause following `unknown parameter %q` in
// unknownErr's message (argguard.go): the alias-collision explanation when key
// names a curated alias whose canonical is already present at this level,
// otherwise a "did you mean" fuzzy-typo suggestion, otherwise (PLAN-358) a
// nested-placement hint when key is a real parameter of one of this level's
// array-of-objects children, or "" when none of those apply.
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
	if suggestion := closest(baseName(key), sh.order); suggestion != "" {
		return fmt.Sprintf("; did you mean %q?", suggestion)
	}
	return placementHint(sh, baseName(key))
}

// placementHint names the correct nesting for key when it is not a parameter at
// this level but IS a parameter of one of this level's array-of-objects children
// (e.g. edit_file's top-level "replace_all" belongs inside each edits[] item).
// Returns "" when key matches no child at all, so an ordinary unknown parameter
// is unaffected. Checked only after the alias-collision and "did you mean" cases
// have already had a chance to fire — a real typo of a top-level name should
// still get the closer, cheaper correction.
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

// minimalNestedExample renders a minimal valid JSON shape for the array-of-
// objects parameter arrKey: one element carrying its required fields plus key
// (key is included even when not required, since it is the field the caller was
// trying to set). Field values are placeholders ("...") — this is a shape
// example, not a literal call to copy verbatim.
func minimalNestedExample(arrKey string, child *shape, key string) string {
	fields := append([]string{}, child.required...)
	found := false
	for _, f := range fields {
		if f == key {
			found = true
			break
		}
	}
	if !found {
		fields = append(fields, key)
	}
	parts := make([]string, 0, len(fields))
	for _, f := range fields {
		parts = append(parts, fmt.Sprintf("%q: ...", f))
	}
	return fmt.Sprintf(`{%q: [{%s}]}`, arrKey, strings.Join(parts, ", "))
}
