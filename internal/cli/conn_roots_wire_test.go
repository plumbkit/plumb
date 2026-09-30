package cli

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/sessionstate"
)

// rootsAnswer is a RequestFn that answers roots/list with the given folders, the
// way a client does after it sends notifications/roots/list_changed.
func rootsAnswer(t *testing.T, folders ...string) func(context.Context, string, any) (json.RawMessage, error) {
	t.Helper()
	type root struct {
		URI string `json:"uri"`
	}
	var resp struct {
		Roots []root `json:"roots"`
	}
	for _, f := range folders {
		resp.Roots = append(resp.Roots, root{URI: "file://" + f})
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	return func(_ context.Context, method string, _ any) (json.RawMessage, error) {
		if method != "roots/list" {
			t.Errorf("unexpected client request %q", method)
		}
		return raw, nil
	}
}

// TestHandleRootsListChanged_FollowsClientFolder exercises the handler the wire
// dispatcher now reaches (it was unreachable before the method-name fix): the
// roots/list round-trip, then the re-pin, for a pin the client itself set.
func TestHandleRootsListChanged_FollowsClientFolder(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	rootA, rootB := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, rootA)
	mustGitDir(t, rootB)
	s := newConnSession(context.Background(), detectTestPool(), nil, config.NewStore(config.Defaults()), nil, nil, newSharedBudgets())
	defer s.close()

	s.handleRootsListChanged(context.Background(), rootsAnswer(t, rootA))
	if got := s.workspace(); got != rootA {
		t.Fatalf("first list_changed: workspace = %q, want %q", got, rootA)
	}
	s.handleRootsListChanged(context.Background(), rootsAnswer(t, rootB))
	if got := s.workspace(); got != rootB {
		t.Fatalf("client switched folders: workspace = %q, want %q", got, rootB)
	}
}

// TestHandleRootsListChanged_ExplicitPinOutranksRoots: now that the notification
// is live, a multiplexing client's folder churn must still not drag a pin an
// agent set with session_start (issue #182). The control above proves the same
// notification does move a roots-origin pin, so this is not passing vacuously.
func TestHandleRootsListChanged_ExplicitPinOutranksRoots(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	rootA, rootB := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, rootA)
	mustGitDir(t, rootB)
	s := newConnSession(context.Background(), detectTestPool(), nil, config.NewStore(config.Defaults()), nil, nil, newSharedBudgets())
	defer s.close()

	s.attachWorkspacePin(context.Background(), "file://"+rootA, sessionstate.PinSourceSessionStart)
	if got := s.workspace(); got != rootA {
		t.Fatalf("setup: workspace = %q, want %q", got, rootA)
	}
	s.handleRootsListChanged(context.Background(), rootsAnswer(t, rootB))
	if got := s.workspace(); got != rootA {
		t.Fatalf("roots change moved an explicit session_start pin: workspace = %q, want %q", got, rootA)
	}
}
