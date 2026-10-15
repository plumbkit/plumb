package web

import (
	"testing"

	"github.com/plumbkit/plumb/internal/session"
)

func TestDefaultSessions_HidesNamespacedSessions(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	def, err := session.Register(session.Info{Folder: "/w"})
	if err != nil {
		t.Fatal(err)
	}
	nsd, err := session.Register(session.Info{Folder: "/w", Namespace: "ns-1"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := defaultSessions()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, info := range got {
		seen[info.ID] = true
	}
	if !seen[def.ID] {
		t.Fatal("control: the default session is missing")
	}
	if seen[nsd.ID] {
		t.Fatal("a namespaced session is listed in the user's view")
	}
}
