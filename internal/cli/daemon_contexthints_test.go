package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/plumbkit/plumb/internal/contexthints"
)

// A daemon whose hints are off never opens the ledger, so a user who never
// turns hints on gets no context_hints.db; the first request that finds them on
// opens it, once (cross-review S3).
func TestStartContextHints_LedgerOpensOnlyOnceHintsAreOn(t *testing.T) {
	t.Setenv(contextHintsEnv, "")
	path := filepath.Join(t.TempDir(), "context_hints.db")
	opened := 0
	prev := openContextHintLedger
	openContextHintLedger = func() (*contexthints.Store, error) { opened++; return contexthints.OpenAt(path) }
	t.Cleanup(func() { openContextHintLedger = prev })

	on := false
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handler, closeLedger := startContextHints(ctx, newConnRegistry(), nil, func() bool { return on })
	defer closeLedger()
	req := contextHintRequest{Host: "claude-code", Event: "UserPromptSubmit", SessionID: "c"}

	if r := handler(ctx, req); r.Outcome != contexthints.OutcomeNoop || opened != 0 {
		t.Fatalf("hints off: outcome %q after %d opens, want noop and none", r.Outcome, opened)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("hints off created the ledger file (stat err %v)", err)
	}
	on = true
	handler(ctx, req)
	handler(ctx, req)
	if opened != 1 {
		t.Fatalf("ledger opened %d times once hints were on, want 1", opened)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("hints on did not create the ledger: %v", err)
	}
}

// A hint is routed only to an identity exactly one live connection knows: none
// or two is no root at all.
func TestHintRoot_ExactlyOneLiveConnection(t *testing.T) {
	registry := newConnRegistry()
	know := func(id, root string) func(string) (string, bool, bool) {
		return func(external string) (string, bool, bool) {
			return root, false, external == id
		}
	}
	if _, ok := registry.hintRoot("conv-1"); ok {
		t.Fatal("an empty registry produced a root")
	}
	registry.add("a", connHandle{hintRoot: know("conv-1", "/ws/one")})
	if root, ok := registry.hintRoot("conv-1"); !ok || root.Path != "/ws/one" {
		t.Fatalf("hintRoot = %+v, %v; want /ws/one", root, ok)
	}
	if _, ok := registry.hintRoot("conv-2"); ok {
		t.Fatal("an identity no connection knows produced a root")
	}
	registry.add("b", connHandle{hintRoot: know("conv-1", "/ws/two")})
	if root, ok := registry.hintRoot("conv-1"); ok {
		t.Fatalf("an identity two connections claim resolved to %+v", root)
	}
}

// A subagent that has made no call resolves to the root its first call will be
// seeded with (shardOf's seeding), not to the connection's root: on a shared
// connection where conversation A pinned itself to X while the connection sits
// at Y, A's new subagent starts at X, and B's (which chose nothing) at Y. Asking
// creates no shard. A conversation the connection does not know resolves to
// nothing.
func TestHookHintRoot_SubagentGetsItsFirstCallRoot(t *testing.T) {
	w := newIdentityWorld(t)
	x, y := identityRepo(t), identityRepo(t)
	c := w.conn("")
	c.start(convA, y, convA, nil)
	// A second conversation: the connection is shared.
	c.start(convB, y, convB, nil)
	// A moves itself to X (pins are sticky, #182); the connection stays at Y.
	c.start(convA, x, convA, map[string]any{"force": true})
	if got := c.s.workspace(); !sameDir(t, got, y) {
		t.Fatalf("connection root = %q, want it left at %q (the precondition)", got, y)
	}

	root, inherited, ok := c.s.hookHintRoot(convA + "/agent-1")
	if !ok || !inherited || !sameDir(t, root, x) {
		t.Fatalf("A's new subagent = %q inherited=%v ok=%v, want %q inherited", root, inherited, ok, x)
	}
	root, inherited, ok = c.s.hookHintRoot(convB + "/agent-2")
	if !ok || !inherited || !sameDir(t, root, y) {
		t.Fatalf("B's new subagent = %q inherited=%v ok=%v, want %q inherited", root, inherited, ok, y)
	}
	c.s.shardsMu.Lock()
	_, made := c.s.shards[convA+"/agent-1"]
	c.s.shardsMu.Unlock()
	if made {
		t.Error("resolving a hint root created the subagent's shard")
	}
	if _, _, ok := c.s.hookHintRoot("conv-UNKNOWN/agent-9"); ok {
		t.Error("a subagent of a conversation this connection does not know resolved")
	}
}

func sameDir(t *testing.T, a, b string) bool {
	t.Helper()
	ia, errA := os.Stat(a)
	ib, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(ia, ib)
}

// The environment overrides the config in either direction; unset (or any other
// value) leaves the config in charge, and the config defaults to off.
func TestContextHintsEnabled(t *testing.T) {
	for _, tc := range []struct {
		env    string
		config bool
		want   bool
	}{
		{"", false, false},
		{"", true, true},
		{"on", false, true},
		{"1", false, true},
		{"TRUE", false, true},
		{"off", true, false},
		{"0", true, false},
		{"false", true, false},
		{"maybe", true, true},
		{"maybe", false, false},
	} {
		t.Setenv(contextHintsEnv, tc.env)
		if got := contextHintsEnabled(func() bool { return tc.config }); got != tc.want {
			t.Errorf("%s=%q config=%v: enabled = %v, want %v", contextHintsEnv, tc.env, tc.config, got, tc.want)
		}
	}
}
