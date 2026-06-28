package mcp

import (
	"fmt"
	"sort"
	"strings"
)

// relocateMisplaced repairs a call that put a DECLARED parameter at the wrong
// structural level — the parameter exists in the schema, just nested wrong. It
// runs after alias/typo resolution and BEFORE validation, and only ever touches a
// key that is unknown at its current level (one validation would otherwise
// hard-reject as "unknown parameter"), so it can never alter a call that already
// validates. Unlike the fuzzy corrector this is EXACT — the relocated key is the
// declared parameter name verbatim, just at the wrong level — so there is no
// guessing and the safety-critical gate is not needed. Two directions, both
// warned:
//
//   - hoist: a key inside an array element that is actually a top-level parameter
//     (edit_file's expected_mtime sent inside an edits[] item) moves up.
//   - wrap:  scattered top-level keys that belong to a single, absent array-of-
//     objects parameter (edit_file given old_string/new_string at the top level
//     with no edits array) are wrapped into it.
//
// Returns whether it changed anything.
func relocateMisplaced(sh *shape, obj map[string]any, warnings *[]string) bool {
	changed := hoistFromArrayChildren(sh, obj, warnings)
	if wrapIntoArrayChild(sh, obj, warnings) {
		changed = true
	}
	return changed
}

// hoistFromArrayChildren moves a key found inside an array element that is really
// a top-level parameter up to the top level (first occurrence wins; a later
// duplicate stays and is rejected, which is correct for a nonsensical repeat).
func hoistFromArrayChildren(sh *shape, obj map[string]any, warnings *[]string) bool {
	changed := false
	for k, isArray := range sh.arrays {
		elemShape := sh.children[k]
		list, ok := obj[k].([]any)
		if !isArray || !ok || elemShape == nil {
			continue
		}
		for _, e := range list {
			em, ok := e.(map[string]any)
			if !ok {
				continue
			}
			for key := range em {
				if !hoistable(key, sh, elemShape, obj) {
					continue
				}
				obj[key] = em[key]
				delete(em, key)
				*warnings = append(*warnings, fmt.Sprintf("moved %q out of a %q[] item to the top level", key, k))
				changed = true
			}
		}
	}
	return changed
}

// hoistable reports whether key (found in an array element) is unknown there but
// is an eligible top-level parameter to move up to.
func hoistable(key string, parent, elem *shape, obj map[string]any) bool {
	if _, known := elem.props[key]; known {
		return false
	}
	if _, isParent := parent.props[key]; !isParent {
		return false
	}
	return eligible(key, parent, obj)
}

// wrapIntoArrayChild wraps top-level keys that belong to the tool's single
// array-of-objects parameter into a one-element array, when that parameter is
// absent. Only fires for an unambiguous single array param so the synthesised
// shape is never a guess between two.
func wrapIntoArrayChild(sh *shape, obj map[string]any, warnings *[]string) bool {
	arrayKey, elemShape := singleAbsentArrayChild(sh, obj)
	if arrayKey == "" {
		return false
	}
	moved := map[string]any{}
	for key := range obj {
		if _, known := sh.props[key]; known {
			continue
		}
		if _, inElem := elemShape.props[key]; !inElem {
			continue
		}
		moved[key] = obj[key]
	}
	if len(moved) == 0 {
		return false
	}
	names := make([]string, 0, len(moved))
	for key := range moved {
		names = append(names, key)
		delete(obj, key)
	}
	sort.Strings(names)
	obj[arrayKey] = []any{moved}
	*warnings = append(*warnings, fmt.Sprintf("wrapped top-level %s into %q[]", quoteList(names), arrayKey))
	return true
}

// singleAbsentArrayChild returns the tool's array-of-objects parameter and its
// element shape when there is EXACTLY ONE and it is absent from obj; otherwise
// ("", nil). More than one array param is ambiguous; an already-supplied array
// means a stray top-level key is a genuine error, not a misplacement.
func singleAbsentArrayChild(sh *shape, obj map[string]any) (string, *shape) {
	found := ""
	for k, isArray := range sh.arrays {
		if !isArray {
			continue
		}
		if found != "" {
			return "", nil
		}
		found = k
	}
	if found == "" {
		return "", nil
	}
	if _, present := obj[found]; present {
		return "", nil
	}
	return found, sh.children[found]
}

func quoteList(names []string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = fmt.Sprintf("%q", n)
	}
	return strings.Join(q, ", ")
}
