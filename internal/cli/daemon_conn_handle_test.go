package cli

import (
	"context"
	"path/filepath"
	"testing"
)

// The registry reaches a connection only through the handle newConnHandle builds,
// so every hook must be set, and the sensitive-path decision must be the
// connection's own: a file the default sensitive globs cover is withheld, an
// ordinary one is not (the control).
func TestNewConnHandle_CarriesEveryHookAndTheConnectionsSensitiveDecision(t *testing.T) {
	s, _ := buildTestConnSession(t)
	root := t.TempDir()
	s.mutate(func(v *sessionView) { v.acquiredRoot = root })
	h := newConnHandle(s)

	for name, set := range map[string]bool{
		"cancel": h.cancel != nil, "workspace": h.workspace != nil, "proxySessionID": h.proxySessionID != nil,
		"reloadProject": h.reloadProject != nil, "summarise": h.summarise != nil, "mailboxInbox": h.mailboxInbox != nil,
		"hintRoot": h.hintRoot != nil, "sensitive": h.sensitive != nil,
	} {
		if !set {
			t.Errorf("the handle has no %s", name)
		}
	}
	if h.workspace == nil || h.sensitive == nil {
		t.FailNow()
	}
	if h.workspace() != root {
		t.Errorf("handle workspace = %q, want the connection's %q", h.workspace(), root)
	}
	ctx := context.Background()
	if !h.sensitive(ctx, filepath.Join(root, ".env"), "") {
		t.Error("a .env file is not sensitive through the handle")
	}
	if h.sensitive(ctx, filepath.Join(root, "main.go"), "") {
		t.Error("control: an ordinary file is sensitive through the handle")
	}
}
