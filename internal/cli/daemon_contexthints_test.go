package cli

import (
	"testing"

	"github.com/plumbkit/plumb/internal/tools"
)

// A hint is routed only to an identity exactly one live connection knows: none
// or two is no root at all.
func TestHintRoot_ExactlyOneLiveConnection(t *testing.T) {
	registry := newConnRegistry()
	know := func(id, root string) func(string) (tools.Inbox, bool) {
		return func(external string) (tools.Inbox, bool) {
			return tools.Inbox{Self: "n", SelfID: "i", Root: root}, external == id
		}
	}
	if _, ok := registry.hintRoot("conv-1"); ok {
		t.Fatal("an empty registry produced a root")
	}
	registry.add("a", connHandle{mailboxInbox: know("conv-1", "/ws/one")})
	if root, ok := registry.hintRoot("conv-1"); !ok || root != "/ws/one" {
		t.Fatalf("hintRoot = %q, %v; want /ws/one", root, ok)
	}
	if _, ok := registry.hintRoot("conv-2"); ok {
		t.Fatal("an identity no connection knows produced a root")
	}
	registry.add("b", connHandle{mailboxInbox: know("conv-1", "/ws/two")})
	if root, ok := registry.hintRoot("conv-1"); ok {
		t.Fatalf("an identity two connections claim resolved to %q", root)
	}
}

func TestContextHintsEnabled_EnvOffSwitch(t *testing.T) {
	for v, want := range map[string]bool{"": true, "on": true, "off": false, "OFF": false, "0": false, "false": false} {
		t.Setenv(contextHintsEnv, v)
		if got := contextHintsEnabled(); got != want {
			t.Errorf("%s=%q: enabled = %v, want %v", contextHintsEnv, v, got, want)
		}
	}
}
