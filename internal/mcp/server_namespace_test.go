package mcp

import (
	"context"
	"errors"
	"testing"
)

const nsInitRaw = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"_meta":{"dev.plumbkit/namespace-token":"pns1-abc","dev.plumbkit/proxy-session-id":"psid-9"}}}`

// TestNamespaceGate_RefusesTheConnection: a refused token answers initialize
// with CodeNamespaceRefused, fires no other initialize hook, and every later
// request on the connection gets the same refusal.
func TestNamespaceGate_RefusesTheConnection(t *testing.T) {
	s := New(ServerInfo{Name: "t", Version: "0"})
	var gotToken, gotProxy string
	s.OnNamespace = func(_ context.Context, token, proxy string) error {
		gotToken, gotProxy = token, proxy
		return errors.New("namespace revoked")
	}
	proxyHook := false
	s.OnProxySession = func(context.Context, string) { proxyHook = true }

	resp, _ := s.handle(context.Background(), []byte(nsInitRaw))
	if resp.Error == nil || resp.Error.Code != CodeNamespaceRefused || resp.Error.Message != "namespace revoked" {
		t.Fatalf("initialize = %+v, want the namespace refusal", resp.Error)
	}
	if gotToken != "pns1-abc" || gotProxy != "psid-9" {
		t.Fatalf("gate saw token %q proxy %q", gotToken, gotProxy)
	}
	if proxyHook {
		t.Fatal("OnProxySession ran on a refused connection")
	}
	later, _ := s.handle(context.Background(), []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`))
	if later.Error == nil || later.Error.Code != CodeNamespaceRefused {
		t.Fatalf("a request after the refusal = %+v, want the refusal again", later.Error)
	}
}

// TestNamespaceGate_AdmitsAndRunsFirst: an admitted connection proceeds, with
// the gate running before OnProxySession; a client sending no token is still
// asked (with ""), so a namespaced proxy session cannot shed its token.
func TestNamespaceGate_AdmitsAndRunsFirst(t *testing.T) {
	s := New(ServerInfo{Name: "t", Version: "0"})
	var order []string
	s.OnNamespace = func(_ context.Context, token, _ string) error {
		order = append(order, "gate:"+token)
		return nil
	}
	s.OnProxySession = func(context.Context, string) { order = append(order, "proxy") }
	resp, _ := s.handle(context.Background(), []byte(nsInitRaw))
	if resp.Error != nil {
		t.Fatalf("initialize refused: %+v", resp.Error)
	}
	if len(order) != 2 || order[0] != "gate:pns1-abc" || order[1] != "proxy" {
		t.Fatalf("hook order = %v, want the gate first", order)
	}
	order = nil
	plain := `{"jsonrpc":"2.0","id":3,"method":"initialize","params":{"_meta":{"dev.plumbkit/proxy-session-id":"psid-1"}}}`
	if resp, _ := s.handle(context.Background(), []byte(plain)); resp.Error != nil {
		t.Fatalf("plain initialize refused: %+v", resp.Error)
	}
	if len(order) == 0 || order[0] != "gate:" {
		t.Fatalf("hook order = %v, want the gate asked with an empty token", order)
	}
}
