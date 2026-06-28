package mcp

import (
	"encoding/json"
	"testing"
)

func TestAllowDirsFromParams(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		params string
		want   []string
	}{
		{
			name:   "present",
			params: `{"_meta":{"` + MetaAllowDirsKey + `":["/a","/b"]}}`,
			want:   []string{"/a", "/b"},
		},
		{
			name:   "blanks dropped",
			params: `{"_meta":{"` + MetaAllowDirsKey + `":["/a","","  /b not-trimmed"]}}`,
			want:   []string{"/a", "  /b not-trimmed"}, // only empty strings are dropped, not whitespace
		},
		{name: "no meta", params: `{"clientInfo":{"name":"x"}}`, want: nil},
		{name: "wrong key", params: `{"_meta":{"other":["/a"]}}`, want: nil},
		{name: "wrong type", params: `{"_meta":{"` + MetaAllowDirsKey + `":"notarray"}}`, want: nil},
		{name: "malformed", params: `not json`, want: nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := allowDirsFromParams(json.RawMessage(c.params))
			if len(got) != len(c.want) {
				t.Fatalf("got %v want %v", got, c.want)
			}
			for i := range c.want {
				if got[i] != c.want[i] {
					t.Fatalf("entry %d: got %q want %q", i, got[i], c.want[i])
				}
			}
		})
	}
}
