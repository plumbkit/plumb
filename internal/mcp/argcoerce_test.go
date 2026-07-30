package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

// The two coercions added alongside the string→integer/boolean pass: a
// number-typed parameter, and a lone scalar under a scalar-element ARRAY
// parameter. The latter is the canonical-name counterpart of the wrapScalar
// alias transform — before it, diagnostics({uris: "/a.go"}) still died in the
// tool's own json.Unmarshal even though the same call reached by an alias
// (path/file → uris) was wrapped for the caller.

func TestResolveArgs_NumberCoercion(t *testing.T) {
	const schema = `{"type":"object","properties":{"ratio":{"type":"number"},"pattern":{"type":"string"}},"additionalProperties":false}`
	sh, ok := parseShape(json.RawMessage(schema))
	if !ok {
		t.Fatal("parseShape failed")
	}

	out, warnings, err := resolveArgs(sh, json.RawMessage(`{"ratio":"0.5"}`), "tool")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(string(out), `"ratio":0.5`) {
		t.Errorf(`ratio not coerced to a JSON number: %s`, out)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], `coerced "ratio" from string to number`) {
		t.Errorf("want a number-coercion warning, got %v", warnings)
	}

	// An integer-looking string is fine under a number param, but garbage and
	// non-finite values are left for the tool's own decoder.
	for _, bad := range []string{`{"ratio":"abc"}`, `{"ratio":"NaN"}`, `{"ratio":"Inf"}`} {
		out, warnings, err := resolveArgs(sh, json.RawMessage(bad), "tool")
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", bad, err)
		}
		if len(warnings) != 0 {
			t.Errorf("%s: want no coercion, got %v (out %s)", bad, warnings, out)
		}
	}
}

func TestResolveArgs_ScalarWrappedIntoArrayParam(t *testing.T) {
	// uris is an array of SCALARS (wrappable); edits is an array of OBJECTS,
	// where a bare scalar is nonsense rather than missing brackets.
	const schema = `{"type":"object","properties":` +
		`{"uris":{"type":"array","items":{"type":"string"}},` +
		`"edits":{"type":"array","items":{"type":"object","properties":{"old_string":{"type":"string"}}}}},` +
		`"additionalProperties":false}`
	sh, ok := parseShape(json.RawMessage(schema))
	if !ok {
		t.Fatal("parseShape failed")
	}

	out, warnings, err := resolveArgs(sh, json.RawMessage(`{"uris":"/a.go"}`), "diagnostics")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(string(out), `"uris":["/a.go"]`) {
		t.Errorf("scalar not wrapped into the array param: %s", out)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], `wrapped "uris" in a single-element array`) {
		t.Errorf("want a wrap warning, got %v", warnings)
	}

	// Already an array: untouched, no warning, original bytes preserved.
	args := json.RawMessage(`{"uris":["/a.go","/b.go"]}`)
	out, warnings, err = resolveArgs(sh, args, "diagnostics")
	if err != nil || len(warnings) != 0 {
		t.Fatalf("array value must be left alone: warnings=%v err=%v", warnings, err)
	}
	if string(out) != string(args) {
		t.Errorf("original bytes not preserved: %s", out)
	}

	// An array OF OBJECTS is never wrapped from a scalar — relocateMisplaced
	// handles the real misplacement case, and a scalar there is a genuine error.
	out, _, err = resolveArgs(sh, json.RawMessage(`{"edits":"old"}`), "edit_file")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(string(out), `"edits":["old"]`) {
		t.Errorf("a scalar must not be wrapped into an array-of-objects param: %s", out)
	}
}
