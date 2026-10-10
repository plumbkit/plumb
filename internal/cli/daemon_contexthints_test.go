package cli

import (
	"os"
	"testing"
)

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

func TestContextHintsEnabled_EnvOffSwitch(t *testing.T) {
	for v, want := range map[string]bool{"": true, "on": true, "off": false, "OFF": false, "0": false, "false": false} {
		t.Setenv(contextHintsEnv, v)
		if got := contextHintsEnabled(); got != want {
			t.Errorf("%s=%q: enabled = %v, want %v", contextHintsEnv, v, got, want)
		}
	}
}
