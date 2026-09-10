package mcp

import (
	"encoding/json"
	"testing"
)

// TestSplitLogicalAgentArg pins the reserved-argument channel's contract at the
// byte level. A hook that rewrites tool input cannot touch _meta, so the
// identity rides inside `arguments` under ArgLogicalAgentKey; the split must
// take exactly that key and nothing else, and it must not reshape a sibling
// value on the way through — `1.10` re-marshalled through float64 becomes
// `1.1`, and a tool decoding the number as a string would see a different
// argument than the client sent.
func TestSplitLogicalAgentArg(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		wantID   string
		wantRest string // "" means: bytes must be identical to in
	}{
		{name: "absent key leaves bytes untouched", in: `{"name":"x","n":1.10}`},
		{
			name:   "string value is taken and removed",
			in:     `{"name":"x","dev.plumbkit/logical-agent":"C/A","n":1.10,"big":12345678901234567890,"nested":{"k":[1,2.50]}}`,
			wantID: "C/A", wantRest: `{"big":12345678901234567890,"n":1.10,"name":"x","nested":{"k":[1,2.50]}}`,
		},
		{
			name: "non-string value is removed but never becomes an identity",
			in:   `{"name":"x","dev.plumbkit/logical-agent":7}`, wantID: "", wantRest: `{"name":"x"}`,
		},
		{
			name: "empty string is removed and yields no identity",
			in:   `{"name":"x","dev.plumbkit/logical-agent":""}`, wantID: "", wantRest: `{"name":"x"}`,
		},
		{
			name: "nested occurrence is not the channel",
			in:   `{"name":"x","inner":{"dev.plumbkit/logical-agent":"C"}}`,
		},
		{name: "array arguments untouched", in: `[1,2]`},
		{name: "scalar arguments untouched", in: `"x"`},
		{name: "null untouched", in: `null`},
		{name: "empty untouched", in: ``},
		{name: "malformed untouched", in: `{"name":`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, rest := splitLogicalAgentArg(json.RawMessage(tc.in))
			if id != tc.wantID {
				t.Fatalf("id = %q, want %q", id, tc.wantID)
			}
			want := tc.wantRest
			if want == "" {
				want = tc.in
			}
			if string(rest) != want {
				t.Fatalf("rest = %s, want %s", rest, want)
			}
		})
	}
}

// TestResolveLogicalAgent pins the precedence: a per-call _meta identity is the
// stronger channel and always wins over the argument-carried one.
func TestResolveLogicalAgent(t *testing.T) {
	meta := map[string]json.RawMessage{MetaLogicalAgentKey: json.RawMessage(`"meta-agent"`)}
	if got := resolveLogicalAgent(meta, "arg-agent"); got != "meta-agent" {
		t.Fatalf("meta must outrank the argument, got %q", got)
	}
	if got := resolveLogicalAgent(nil, "arg-agent"); got != "arg-agent" {
		t.Fatalf("argument must be used when _meta carries none, got %q", got)
	}
	if got := resolveLogicalAgent(map[string]json.RawMessage{MetaLogicalAgentKey: json.RawMessage(`7`)}, "arg-agent"); got != "arg-agent" {
		t.Fatalf("a malformed _meta value must fall through to the argument, got %q", got)
	}
	if got := resolveLogicalAgent(nil, ""); got != "" {
		t.Fatalf("no channel must yield no identity, got %q", got)
	}
}

// FuzzSplitLogicalAgentArg: the split never panics, an object stays an object,
// and a non-string reserved value never becomes an identity.
func FuzzSplitLogicalAgentArg(f *testing.F) {
	f.Add(`{"a":1}`)
	f.Add(`{"dev.plumbkit/logical-agent":"x","a":[1,{"b":2.50}]}`)
	f.Add(`{"dev.plumbkit/logical-agent":{"deep":true}}`)
	f.Add(`{"dev.plumbkit/logical-agent":null}`)
	f.Add(`[]`)
	f.Add(``)
	f.Fuzz(func(t *testing.T, in string) {
		id, rest := splitLogicalAgentArg(json.RawMessage(in))
		var probe map[string]json.RawMessage
		wasObject := json.Unmarshal([]byte(in), &probe) == nil && probe != nil
		if !wasObject {
			if string(rest) != in || id != "" {
				t.Fatalf("non-object input must pass through untouched: id=%q rest=%s", id, rest)
			}
			return
		}
		var after map[string]json.RawMessage
		if err := json.Unmarshal(rest, &after); err != nil {
			t.Fatalf("object input must stay an object: %v (%s)", err, rest)
		}
		if _, still := after[ArgLogicalAgentKey]; still {
			t.Fatalf("reserved key must never survive the split: %s", rest)
		}
		if raw, had := probe[ArgLogicalAgentKey]; had {
			var s string
			if json.Unmarshal(raw, &s) != nil && id != "" {
				t.Fatalf("non-string reserved value %s became identity %q", raw, id)
			}
		} else if id != "" {
			t.Fatalf("no reserved key but identity %q", id)
		}
	})
}
