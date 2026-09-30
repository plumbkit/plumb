package cli

import (
	"context"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
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
