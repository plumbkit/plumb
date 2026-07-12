package mcp

import (
	"context"
	"encoding/json"
	"testing"
)

func TestPinnedWorkspaceFromParams(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		params string
		want   string
	}{
		{name: "present", params: `{"_meta":{"` + MetaPinnedWorkspaceKey + `":"/Users/dev/proj"}}`, want: "/Users/dev/proj"},
		{name: "empty string", params: `{"_meta":{"` + MetaPinnedWorkspaceKey + `":""}}`, want: ""},
		{name: "no meta", params: `{"clientInfo":{"name":"x"}}`, want: ""},
		{name: "wrong key", params: `{"_meta":{"` + MetaWorkspaceKey + `":"/x"}}`, want: ""},
		{name: "wrong type", params: `{"_meta":{"` + MetaPinnedWorkspaceKey + `":["arr"]}}`, want: ""},
		{name: "malformed", params: `not json`, want: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := pinnedWorkspaceFromParams(json.RawMessage(c.params)); got != c.want {
				t.Fatalf("got %q want %q", got, c.want)
			}
		})
	}
}

// TestHandleInitialize_FiresPinnedWorkspace verifies the authoritative pin hook
// fires from the replayed _meta, and never fires when the key is absent (a first
// connect, or a proxy that predates the key).
func TestHandleInitialize_FiresPinnedWorkspace(t *testing.T) {
	t.Parallel()
	srv := New(ServerInfo{Name: "test", Version: "0"})
	var got string
	srv.OnPinnedWorkspace = func(_ context.Context, dir string) { got = dir }

	params := json.RawMessage(`{"_meta":{"` + MetaPinnedWorkspaceKey + `":"/Users/dev/proj"}}`)
	srv.handleInitialize(context.Background(), mcpRequest{ID: 1, Params: params})
	if got != "/Users/dev/proj" {
		t.Fatalf("OnPinnedWorkspace got %q, want /Users/dev/proj", got)
	}

	got = "unchanged"
	srv.handleInitialize(context.Background(), mcpRequest{ID: 2, Params: json.RawMessage(`{}`)})
	if got != "unchanged" {
		t.Fatalf("OnPinnedWorkspace fired for params with no pin (got %q)", got)
	}
}
