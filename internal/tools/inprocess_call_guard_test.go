package tools

import (
	"encoding/json"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// In-process tool→tool call guard.
//
// The MCP parameter-alias engine (internal/mcp/argguard.go) resolves alias
// names to canonical ones only at the dispatch boundary — the path a request
// takes from tools/call into Execute. When one tool in this package instead
// builds JSON args in-process (json.Marshal(map[string]...)) and calls
// another tool's Execute directly, the alias layer never runs: only the
// target's CANONICAL parameter names (the keys under its InputSchema
// "properties") are honoured. A non-canonical key isn't rejected — it's
// silently dropped by the target's json.Unmarshal, leaving the field at its
// zero value. That is exactly how a real 0.7.22 bug shipped: read_multiple_files
// composed {"path": p} for ReadFile.Execute, but ReadFile's canonical key is
// "file_path"; every composed read silently used an empty path instead of p.
//
// Two layers close this off:
//
//  1. TestInProcessCompositionsUseCanonicalKeys — a static check that every
//     key in each reviewed composition's literal args map is a canonical
//     property of its target tool's schema.
//  2. TestInProcessCompositionsDiscoverUnreviewedSites — a scan of this
//     package's non-test source for the composition shape (a
//     json.Marshal(map[string]...) result fed straight into another value's
//     .Execute(ctx, ...)) that fails, with a fix-it pointer, on any file
//     exhibiting that shape which isn't registered in reviewedCompositions
//     below. A future composition site must add itself here — the same
//     "explicit reviewed list" convention schema_contract_test.go uses for
//     nestedSchemaExemptions.
// ---------------------------------------------------------------------------

// compositionSite is one reviewed in-process tool→tool call: the source file
// building the args, and the target tool whose canonical schema those args
// must be checked against.
type compositionSite struct {
	file         string          // filename within this package directory
	description  string          // one-line note on what composes what, for failure messages
	targetName   string          // target tool's Name(), for failure messages
	targetSchema json.RawMessage // target tool's InputSchema()
}

// reviewedCompositions is the explicit, human-reviewed allowlist of files
// known to compose args for an in-process Execute call. Add an entry here —
// naming the target tool and its canonical schema — whenever a new
// composition site is introduced; TestInProcessCompositionsDiscoverUnreviewedSites
// fails with a pointer to this list if one appears unregistered.
func reviewedCompositions() []compositionSite {
	return []compositionSite{
		{
			file:         "read_multiple_files.go",
			description:  "ReadMultipleFiles.Execute composes per-path args for ReadFile.Execute",
			targetName:   (*ReadFile)(nil).Name(),
			targetSchema: (*ReadFile)(nil).InputSchema(),
		},
		{
			file:         "rename_symbol_fallback.go",
			description:  "RenameSymbol.structuralFallback composes args for findReplaceTool.Execute",
			targetName:   (*findReplaceTool)(nil).Name(),
			targetSchema: (*findReplaceTool)(nil).InputSchema(),
		},
	}
}

// schemaProperties returns the canonical top-level property names declared by
// a tool's InputSchema "properties" object.
func schemaProperties(t *testing.T, schema json.RawMessage) map[string]bool {
	t.Helper()
	var raw struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(schema, &raw); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	props := make(map[string]bool, len(raw.Properties))
	for k := range raw.Properties {
		props[k] = true
	}
	return props
}

// mapLiteralKeyRe pulls quoted object keys ("key":) out of a map-literal
// source span.
var mapLiteralKeyRe = regexp.MustCompile(`"(\w+)":`)

// marshalLiteralKeys returns the quoted keys of the map[string]... literal
// passed to the json.Marshal( call starting at marshalIdx in src, found by
// scanning forward to the call's matching close paren by paren-depth (a
// source-text scan rather than a Go parse, deliberately: this test wants to
// see the literal args exactly as an alias-blind Execute call would receive
// them).
func marshalLiteralKeys(src string, marshalIdx int) []string {
	open := strings.Index(src[marshalIdx:], "(")
	if open < 0 {
		return nil
	}
	start := marshalIdx + open
	depth := 0
	end := len(src)
	for i := start; i < len(src); i++ {
		switch src[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				end = i + 1
				i = len(src) // break outer loop
			}
		}
	}
	span := src[start:end]
	matches := mapLiteralKeyRe.FindAllStringSubmatch(span, -1)
	keys := make([]string, 0, len(matches))
	for _, m := range matches {
		keys = append(keys, m[1])
	}
	return keys
}

// TestInProcessCompositionsUseCanonicalKeys is the static half of the guard:
// for every reviewed composition site, every key in its literal args map must
// be a canonical (schema-declared) property of the target tool — proving the
// 0.7.22-shaped bug (a stale/aliased key silently dropped by the target's
// json.Unmarshal, because the dispatch-boundary alias layer never ran) cannot
// recur at these sites.
func TestInProcessCompositionsUseCanonicalKeys(t *testing.T) {
	for _, site := range reviewedCompositions() {
		b, err := os.ReadFile(site.file)
		if err != nil {
			t.Fatalf("%s: %v", site.file, err)
		}
		src := string(b)
		idx := strings.Index(src, "json.Marshal(map[string]")
		if idx < 0 {
			t.Fatalf("%s: expected an in-process json.Marshal(map[string]...) composition for %s, found none — "+
				"update reviewedCompositions (inprocess_call_guard_test.go) if this site was refactored away",
				site.file, site.targetName)
		}
		keys := marshalLiteralKeys(src, idx)
		if len(keys) == 0 {
			t.Fatalf("%s: found json.Marshal(map[string]...) but extracted no keys — check marshalLiteralKeys", site.file)
		}

		canon := schemaProperties(t, site.targetSchema)
		var bad []string
		for _, k := range keys {
			if !canon[k] {
				bad = append(bad, k)
			}
		}
		if len(bad) == 0 {
			continue
		}
		sort.Strings(bad)
		canonList := make([]string, 0, len(canon))
		for k := range canon {
			canonList = append(canonList, k)
		}
		sort.Strings(canonList)
		t.Errorf("%s: composes args for %s (%s) using non-canonical key(s) %v — the MCP parameter-alias layer "+
			"(internal/mcp/argguard.go) runs only at the dispatch boundary, so an in-process Execute call must use "+
			"canonical keys or they are silently dropped by json.Unmarshal (the read_multiple_files/ReadFile 0.7.22 "+
			"bug). Canonical properties of %s: %v", site.file, site.targetName, site.description, bad, site.targetName, canonList)
	}
}

// marshalAssignRe matches a json.Marshal(map[string]...) call assigned to a
// variable, e.g. `raw, _ := json.Marshal(map[string]string{...})` or
// `frArgs, err := json.Marshal(map[string]any{...})`, capturing the variable
// name so it can be correlated with a later Execute(ctx, <var>) call.
var marshalAssignRe = regexp.MustCompile(`(\w+),\s*\w+\s*:=\s*json\.Marshal\(map\[string\]`)

// executeCallRe matches a direct in-process Execute(ctx, <var>) call,
// capturing the argument variable name.
var executeCallRe = regexp.MustCompile(`\.Execute\(ctx,\s*(\w+)\)`)

// reviewedFiles returns the filenames covered by reviewedCompositions.
func reviewedFiles() map[string]bool {
	out := map[string]bool{}
	for _, s := range reviewedCompositions() {
		out[s.file] = true
	}
	return out
}

// TestInProcessCompositionsDiscoverUnreviewedSites is the discovery half of
// the guard: it scans every non-test .go file in this package for the
// in-process composition shape — a json.Marshal(map[string]...)-built value
// fed straight into another value's .Execute(ctx, ...) — and fails, with a
// fix-it pointer to reviewedCompositions, on any file exhibiting that shape
// that isn't already registered there. This is what keeps the guard durable
// as the package grows: a future composition site must register itself and
// pass TestInProcessCompositionsUseCanonicalKeys, rather than silently going
// unchecked. The scan runs on whatever is on disk, so a composition site
// added by a concurrent change is caught on the next run, not retroactively.
//
// Best-effort by design — known blind spots (review catches what the regex
// cannot): a composition that marshals a struct literal instead of a map,
// routes the marshalled args through a helper before Execute, or reassigns
// with = rather than :=, escapes this scan; and the paren-depth key scan in
// the static half reads only the first json.Marshal(map…) per file and does
// not skip string literals, so a stray ')' inside a composed string value
// would truncate key extraction. Keep compositions in the simple
// marshal-map-then-Execute shape so the guard sees them.
func TestInProcessCompositionsDiscoverUnreviewedSites(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	reviewed := reviewedFiles()
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

		marshalled := map[string]bool{}
		for _, m := range marshalAssignRe.FindAllStringSubmatch(src, -1) {
			marshalled[m[1]] = true
		}
		if len(marshalled) == 0 {
			continue
		}

		isComposition := false
		for _, m := range executeCallRe.FindAllStringSubmatch(src, -1) {
			if marshalled[m[1]] {
				isComposition = true
				break
			}
		}
		if !isComposition || reviewed[name] {
			continue
		}
		t.Errorf("%s: found a NEW in-process composition site — a json.Marshal(map[string]...) result fed "+
			"straight into another value's .Execute(ctx, ...). The MCP parameter-alias layer "+
			"(internal/mcp/argguard.go) runs only at the dispatch boundary, so this call must build args with the "+
			"target tool's CANONICAL parameter names or they are silently dropped (the read_multiple_files/ReadFile "+
			"0.7.22 bug). Add an entry to reviewedCompositions() in inprocess_call_guard_test.go naming the target "+
			"tool and its schema, then re-run TestInProcessCompositionsUseCanonicalKeys to verify the keys are "+
			"canonical.", name)
	}
}
