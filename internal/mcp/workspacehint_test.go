package mcp

import (
	"context"
	"encoding/json"
	"testing"
)

func TestWorkspaceHintFromParams(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		params string
		want   string
	}{
		{name: "present", params: `{"_meta":{"` + MetaWorkspaceKey + `":"/Users/dev/proj"}}`, want: "/Users/dev/proj"},
		{name: "empty string", params: `{"_meta":{"` + MetaWorkspaceKey + `":""}}`, want: ""},
		{name: "no meta", params: `{"clientInfo":{"name":"x"}}`, want: ""},
		{name: "wrong key", params: `{"_meta":{"other":"/x"}}`, want: ""},
		{name: "wrong type", params: `{"_meta":{"` + MetaWorkspaceKey + `":["arr"]}}`, want: ""},
		{name: "malformed", params: `not json`, want: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := workspaceHintFromParams(json.RawMessage(c.params)); got != c.want {
				t.Fatalf("got %q want %q", got, c.want)
			}
		})
	}
}

// TestHandleInitialize_FiresWorkspaceHint verifies the hook receives the
// transported hint during the initialize exchange, and that an absent hint
// never fires it.
func TestHandleInitialize_FiresWorkspaceHint(t *testing.T) {
	t.Parallel()
	srv := New(ServerInfo{Name: "test", Version: "0"})
	var got string
	srv.OnWorkspaceHint = func(_ context.Context, dir string) { got = dir }

	params := json.RawMessage(`{"_meta":{"` + MetaWorkspaceKey + `":"/Users/dev/proj"}}`)
	srv.handleInitialize(context.Background(), mcpRequest{ID: 1, Params: params})
	if got != "/Users/dev/proj" {
		t.Fatalf("OnWorkspaceHint got %q, want /Users/dev/proj", got)
	}

	got = "unchanged"
	srv.handleInitialize(context.Background(), mcpRequest{ID: 2, Params: json.RawMessage(`{}`)})
	if got != "unchanged" {
		t.Fatalf("OnWorkspaceHint fired for params with no hint (got %q)", got)
	}
}
