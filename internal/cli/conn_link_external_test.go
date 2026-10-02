package cli

// conn_link_external_test.go — the rules the external-ID linker follows (#556): a
// connection relinks to the conversation that names it, who is told it resumed, and
// that no predecessor's mail or threads follow a conversation id.

import (
	"context"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/tools"
)

// What resuming means is delivered to the conversation's owner once, and a subagent
// that merely linked the connection first is told nothing and consumes nothing. What
// the owner is told is only what happened: the name came back, and nothing else did.
func TestDeliverResume(t *testing.T) {
	setup := func(t *testing.T) *connSession {
		t.Helper()
		t.Setenv("XDG_DATA_HOME", t.TempDir())
		s := newConnSession(context.Background(), detectTestPool(), nil, config.NewStore(config.Defaults()), nil, nil, newSharedBudgets())
		t.Cleanup(s.close)
		s.mutate(func(v *sessionView) { v.pendingResume = &resumeState{linkage: "conv", name: "old-owl"} })
		return s
	}
	told := tools.LinkResult{InheritedName: "old-owl", NewIdentity: true}

	t.Run("a subagent is told nothing and leaves it pending", func(t *testing.T) {
		s := setup(t)
		s.recordLogicalAgentCall("conv")
		s.recordLogicalAgentCall("conv/agent-1")
		if got := s.deliverResume("conv/agent-1", "conv"); got != (tools.LinkResult{}) {
			t.Errorf("a subagent was told %+v", got)
		}
		if s.view().pendingResume == nil {
			t.Error("a subagent consumed the owner's resume")
		}
	})

	t.Run("the stamped main thread is told, once, and inherits nothing", func(t *testing.T) {
		s := setup(t)
		if got := s.deliverResume("conv", "conv"); got != told {
			t.Errorf("the owner was told %+v, want %+v", got, told)
		}
		if ids := s.inheritedSessionIDs(); len(ids) != 0 {
			t.Errorf("the owner inherited %v: a conversation id authorises no predecessor's mail or threads", ids)
		}
		if s.view().pendingResume != nil {
			t.Error("the resume was not consumed")
		}
		if again := s.deliverResume("conv", "conv"); again != (tools.LinkResult{}) {
			t.Errorf("the owner was told twice: %+v", again)
		}
	})

	t.Run("an unstamped lone caller is told the name and inherits nothing", func(t *testing.T) {
		s := setup(t)
		if got := s.deliverResume("", "conv"); got != told {
			t.Errorf("an unstamped lone caller was told %+v, want %+v", got, told)
		}
		if ids := s.inheritedSessionIDs(); len(ids) != 0 {
			t.Errorf("an unstamped caller inherited %v: nothing vouches for the id it typed", ids)
		}
	})

	t.Run("an unstamped caller on a shared connection is nobody's owner", func(t *testing.T) {
		s := setup(t)
		s.recordLogicalAgentCall("conv-a")
		s.recordLogicalAgentCall("conv-b")
		if got := s.deliverResume("", "conv"); got != (tools.LinkResult{}) {
			t.Errorf("an unattributable caller was told %+v", got)
		}
		if s.view().pendingResume == nil {
			t.Error("an unattributable caller consumed the resume")
		}
	})

	t.Run("a resume found for another conversation is not this one's", func(t *testing.T) {
		s := setup(t)
		if got := s.deliverResume("conv-b", "conv-b"); got != (tools.LinkResult{}) {
			t.Errorf("conv-b was told %+v about conv's predecessor", got)
		}
		if s.view().pendingResume == nil {
			t.Error("conv-b consumed conv's resume")
		}
	})
}

// Claude Code's /clear keeps the serve connection and starts a NEW conversation id,
// so one agent's connection is relinked to a different conversation in the ordinary
// course of its work, and the old id never speaks again. The new conversation is the
// connection's: it is linked to it, it answers to the name its peers already write to,
// mail sent to that name reaches it, and the Stop hook and `plumb mail --external-id`
// find it by its new id. A guard that held the connection to the first conversation
// stranded all four. What lets the connection tell /clear from a second concurrent
// conversation is the SessionStart hook's announcement (conn_clear_handover.go),
// which the daemon holds as a marker until the new id's first call arrives.
func TestClearRelinksTheConnectionToTheNewConversation(t *testing.T) {
	w := newIdentityWorld(t)
	ws := identityRepo(t)
	peer, c := w.conn(""), w.conn("")
	peer.call("", "session_start", map[string]any{"workspace": ws})

	c.start("conv-OLD", ws, "conv-OLD", nil)
	name := c.s.sessionName()
	if out, isErr := peer.call("", "leave_note", map[string]any{"to": name, "body": "NOTE to the established name"}); isErr {
		t.Fatalf("peer leave_note: %s", out)
	}

	w.registry.clears.mark("conv-NEW")
	out := c.start("conv-NEW", "", "conv-NEW", nil)
	if got := c.s.externalID(); got != "conv-NEW" {
		t.Fatalf("after /clear the connection is linked to %q, want conv-NEW", got)
	}
	if got := c.s.sessionName(); got != name {
		t.Errorf("after /clear the connection answers to %q, want the name its peers write to, %q", got, name)
	}
	if line := sessionLine(out); !strings.Contains(line, name+" (you") {
		t.Errorf("the new conversation is not told it is %q: %q", name, line)
	}
	got, _ := c.call("conv-NEW", "check_messages", nil)
	if !strings.Contains(out+got, "NOTE to the established name") {
		t.Errorf("the new conversation cannot read mail addressed to its established name %q:\nsession_start: %q\ncheck_messages: %q", name, out, got)
	}
	info, err := resolveMailSessionFor("external-id", "conv-NEW")
	if err != nil {
		t.Fatalf("`plumb mail --external-id conv-NEW` finds no session: %v", err)
	}
	if info.ID != c.s.sessionID() {
		t.Errorf("`plumb mail --external-id conv-NEW` resolves to %q, want the connection %q", info.ID, c.s.sessionID())
	}
	if report, ok := hookMailReport("conv-NEW", ws); !ok || report.Session != name {
		t.Errorf("the Stop hook finds %q (ok=%v) for conv-NEW, want %q", report.Session, ok, name)
	}
}

// Relinking moves the connection's owner with it: the owner is whichever conversation
// the connection is linked to NOW. The one it was linked to is a stranger from then
// on, and a subagent of either is never the connection. A connection is relinked two
// ways: by a session_start that carries no stamp, which cannot be told from the
// conversation it replaces, and by the /clear handover.
func TestTheOwnerIsWhicheverConversationIsLinked(t *testing.T) {
	const old, now = "conv-OLD", "conv-NEW"
	for how, relink := range map[string]func(s *connSession){
		"an unstamped session_start": func(s *connSession) { s.linkExternalID(context.Background(), now) },
		"a /clear handover": func(s *connSession) {
			s.registry = newConnRegistry()
			s.registry.clears.mark(now)
			s.handOverOnClear(now)
		},
	} {
		t.Run(how, func(t *testing.T) {
			s := newIdentitySession(t)
			s.linkExternalID(stampedCtx(old), old)
			s.recordLogicalAgentCall(old)
			s.inheritSessionID("pred-1")

			relink(s)
			if got := s.externalID(); got != now {
				t.Fatalf("the connection is linked to %q, want %q", got, now)
			}
			s.recordLogicalAgentCall(now)
			s.recordLogicalAgentCall(now + "/agent-1")

			for _, c := range []struct {
				agent string
				owner bool
			}{
				{now, true},
				{now + "/agent-1", false},
				{old, false},
				{"", false},
			} {
				got := s.inheritedSessionIDsFor(stampedCtx(c.agent))
				if c.owner != (len(got) == 1 && got[0] == "pred-1") || (!c.owner && len(got) != 0) {
					t.Errorf("after the relink, %q inherited %v (owner=%v)", c.agent, got, c.owner)
				}
				name := s.sessionNameFor(stampedCtx(c.agent))
				if c.owner && name != s.sessionName() {
					t.Errorf("after the relink the new owner answers to %q, want the connection's %q", name, s.sessionName())
				}
				if !c.owner && c.agent != "" && (name == "" || name == s.sessionName()) {
					t.Errorf("after the relink %q answers to %q, want a name of its own and not the connection's %q", c.agent, name, s.sessionName())
				}
			}
		})
	}
}
