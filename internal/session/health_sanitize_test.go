package session_test

// health_sanitize_test.go — issue #358, change 5: HealthMessage embeds
// client-supplied text (a boundary-violation message quotes the offending
// path), and ESC is a legal byte in a POSIX path. A prior attempt stripped
// escapes inside one TUI renderer only (dashboard_alerts.go's blockedAlert),
// so the same field still reached the terminal raw through model_right.go's
// session detail pane and through internal/web/api_sessions.go. These tests
// assert the fix at the boundary both writers go through — Register and
// Patch both bottom out in writeSessionFileAtomic — so every reader gets
// already-clean text, not just the one that was patched last time.

import (
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/session"
)

func TestPatch_SanitizesHealthMessage(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	id, err := registerID(session.Info{Folder: "/tmp/x"})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	defer session.Unregister(id)

	const dirty = "boundary: /tmp/\x1b[31mevil"
	session.Patch(id, func(in *session.Info) {
		in.Health = "blocked"
		in.HealthMessage = dirty
	})

	infos, err := session.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var got string
	found := false
	for _, info := range infos {
		if info.ID == id {
			got, found = info.HealthMessage, true
		}
	}
	if !found {
		t.Fatalf("session %s not found after Patch", id)
	}
	if strings.Contains(got, "\x1b") {
		t.Fatalf("HealthMessage read back with an ESC byte still present: %q", got)
	}
	if !strings.Contains(got, "boundary: /tmp/") || !strings.Contains(got, "evil") {
		t.Fatalf("sanitisation ate more than the escape: got %q", got)
	}
}

func TestRegister_SanitizesHealthMessage(t *testing.T) {
	// Register is the other writer that reaches writeSessionFileAtomic; the
	// choke point must cover both, not just the one Patch exercises.
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	const dirty = "claim: /tmp/\x1b]0;evil\x07 refused"
	reg, err := session.Register(session.Info{Folder: "/tmp/x", Health: "blocked", HealthMessage: dirty})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	defer session.Unregister(reg.ID)

	infos, err := session.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var got string
	found := false
	for _, info := range infos {
		if info.ID == reg.ID {
			got, found = info.HealthMessage, true
		}
	}
	if !found {
		t.Fatalf("session %s not found after Register", reg.ID)
	}
	if strings.ContainsAny(got, "\x1b\x07") {
		t.Fatalf("HealthMessage read back with control bytes still present: %q", got)
	}
}
