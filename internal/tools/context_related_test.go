package tools

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// context_related_test.go — bodies of related declarations: what is read for them,
// what is recorded as read, and what is said when one cannot be read.

// A related body is a read like any other (Invariant 4): delivering it records the
// file it was sliced from, once, with that snapshot's own version; a pack that could
// not afford the body records nothing for it.
func TestContextForTask_ARelatedBodyIsARecordedReadOnlyWhenDelivered(t *testing.T) {
	s := newShop(t)
	var calls []string
	tracker := NewReadTracker()
	tracker.SetPersistSink(func(path string, _ time.Time, _ string) { calls = append(calls, path) })
	s.tool.WithReads(tracker)
	args := map[string]any{"symbols": []string{"cart/cart.go#Cart.Total"}}
	canon := canonicalRoot(s.root)
	cart, pricing := filepath.Join(canon, "cart", "cart.go"), filepath.Join(canon, "pricing", "discount.go")

	out, err := s.run(t, args)
	if err != nil {
		t.Fatal(err)
	}
	related := parseRelatedBodies(t, out)
	if len(related) < 2 {
		t.Fatalf("want the callees' bodies delivered, got %d:\n%s", len(related), out)
	}
	if len(calls) != 2 || calls[0] != lockPathKey(cart) || calls[1] != lockPathKey(pricing) {
		t.Errorf("recorded %v, want cart.go (the seed) and pricing/discount.go (the callees), once each", calls)
	}
	if related[0].guard == nil || related[0].guard.path != "pricing/discount.go" || related[0].guard.sha != fileSHA(t, filepath.Join(s.root, "pricing", "discount.go")) {
		t.Errorf("the first related body of a file must carry that file's guard, got %+v", related[0].guard)
	}

	// Negative control: at a budget that keeps the seed's body but has no room for a
	// related one, only the seed is a read. The sweep finds that budget rather than
	// assuming one, and fails if the packer never produces it.
	for mb := contextMinMaxBytes; mb <= 6000; mb += 50 {
		calls = nil
		args["max_bytes"] = mb
		out, err = s.run(t, args)
		if err != nil {
			t.Fatal(err)
		}
		if len(parseBodies(t, out)) != 1 || len(parseRelatedBodies(t, out)) != 0 {
			continue
		}
		if len(calls) != 1 || calls[0] != lockPathKey(cart) {
			t.Errorf("max_bytes %d recorded %v, want only the seed's file: an omitted related body is not a read", mb, calls)
		}
		return
	}
	t.Fatal("no budget delivered the seed's body without a related one, so the control proved nothing")
}

// A related declaration whose body cannot be read still shows its line, and the
// pack accounts for the missing body rather than leaving it unexplained.
func TestContextForTask_AnUnreadableRelatedBodyIsAccountedFor(t *testing.T) {
	s := newModuleWorkspace(t, map[string]string{
		"go.mod":   chainModule,
		"a/a.go":   "package a\n\nimport \"example.com/x/big\"\n\n// A calls into a large file.\nfunc A() int { return big.F() }\n",
		"big/f.go": "package big\n\n// F lives in a file larger than four times max_bytes.\nfunc F() int { return 1 }\n\n// " + strings.Repeat("padding ", 3000) + "\nfunc G() int { return 2 }\n",
	})
	out, err := s.run(t, map[string]any{"symbols": []string{"a/a.go#A"}, "max_bytes": 3500})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[e2 derived] F — big/f.go:4 function; callee of A") {
		t.Errorf("the related declaration's line must survive an unreadable body:\n%s", out)
	}
	if !strings.Contains(out, "related declaration(s) show a signature only, no body (source-read cap reached") {
		t.Errorf("the missing body is not accounted for:\n%s", out)
	}
	if len(parseRelatedBodies(t, out)) != 0 {
		t.Errorf("a body was delivered from a file over the read cap:\n%s", out)
	}
}
