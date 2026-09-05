package tools

// session_start_self_test.go — the caller must be able to find ITSELF in its own
// orientation packet.
//
// The incident that produced these tests: an agent called session_start, the
// response named one session — a PEER — and the agent reported that its own name
// had changed to the peer's. It had not. The packet simply never said who the
// caller was, and the only name in it belonged to somebody else. Every
// assertion here is about that specific confusion, which is why they check the
// self line and the peer list TOGETHER rather than either alone.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/session"
)

// selfStart builds a SessionStart wired with an identity, as registerAllTools
// wires it in the daemon.
func selfStart(t *testing.T, ws, name, id string) *SessionStart {
	t.Helper()
	return NewSessionStart(func(context.Context) string { return ws }, nil, nil, nil,
		func() string { return "" }, nil).
		WithSelfSession(func() string { return id }).
		WithSelfIdentity(func() string { return name })
}

// TestSessionStart_NamesTheCallerOnFirstContact is the headline regression. It
// deliberately runs with NO resumed name and NO linkage — an ordinary first
// contact, the exact case the old identity block said nothing at all in.
func TestSessionStart_NamesTheCallerOnFirstContact(t *testing.T) {
	ws := t.TempDir()
	for _, detail := range []string{"full", "brief"} {
		t.Run(detail, func(t *testing.T) {
			out, err := selfStart(t, ws, "calm-stag", "abcd1234deadbeef").
				Execute(t.Context(), json.RawMessage(`{"detail":"`+detail+`"}`))
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if !strings.Contains(out, "calm-stag") {
				t.Fatalf("%s packet never names the caller:\n%s", detail, out)
			}
			// The label is the whole point: a bare name beside a peer list is
			// what the caller misread in the first place.
			if !strings.Contains(out, "calm-stag (you") {
				t.Fatalf("%s packet names the caller without saying it is the caller:\n%s", detail, out)
			}
			if !strings.Contains(out, "abcd1234") {
				t.Fatalf("%s packet omits the caller's session ID, which is what mail is bound "+
					"to and what daemon_info correlates on:\n%s", detail, out)
			}
		})
	}
}

// TestSessionStart_CallerIsDistinguishableFromItsPeers reproduces the incident's
// shape directly: a live peer exists, and the packet must make it impossible to
// mistake the peer's name for the caller's.
func TestSessionStart_CallerIsDistinguishableFromItsPeers(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ws := t.TempDir()

	peer, err := session.Register(session.Info{Name: "azure-falcon", Folder: ws})
	if err != nil {
		t.Fatalf("registering the peer: %v", err)
	}
	t.Cleanup(func() { session.Unregister(peer.ID) })

	out, err := selfStart(t, ws, "stark-narwhal", "abcd1234deadbeef").
		WithCollab(func() (bool, int) { return true, 4096 }).
		Execute(t.Context(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "stark-narwhal (you") {
		t.Fatalf("the caller is not named as itself:\n%s", out)
	}
	if !strings.Contains(out, "azure-falcon") {
		t.Skip("the peer digest did not render, so this test cannot exercise the confusion it targets")
	}
	// The peer must be presented as a peer. Both names now appear, and only the
	// labels tell them apart — so the labels are what is asserted.
	if strings.Contains(out, "azure-falcon (you") {
		t.Fatalf("a PEER is labelled as the caller:\n%s", out)
	}
	selfAt := strings.Index(out, "stark-narwhal (you")
	peerAt := strings.Index(out, "azure-falcon")
	if peerAt < selfAt {
		t.Errorf("the peer is named before the caller is; the first name in the packet must be "+
			"the reader's own, which is the ordering the incident turned on:\n%s", out)
	}
}

// TestSessionStart_SelfLineSurvivesTheBriefBudget guards the constraint that
// makes the brief packet worth having. The self line is small, but "small" is
// not an argument — the budget is the argument, so it is measured.
func TestSessionStart_SelfLineSurvivesTheBriefBudget(t *testing.T) {
	ws := t.TempDir()
	out, err := selfStart(t, ws, "calm-stag", "abcd1234deadbeef").
		Execute(t.Context(), json.RawMessage(`{"detail":"brief"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "calm-stag (you") {
		t.Fatalf("brief dropped the self line:\n%s", out)
	}
	const briefBudget = 1536 // the ≤1.5 KB the brief packet promises
	if len(out) > briefBudget {
		t.Errorf("brief packet is %d bytes, over the %d-byte budget:\n%s", len(out), briefBudget, out)
	}
}

// TestSessionStart_ResumedNameIsStillAnnounced pins the interaction between the
// new self line and the older resumed-name announcement, in both directions.
//
// The failure this guards against is subtle and was real during development:
// adding the self line silently swallowed the resumed name for any caller that
// had not wired the self accessor, retiring PR #189's bootstrap guarantee as a
// side effect of strengthening a different one.
func TestSessionStart_ResumedNameIsStillAnnounced(t *testing.T) {
	ws := t.TempDir()

	t.Run("with a self accessor the resumed marker rides the self line", func(t *testing.T) {
		out, err := selfStart(t, ws, "calm-stag", "abcd1234deadbeef").
			WithExternalID(func(string) string { return "calm-stag" }).
			Execute(t.Context(), json.RawMessage(`{"session_id":"conv-1"}`))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if !strings.Contains(out, "calm-stag (you") || !strings.Contains(out, "resumed") {
			t.Fatalf("a resumed session must be told both who it is and that it resumed:\n%s", out)
		}
	})

	t.Run("without a self accessor the old resumed line still renders", func(t *testing.T) {
		out, err := NewSessionStart(func(context.Context) string { return ws }, nil, nil, nil,
			func() string { return "" }, nil).
			WithExternalID(func(string) string { return "calm-stag" }).
			Execute(t.Context(), json.RawMessage(`{"session_id":"conv-1"}`))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if !strings.Contains(out, "calm-stag (resumed)") {
			t.Fatalf("an unwired caller lost the resumed-name announcement entirely:\n%s", out)
		}
	})

	t.Run("a refused inheritance says so rather than showing a name silently", func(t *testing.T) {
		// externalIDFn reports the name it TRIED to inherit; the session is
		// actually called something else because a live peer holds it. Showing
		// only the actual name would look like the session_id argument did
		// nothing at all.
		out, err := selfStart(t, ws, "velvet-bison", "abcd1234deadbeef").
			WithExternalID(func(string) string { return "calm-stag" }).
			Execute(t.Context(), json.RawMessage(`{"session_id":"conv-1"}`))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if !strings.Contains(out, "velvet-bison (you") {
			t.Fatalf("the packet must name what the session is ACTUALLY called:\n%s", out)
		}
		if !strings.Contains(out, "requested calm-stag") {
			t.Fatalf("a refused inheritance must be visible, got:\n%s", out)
		}
	})
}

// TestSessionStart_UnregisteredSessionIsNotPresentedAsAddressable: a session
// whose registration failed has a display name drawn without a uniqueness check
// and no file for any peer's check to find. Naming it beside an ID would invite
// the caller to hand it out as a mail address it cannot receive on.
func TestSessionStart_UnregisteredSessionIsNotPresentedAsAddressable(t *testing.T) {
	ws := t.TempDir()
	out, err := selfStart(t, ws, "lonely-heron", "").
		Execute(t.Context(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "lonely-heron (you)") {
		t.Fatalf("an unregistered session should still be told what it is called:\n%s", out)
	}
	if strings.Contains(out, "(you, id ") {
		t.Fatalf("an unregistered session has no ID and none must be implied:\n%s", out)
	}
}

// TestShortSessionID keeps the abbreviation honest at its boundaries: it must
// never lengthen a value, and must always mark that it truncated.
func TestShortSessionID(t *testing.T) {
	cases := map[string]string{
		"":                 "",
		"abc":              "abc",
		"abcd1234":         "abcd1234",
		"abcd12345":        "abcd1234…",
		"abcd1234deadbeef": "abcd1234…",
	}
	for in, want := range cases {
		if got := shortSessionID(in); got != want {
			t.Errorf("shortSessionID(%q) = %q, want %q", in, got, want)
		}
	}
}
