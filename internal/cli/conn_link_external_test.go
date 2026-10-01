package cli

// conn_link_external_test.go — the rules the external-ID linker follows (#556,
// #564), one case each: who may link a connection, who is told it resumed, and who
// inherits a predecessor's mail and threads.

import (
	"context"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/tools"
)

func TestPlanLink(t *testing.T) {
	cases := []struct {
		name    string
		linked  string   // the connection's existing linkage
		seen    []string // identities already committed on the connection
		stamp   string
		linkage string
		want    linkPlan
	}{
		{name: "first link by the stamped main thread", stamp: "conv", linkage: "conv", want: linkPlan{declare: true, link: true}},
		{name: "first link by a stamped subagent links its conversation", stamp: "conv/agent-1", linkage: "conv", want: linkPlan{declare: true, link: true}},
		{name: "first link by an unstamped caller on a lone connection", linkage: "conv", want: linkPlan{declare: true, link: true}},
		{
			name: "an unstamped caller cannot link a shared connection", seen: []string{"conv-a", "conv-b"}, linkage: "conv-c",
			want: linkPlan{declare: true, reason: tools.UnlinkedAnonymous},
		},
		{name: "the conversation already linked", linked: "conv", stamp: "conv", linkage: "conv", want: linkPlan{declare: true}},
		{name: "a subagent of the linked conversation", linked: "conv", stamp: "conv/agent-1", linkage: "conv", want: linkPlan{declare: true}},
		{
			name: "a stamped different conversation is declared but not linked", linked: "conv-a", stamp: "conv-b", linkage: "conv-b",
			want: linkPlan{declare: true, reason: tools.UnlinkedOtherConversation},
		},
		{
			name: "a subagent of a different conversation likewise", linked: "conv-a", stamp: "conv-b/agent-1", linkage: "conv-b",
			want: linkPlan{declare: true, reason: tools.UnlinkedOtherConversation},
		},
		{
			// Not linked, but still evidence the connection is shared: forgetting it would
			// admit the anonymous writes the gate exists to refuse.
			name: "an unstamped different id is declared but not linked", linked: "conv-a", linkage: "typed-id",
			want: linkPlan{declare: true, reason: tools.UnlinkedOtherConversation},
		},
		{
			name: "a stamp contradicting the id links nothing", linked: "conv-a", stamp: "conv-b", linkage: "conv-a",
			want: linkPlan{reason: tools.UnlinkedStampMismatch},
		},
		{
			name: "a stamp contradicting the id on an unlinked connection", stamp: "conv-b", linkage: "conv-a",
			want: linkPlan{reason: tools.UnlinkedStampMismatch},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("XDG_DATA_HOME", t.TempDir())
			s := newConnSession(context.Background(), detectTestPool(), nil, config.NewStore(config.Defaults()), nil, nil, newSharedBudgets())
			t.Cleanup(s.close)
			if c.linked != "" {
				session.SetExternalID(s.sessionID(), c.linked)
			}
			for _, id := range c.seen {
				s.recordLogicalAgentCall(id)
			}
			if got := s.planLink(c.stamp, c.linkage); got != c.want {
				t.Errorf("planLink(%q, %q) = %+v, want %+v", c.stamp, c.linkage, got, c.want)
			}
		})
	}
}

// Only the main thread of the conversation inherits: each condition is one row.
func TestIsMainThread(t *testing.T) {
	cases := []struct {
		stamp, linkage string
		want           bool
	}{
		{"conv", "conv", true},
		{"", "conv", false},                     // an unstamped id is a claim nothing vouches for
		{"conv-2", "conv", false},               // another conversation
		{"conv/agent-1", "conv", false},         // a subagent of this one
		{"conv/agent-1", "conv/agent-1", false}, // even one that matches itself
	}
	for _, c := range cases {
		if got := isMainThread(c.stamp, c.linkage); got != c.want {
			t.Errorf("isMainThread(%q, %q) = %v, want %v", c.stamp, c.linkage, got, c.want)
		}
	}
}

// What resuming means is delivered to the conversation's owner once, and a subagent
// that merely linked the connection first is told nothing and consumes nothing.
func TestDeliverResume(t *testing.T) {
	setup := func(t *testing.T) *connSession {
		t.Helper()
		t.Setenv("XDG_DATA_HOME", t.TempDir())
		s := newConnSession(context.Background(), detectTestPool(), nil, config.NewStore(config.Defaults()), nil, nil, newSharedBudgets())
		t.Cleanup(s.close)
		s.mutate(func(v *sessionView) { v.pendingResume = &resumeState{predecessorID: "pred-1", name: "old-owl"} })
		return s
	}

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
		if ids := s.inheritedSessionIDs(); len(ids) != 0 {
			t.Errorf("a subagent's call granted %v", ids)
		}
	})

	t.Run("the stamped main thread is told, and inherits", func(t *testing.T) {
		s := setup(t)
		got := s.deliverResume("conv", "conv")
		want := tools.LinkResult{InheritedName: "old-owl", ThreadsInherited: true}
		if got != want {
			t.Errorf("the owner was told %+v, want %+v", got, want)
		}
		if ids := s.inheritedSessionIDs(); len(ids) != 1 || ids[0] != "pred-1" {
			t.Errorf("the owner inherited %v, want [pred-1]", ids)
		}
		if s.view().pendingResume != nil {
			t.Error("the resume was not consumed")
		}
		if again := s.deliverResume("conv", "conv"); again != (tools.LinkResult{}) {
			t.Errorf("the owner was told twice: %+v", again)
		}
	})

	t.Run("an unstamped owner is told the name and inherits nothing", func(t *testing.T) {
		s := setup(t)
		got := s.deliverResume("", "conv")
		want := tools.LinkResult{InheritedName: "old-owl", NewIdentity: true}
		if got != want {
			t.Errorf("an unstamped lone caller was told %+v, want %+v", got, want)
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
}
