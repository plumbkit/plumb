package tools

// session_start_caller_test.go — what the tools do with a caller's identity once
// the connection can tell its callers apart (#556): nobody is handed another
// agent's name, session ID, mail or commit signature, and what resuming a
// conversation means is told to the agent it happened to.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/plumbkit/plumb/internal/mcp"
)

// callerNames resolves a name per stamped agent, and "" for an unstamped call —
// the shape connSession.sessionNameFor has on a shared connection.
func callerNames(names map[string]string) func(context.Context) string {
	return func(ctx context.Context) string { return names[mcp.LogicalAgentFromCtx(ctx)] }
}

// The per-call copy is what lets one registered SessionStart answer for many
// callers at once. Run concurrently, each of them must see only its own identity.
func TestSessionStart_EachCallerIsToldItsOwnIdentity(t *testing.T) {
	names := map[string]string{"conv": "azure-falcon", "conv/agent-1": "pale-finch", "conv/agent-2": "calm-stag"}
	ids := map[string]string{"conv": "aaaaaaaa-1111", "conv/agent-1": "bbbbbbbb-2222", "conv/agent-2": "cccccccc-3333"}
	tool := NewSessionStart(func(context.Context) string { return t.TempDir() }, nil, nil, nil, func() string { return "" }, nil).
		WithCallerIdentity(callerNames(names), func(ctx context.Context) string { return ids[mcp.LogicalAgentFromCtx(ctx)] })

	var wg sync.WaitGroup
	for agent, name := range names {
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				out, err := tool.Execute(mcp.WithLogicalAgent(context.Background(), agent), json.RawMessage(`{"detail":"brief"}`))
				if err != nil {
					t.Errorf("%s: %v", agent, err)
					return
				}
				line := ""
				for _, l := range strings.Split(out, "\n") {
					if strings.HasPrefix(l, "Session:") {
						line = l
					}
				}
				if !strings.Contains(line, name+" (you") || !strings.Contains(line, ids[agent][:8]) {
					t.Errorf("%s was not told its own identity %s/%s: %q", agent, name, ids[agent][:8], line)
				}
				for other, otherName := range names {
					if other != agent && strings.Contains(line, otherName) {
						t.Errorf("%s was told it is %s (%s): %q", agent, other, otherName, line)
					}
				}
			}()
		}
	}
	wg.Wait()
}

// A caller with no identity of its own is told nothing about who it is, never the
// connection's name.
func TestSessionStart_AnAgentWithNoIdentityIsNotNamedAsSomeoneElse(t *testing.T) {
	tool := NewSessionStart(func(context.Context) string { return t.TempDir() }, nil, nil, nil, func() string { return "" }, nil).
		WithSelfIdentity(func() string { return "the-connections-name" }).
		WithSelfSession(func() string { return "the-connections-id" }).
		WithCallerIdentity(callerNames(nil), func(context.Context) string { return "" })
	out, err := tool.Execute(mcp.WithLogicalAgent(context.Background(), "stranger"), json.RawMessage(`{"detail":"brief"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(out, "the-connections-name") || strings.Contains(out, "Session:") {
		t.Errorf("a caller with no identity was handed the connection's:\n%s", out)
	}
}

// The linker is told who is calling, and what it answers reaches the packet: the
// resume wording, and the refusal when the id was not linked.
func TestSessionStart_LinkageIsAnsweredPerCaller(t *testing.T) {
	cases := []struct {
		name     string
		link     LinkResult
		contains []string
		avoids   []string
	}{
		{
			name:     "the owner that resumed the name only",
			link:     LinkResult{InheritedName: "wise-cobra", NewIdentity: true},
			contains: []string{"wise-cobra (you", "resumed; new internal identity", "not inherited"},
		},
		{
			name:     "the owner whose predecessor's threads followed",
			link:     LinkResult{InheritedName: "wise-cobra", ThreadsInherited: true},
			contains: []string{"resumed; the threads and mail bound to your predecessor session continue under this one"},
			avoids:   []string{"not inherited", "new internal identity"},
		},
		{
			name:     "the owner whose name was refused but whose threads followed",
			link:     LinkResult{ThreadsInherited: true},
			contains: []string{"the threads and mail bound to your predecessor session continue under this one"},
			avoids:   []string{"resumed"},
		},
		{
			name:   "a caller that resumed nothing",
			link:   LinkResult{},
			avoids: []string{"resumed", "NOTE: this connection is already linked"},
		},
		{
			name:     "a different conversation is told its id was not linked",
			link:     LinkResult{Unlinked: UnlinkedOtherConversation},
			contains: []string{"already linked to a different conversation", "was not linked"},
			avoids:   []string{"resumed"},
		},
		{
			name:     "a stamp that contradicts the id",
			link:     LinkResult{Unlinked: UnlinkedStampMismatch},
			contains: []string{"names a different conversation than the identity stamped on this call"},
		},
		{
			name:     "an unattributable call",
			link:     LinkResult{Unlinked: UnlinkedAnonymous},
			contains: []string{"carried no agent identity", "will not guess"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var gotCtx context.Context
			tool := NewSessionStart(func(context.Context) string { return t.TempDir() }, nil, nil, nil, func() string { return "" }, nil).
				WithCallerIdentity(func(context.Context) string { return "wise-cobra" }, func(context.Context) string { return "ad50278a-rest" }).
				WithLinkage(func(ctx context.Context, id string) LinkResult {
					gotCtx = ctx
					return c.link
				})
			ctx := mcp.WithLogicalAgent(context.Background(), "the-stamp")
			out, err := tool.Execute(ctx, json.RawMessage(`{"session_id":"conv-x","detail":"full"}`))
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if gotCtx == nil || mcp.LogicalAgentFromCtx(gotCtx) != "the-stamp" {
				t.Errorf("the linker was not handed the call's stamped identity: %v", gotCtx)
			}
			for _, want := range c.contains {
				if !strings.Contains(out, want) {
					t.Errorf("packet does not say %q:\n%s", want, out)
				}
			}
			for _, avoid := range c.avoids {
				if strings.Contains(out, avoid) {
					t.Errorf("packet says %q, which this outcome does not establish:\n%s", avoid, out)
				}
			}
		})
	}
}

// The linker sees the per-call identity, not the one the session_id argument
// declares: an unstamped call that merely typed an id must reach it as unstamped.
func TestSessionStart_LinkerSeesTheStampNotTheTypedID(t *testing.T) {
	var seen string
	tool := NewSessionStart(func(context.Context) string { return t.TempDir() }, nil, nil, nil, func() string { return "" }, nil).
		WithDeclaredAgent(func(ctx context.Context, id string) context.Context { return mcp.WithLogicalAgent(ctx, id) }).
		WithLinkage(func(ctx context.Context, _ string) LinkResult {
			seen = mcp.LogicalAgentFromCtx(ctx)
			return LinkResult{}
		})
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"session_id":"typed-id","detail":"brief"}`)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if seen != "" {
		t.Errorf("the linker was handed %q for an unstamped call: the declared id leaked into the per-call identity", seen)
	}
}

// The legacy caller-blind linker keeps working for a connection with nothing to
// tell its callers apart by.
func TestSessionStart_WithExternalIDStillResumes(t *testing.T) {
	tool := NewSessionStart(func(context.Context) string { return t.TempDir() }, nil, nil, nil, func() string { return "" }, nil).
		WithSelfIdentity(func() string { return "old-owl" }).
		WithSelfSession(func() string { return "id-1234567890" }).
		WithExternalID(func(id string) string { return "old-owl" })
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"session_id":"conv","detail":"full"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "old-owl (you, id id-12345…) — resumed") {
		t.Errorf("the caller-blind linker's name was not reported as a resume:\n%s", out)
	}
}

// A caller with no name must not be handed the connection's: the per-call
// resolver's answer is final, an empty one included. Every mail tool and the
// commit trailer read through these.
func TestCollabDeps_APerCallAnswerIsFinal(t *testing.T) {
	d := CollabDeps{
		SessionName:    func() string { return "connection-name" },
		SessionID:      func() string { return "connection-id" },
		SessionNameFor: func(context.Context) string { return "" },
		SessionIDFor:   func(context.Context) string { return "" },
	}
	if got := d.sessionName(context.Background()); got != "" {
		t.Errorf("an agent with no name was handed %q", got)
	}
	if got := d.sessionID(context.Background()); got != "" {
		t.Errorf("an agent with no ID was handed %q", got)
	}
	// With no per-call resolver wired, the connection's answer is the only one.
	d.SessionNameFor, d.SessionIDFor = nil, nil
	if d.sessionName(context.Background()) != "connection-name" || d.sessionID(context.Background()) != "connection-id" {
		t.Error("a connection with no per-call resolver lost its own identity")
	}
}

// Inherited predecessors are asked per call, and the answer is the caller's.
func TestCollabDeps_InheritedIDsAreAskedPerCall(t *testing.T) {
	d := CollabDeps{InheritedSessionIDs: func(ctx context.Context) []string {
		if mcp.LogicalAgentFromCtx(ctx) == "owner" {
			return []string{"predecessor"}
		}
		return nil
	}}
	if got := d.inheritedIDs(mcp.WithLogicalAgent(context.Background(), "owner")); fmt.Sprint(got) != "[predecessor]" {
		t.Errorf("the owner's inherited IDs = %v", got)
	}
	if got := d.inheritedIDs(mcp.WithLogicalAgent(context.Background(), "subagent")); len(got) != 0 {
		t.Errorf("a subagent was handed %v", got)
	}
	if got := (CollabDeps{}).inheritedIDs(context.Background()); got != nil {
		t.Errorf("an unwired resolver answered %v", got)
	}
}
