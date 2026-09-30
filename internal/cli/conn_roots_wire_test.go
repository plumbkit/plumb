package cli

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/mcp"
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
	s.markInitSettled()

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
	s.markInitSettled()

	s.attachWorkspacePin(context.Background(), "file://"+rootA, sessionstate.PinSourceSessionStart)
	if got := s.workspace(); got != rootA {
		t.Fatalf("setup: workspace = %q, want %q", got, rootA)
	}
	s.handleRootsListChanged(context.Background(), rootsAnswer(t, rootB))
	if got := s.workspace(); got != rootA {
		t.Fatalf("roots change moved an explicit session_start pin: workspace = %q, want %q", got, rootA)
	}
}

// TestHandleRootsListChanged_WaitsForTheAttachLadder: on a reconnect the roots
// notification and OnInit's attach ladder run in separate goroutines. A
// handler that attached first skipped the ladder's restore of the persisted
// session_start pin and overwrote the stored row with the client's root — the
// silent cross-repo move TestOnInit_RootsAttachDoesNotClobberSessionStartPin
// guards against, reopened by the newly live handler. It must wait.
func TestHandleRootsListChanged_WaitsForTheAttachLadder(t *testing.T) {
	store, ss := newOriginStore(t)
	rootA, rootB := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, rootA)
	mustGitDir(t, rootB)
	calls := 0
	before := newPersistSession(t, store, ss, "proxyX")
	before.attachOnInit(context.Background(), rootsReplying(rootA, &calls))
	if _, err := before.repinWorkspace(context.Background(), rootB, "", false, false); err != nil {
		t.Fatalf("repinWorkspace: %v", err)
	}
	before.close()

	after := newPersistSession(t, store, ss, "proxyX")
	done := make(chan struct{})
	go func() {
		defer close(done)
		after.handleRootsListChanged(context.Background(), rootsReplying(rootA, &calls))
	}()
	select {
	case <-done:
		t.Fatal("the roots handler ran before the attach ladder settled")
	case <-time.After(100 * time.Millisecond):
	}
	after.setClientRequest(rootsReplying(rootA, &calls))
	after.attachOnInit(context.Background(), rootsReplying(rootA, &calls))
	after.markInitSettled()
	<-done

	if got := after.workspace(); got != rootB {
		t.Fatalf("workspace = %q, want the restored session_start pin %q", got, rootB)
	}
	ws, _, src, ok, err := ss.LoadPin("proxyX")
	if err != nil || !ok || ws != rootB || src != sessionstate.PinSourceSessionStart {
		t.Fatalf("stored pin = (%q, %q, ok=%v, err=%v), want (%q, %q)", ws, src, ok, err, rootB, sessionstate.PinSourceSessionStart)
	}
}

// TestHandleRootsListChanged_NoAttachAfterClose: an attach after close() would
// take a language-server reference nothing will ever release.
func TestHandleRootsListChanged_NoAttachAfterClose(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	root := freshTempDir(t)
	mustGitDir(t, root)
	s := newConnSession(context.Background(), detectTestPool(), nil, config.NewStore(config.Defaults()), nil, nil, newSharedBudgets())
	s.markInitSettled()
	s.close()
	s.handleRootsListChanged(context.Background(), rootsAnswer(t, root))
	if got := s.workspace(); got != "" {
		t.Fatalf("a closed connection attached %q", got)
	}
}

// TestRegisterHooks_RootsHandlerRunsAfterOnInit drives the callbacks
// registerHooks installs, not a hand-called markInitSettled: the OnInit wrapper
// is the only production path that releases the roots handler, so a refactor
// that dropped its deferred markInitSettled would park every roots notification
// until disconnect, bringing back "a client's folder change never lands".
func TestRegisterHooks_RootsHandlerRunsAfterOnInit(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	rootA, rootB := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, rootA)
	mustGitDir(t, rootB)
	s := newConnSession(context.Background(), detectTestPool(), nil, config.NewStore(config.Defaults()), nil, nil, newSharedBudgets())
	defer s.close()
	srv := mcp.New(mcp.ServerInfo{Name: "plumb", Version: "0"})
	s.registerHooks(srv)
	if srv.OnInit == nil || srv.OnRootsChanged == nil {
		t.Fatal("registerHooks left OnInit or OnRootsChanged unwired")
	}

	srv.OnInit(context.Background(), rootsAnswer(t, rootA), func(string, any) error { return nil })
	if got := s.workspace(); got != rootA {
		t.Fatalf("OnInit attach: workspace = %q, want %q", got, rootA)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.OnRootsChanged(context.Background(), rootsAnswer(t, rootB))
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the roots handler is still waiting after OnInit returned: nothing released it")
	}
	if got := s.workspace(); got != rootB {
		t.Fatalf("roots change after OnInit: workspace = %q, want %q", got, rootB)
	}
}

// TestHandleRootsListChanged_CloseDuringRootsList: the connection closes while
// the client is answering roots/list. The handler must not attach afterwards.
func TestHandleRootsListChanged_CloseDuringRootsList(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	root := freshTempDir(t)
	mustGitDir(t, root)
	s := newConnSession(context.Background(), detectTestPool(), nil, config.NewStore(config.Defaults()), nil, nil, newSharedBudgets())
	s.markInitSettled()
	answer := rootsAnswer(t, root)
	closing := func(ctx context.Context, method string, params any) (json.RawMessage, error) {
		s.close()
		return answer(ctx, method, params)
	}
	s.handleRootsListChanged(context.Background(), closing)
	if got := s.workspace(); got != "" {
		t.Fatalf("attached %q after the connection closed during roots/list", got)
	}
}
