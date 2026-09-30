package cli

import (
	"context"
	"testing"

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
	}
}
