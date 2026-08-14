package cli

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestFlattenCapabilityKeys(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{name: "nil", raw: "", want: nil},
		{name: "malformed", raw: "not json", want: nil},
		{name: "non-object top level", raw: `["roots"]`, want: nil},
		{
			name: "nested capabilities flatten one level deep",
			raw:  `{"roots":{"listChanged":true},"elicitation":{}}`,
			want: []string{"elicitation", "roots.listChanged"},
		},
		{
			name: "scalar-valued capability stays a bare key",
			raw:  `{"experimental":{"foo":{}},"sampling":{},"version":1}`,
			want: []string{"experimental.foo", "sampling", "version"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := flattenCapabilityKeys(json.RawMessage(c.raw))
			if !slices.Equal(got, c.want) {
				t.Errorf("flattenCapabilityKeys(%s) = %v, want %v", c.raw, got, c.want)
			}
		})
	}
}

// TestOnProtocolNegotiated pins the connSession half of the negotiation
// record: the offered/answered revisions and the client capabilities land on
// the session view, and protocolStatus renders them for daemon_info.
// session.SetProtocol no-ops against the zero connSession's empty session ID,
// so no session file is touched.
func TestOnProtocolNegotiated(t *testing.T) {
	t.Parallel()
	s := &connSession{}
	s.onProtocolNegotiated("2025-11-25", "2024-11-05",
		json.RawMessage(`{"roots":{"listChanged":true},"elicitation":{}}`))
	st := s.protocolStatus()
	if st.Offered != "2025-11-25" || st.Answered != "2024-11-05" {
		t.Errorf("protocolStatus = (%q, %q), want (2025-11-25, 2024-11-05)", st.Offered, st.Answered)
	}
	wantCaps := []string{"elicitation", "roots.listChanged"}
	if !slices.Equal(st.Capabilities, wantCaps) {
		t.Errorf("capabilities = %v, want %v", st.Capabilities, wantCaps)
	}
}
