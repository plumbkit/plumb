package mcp

import (
	"encoding/json"
	"testing"
)

func TestWithIdentityProperty(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{
			"adds beside existing properties",
			`{"type":"object","properties":{"a":{"type":"string"}},"additionalProperties":false}`,
			`{"additionalProperties":false,"properties":{"a":{"type":"string"},"plumb_agent":` + identityPropertySchema + `},"type":"object"}`,
		},
		{
			"creates properties", `{"type":"object"}`,
			`{"properties":{"plumb_agent":` + identityPropertySchema + `},"type":"object"}`,
		},
		{
			"already declared is untouched", `{"type":"object","properties":{"plumb_agent":{"type":"string"}}}`,
			`{"type":"object","properties":{"plumb_agent":{"type":"string"}}}`,
		},
		{
			"keeps the published property order",
			`{"type":"object","properties":{"zebra":{},"apple":{},"mango":{}}}`,
			`{"properties":{"zebra":{},"apple":{},"mango":{},"plumb_agent":` + identityPropertySchema + `},"type":"object"}`,
		},
		{"not an object", `[1]`, `[1]`},
		{"non-object schema", `{"type":"string"}`, `{"type":"string"}`},
		{"malformed", `{"type":`, `{"type":`},
		{"properties not an object", `{"properties":[1]}`, `{"properties":[1]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(withIdentityProperty(json.RawMessage(tc.in))); got != tc.want {
				t.Fatalf("got  %s\nwant %s", got, tc.want)
			}
		})
	}
}

// TestIdentityPropertyNameIsDeclarable: the Anthropic API rejects a tool whose
// property names fall outside ^[a-zA-Z0-9_.-]{1,64}$, which would break the
// whole tool list on the very client this key exists for.
func TestIdentityPropertyNameIsDeclarable(t *testing.T) {
	k := ArgLogicalAgentDeclaredKey
	if len(k) == 0 || len(k) > 64 {
		t.Fatalf("%q: length %d", k, len(k))
	}
	for _, r := range k {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '.' && r != '-' {
			t.Fatalf("%q contains %q, which a tool property name may not", k, r)
		}
	}
	if k == ArgLogicalAgentKey {
		t.Fatal("the declarable key must differ from the reverse-DNS key")
	}
}

func TestSplitLogicalAgentArg_DeclaredKey(t *testing.T) {
	for _, tc := range []struct{ name, in, wantID, wantArgs string }{
		{"declared only", `{"a":1,"plumb_agent":"d"}`, "d", `{"a":1}`},
		{"reverse-DNS wins", `{"a":1,"dev.plumbkit/logical-agent":"r","plumb_agent":"d"}`, "r", `{"a":1}`},
		{"empty reverse-DNS falls back", `{"dev.plumbkit/logical-agent":"","plumb_agent":"d"}`, "d", `{}`},
		{"null declared is stripped, no identity", `{"a":1,"plumb_agent":null}`, "", `{"a":1}`},
		{"non-string declared is stripped, no identity", `{"plumb_agent":7}`, "", `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, args := splitLogicalAgentArg(json.RawMessage(tc.in))
			if id != tc.wantID || string(args) != tc.wantArgs {
				t.Fatalf("got (%q, %s), want (%q, %s)", id, args, tc.wantID, tc.wantArgs)
			}
		})
	}
}
