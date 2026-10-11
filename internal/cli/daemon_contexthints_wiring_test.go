package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/tools"
	"github.com/plumbkit/plumb/internal/topology"
)

// The registry answers the hint collector's sensitive-path question with the
// decision of the deepest live connection whose workspace holds the path, read
// canonically, so a nested workspace's own globs win over its parent's. A path no
// connection holds, including one in a sibling directory that merely shares a
// name prefix, is withheld without asking anyone.
func TestConnRegistry_ChangeSensitiveAsksTheDeepestHoldingConnection(t *testing.T) {
	outer := t.TempDir()
	inner := filepath.Join(outer, "inner")
	sibling := outer + "-sibling"
	for _, d := range []string{inner, sibling} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { os.RemoveAll(sibling) })
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(inner, link); err != nil {
		t.Fatal(err)
	}

	var asked []string
	decide := func(name string, verdict bool) tools.SensitivePathFn {
		return func(_ context.Context, _, _ string) bool { asked = append(asked, name); return verdict }
	}
	registry := newConnRegistry()
	registry.add("outer", connHandle{workspace: func() string { return outer }, sensitive: decide("outer", false)})
	registry.add("inner", connHandle{workspace: func() string { return inner }, sensitive: decide("inner", true)})
	registry.add("no-decision", connHandle{workspace: func() string { return outer }})

	cases := []struct {
		path    string
		want    bool
		askedBy string
	}{
		{filepath.Join(inner, "x.go"), true, "inner"},
		{filepath.Join(link, "x.go"), true, "inner"},
		{filepath.Join(outer, "y.go"), false, "outer"},
		{filepath.Join(sibling, "z.go"), true, ""},
		{filepath.Join(t.TempDir(), "w.go"), true, ""},
	}
	for _, tc := range cases {
		asked = nil
		if got := registry.changeSensitive(context.Background(), tc.path, ""); got != tc.want {
			t.Errorf("changeSensitive(%s) = %v, want %v", tc.path, got, tc.want)
		}
		switch {
		case tc.askedBy == "" && len(asked) != 0:
			t.Errorf("%s: no connection holds it, yet %v was asked", tc.path, asked)
		case tc.askedBy != "" && (len(asked) != 1 || asked[0] != tc.askedBy):
			t.Errorf("%s: asked %v, want only %s", tc.path, asked, tc.askedBy)
		}
	}
}

// The daemon's hint collector reads the index storeFor holds for the request's
// root, and carries the sensitive-path decision.
func TestNewDaemonContextHinter_IsWiredToTheIndexAndTheSensitiveDecision(t *testing.T) {
	root := t.TempDir()
	var askedFor []string
	storeFor := func(r string) *topology.Store { askedFor = append(askedFor, r); return nil }
	hinter := newDaemonContextHinter(storeFor, newConnRegistry())

	wired, ok := hinter.(interface{ SensitiveWired() bool })
	if !ok || !wired.SensitiveWired() {
		t.Fatalf("%T carries no sensitive-path decision", hinter)
	}
	if _, err := hinter.Hint(context.Background(), tools.HintRequest{
		Workspace: root, Seeds: []tools.ContextSeed{{Symbol: "Total"}}, MaxBytes: contextHintPerTurn, Deadline: time.Now().Add(time.Second),
	}); err != nil {
		t.Fatalf("Hint: %v", err)
	}
	if len(askedFor) == 0 {
		t.Fatal("the hinter never asked storeFor for the request's index")
	}
}
