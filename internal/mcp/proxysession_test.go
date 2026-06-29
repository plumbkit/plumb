package mcp

import (
	"encoding/json"
	"testing"
)

func TestProxySessionFromParams(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		params string
		want   string
	}{
		{name: "present", params: `{"_meta":{"` + MetaProxySessionKey + `":"abc123"}}`, want: "abc123"},
		{name: "empty string", params: `{"_meta":{"` + MetaProxySessionKey + `":""}}`, want: ""},
		{name: "no meta", params: `{"clientInfo":{"name":"x"}}`, want: ""},
		{name: "wrong key", params: `{"_meta":{"other":"abc"}}`, want: ""},
		{name: "wrong type", params: `{"_meta":{"` + MetaProxySessionKey + `":["arr"]}}`, want: ""},
		{name: "malformed", params: `not json`, want: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := proxySessionFromParams(json.RawMessage(c.params)); got != c.want {
				t.Fatalf("got %q want %q", got, c.want)
			}
		})
	}
}
