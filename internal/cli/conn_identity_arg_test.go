package cli

import (
	"context"
	"testing"

	"github.com/plumbkit/plumb/internal/clientcaps"
	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/mcp"
)

func TestClientStripsUndeclaredArgs(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	s := newConnSession(context.Background(), detectTestPool(), nil, config.NewStore(config.Defaults()), nil, nil, newSharedBudgets())
	defer s.close()
	for _, tc := range []struct {
		client string
		want   bool
	}{
		{"", false},
		{"claude-code", false},
		{"codex-mcp-client", false},
		{"local-agent-mode-plumb", true},
		{"local-agent-mode-plumbkit", true},
		{"local-agent-mode", false},
	} {
		s.onClientInfo(tc.client, "1")
		if got := s.clientStripsUndeclaredArgs(); got != tc.want {
			t.Errorf("client %q: %v, want %v", tc.client, got, tc.want)
		}
	}
}

// TestRegisterHooks_DeclaresIdentityArgForTheDesktopConnector drives the real
// registerHooks wiring: deleting the DeclareIdentityArg assignment must fail
// something, since the whole desktop fix hangs on that one line.
func TestRegisterHooks_DeclaresIdentityArgForTheDesktopConnector(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	s := newConnSession(context.Background(), detectTestPool(), nil, config.NewStore(config.Defaults()), nil, nil, newSharedBudgets())
	defer s.close()
	srv := mcp.New(mcp.ServerInfo{Name: "plumb", Version: "0"})
	s.registerHooks(srv)
	if srv.DeclareIdentityArg == nil {
		t.Fatal("registerHooks left DeclareIdentityArg unwired: the desktop connector would drop every identity stamp")
	}
	s.onClientInfo("claude-code", "1")
	if srv.DeclareIdentityArg() {
		t.Error("Claude Code forwards undeclared arguments; its schemas must stay as published")
	}
	s.onClientInfo("local-agent-mode-plumb", "1")
	if !srv.DeclareIdentityArg() {
		t.Error("the desktop connector must get the declared identity key")
	}
}

func TestIsHookClient(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	s := newConnSession(context.Background(), detectTestPool(), nil, config.NewStore(config.Defaults()), nil, nil, newSharedBudgets())
	defer s.close()
	for client, want := range map[string]bool{"claude-code": true, "local-agent-mode-plumb": true, "codex-mcp-client": false, "": false} {
		s.onClientInfo(client, "1")
		if got := s.isHookClient(); got != want {
			t.Errorf("%q: %v, want %v", client, got, want)
		}
		// And through the production accessor session_start reads, so the
		// HookClient field cannot silently drop out of stampChannelState.
		if got := s.stampChannelState(context.Background()).HookClient; got != want {
			t.Errorf("%q: stampChannelState.HookClient = %v, want %v", client, got, want)
		}
	}
}

// TestToolSchemaBytes_ChargesTheDesktopConnectorForTheIdentityProperty
// (#515) drives the real registration and hooks: the per-tool sizes behind
// session_start's surcharge include the declared identity property for Claude
// desktop's connector, which is served it on every tool, and not for Claude
// Code, which is not.
func TestToolSchemaBytes_ChargesTheDesktopConnectorForTheIdentityProperty(t *testing.T) {
	_, plainSrv := buildTestConnSession(t)
	undecorated := plainSrv.ToolSchemaBytes()

	s, srv := buildTestConnSession(t)
	s.registerHooks(srv)

	s.onClientInfo("claude-code", "1")
	for name, got := range srv.ToolSchemaBytes() {
		if got != undecorated[name] {
			t.Errorf("claude-code: %s is %d bytes, want the undecorated %d", name, got, undecorated[name])
		}
	}

	s.onClientInfo("local-agent-mode-plumb", "1")
	desktop := srv.ToolSchemaBytes()
	if len(desktop) != len(undecorated) {
		t.Fatalf("desktop connector: %d tools measured, %d registered", len(desktop), len(undecorated))
	}
	for name, got := range desktop {
		if got <= undecorated[name]+len(mcp.ArgLogicalAgentDeclaredKey) {
			t.Errorf("desktop connector: %s is %d bytes, not charged for the identity property (undecorated %d)", name, got, undecorated[name])
		}
	}
	all := func(string) bool { return true }
	if d, u := clientcaps.ProfileSurcharge(desktop, all).TotalBytes, clientcaps.ProfileSurcharge(undecorated, all).TotalBytes; d <= u {
		t.Errorf("desktop connector surcharge %d bytes is not above the undecorated %d", d, u)
	} else {
		t.Logf("identity property adds %d bytes across %d tools", d-u, len(desktop))
	}
}
