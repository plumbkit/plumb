package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// The per-call identity channel is the ONLY channel the shared-connection write
// gate reads (internal/cli/conn_logical_agent.go refuse). A client whose runtime
// drops the stamp — observed on `local-agent-mode-plumb`, which forwards only
// declared argument keys — declares its identity through session_start fine and
// still has every state-changing call refused, with a remedy line telling it to
// install a hook it already has. These tests pin the disclosure that turns that
// mid-session refusal into an orientation-time fact (PLAN-440 acceptance b).

func TestStampChannelNoteSilentWhenChannelLive(t *testing.T) {
	var s SessionStart
	s.WithStampChannel(func(context.Context) StampChannelState {
		return StampChannelState{Shared: true, PerCallStamped: true}
	})
	if got := s.stampChannelNote(context.Background()); got != "" {
		t.Fatalf("a stamped call must produce no note, got %q", got)
	}
}

func TestStampChannelNoteWarnsWhenSharedAndUnstamped(t *testing.T) {
	var s SessionStart
	s.WithStampChannel(func(context.Context) StampChannelState {
		return StampChannelState{Shared: true, PerCallStamped: false}
	})
	got := s.stampChannelNote(context.Background())
	if got == "" {
		t.Fatal("a shared connection with no per-call stamp must warn; got no note")
	}
	// Every remedy that can apply: the hook for Claude Code (which, since the
	// connector declares plumb_agent, now works on Claude desktop too), _meta,
	// and the transport remedy. Never "type plumb_agent yourself": without the
	// hook an invented id is admitted as a fresh agent on the connection root.
	if !strings.Contains(got, "plumb hooks install") {
		t.Errorf("note must name the hook: %q", got)
	}
	if strings.Contains(got, "plumb_agent") {
		t.Errorf("note must not invite a model to type plumb_agent: %q", got)
	}
	if !strings.Contains(got, "one plumb serve per") {
		t.Errorf("note must still name the transport remedy, got %q", got)
	}
}

func TestStampChannelNoteWarnsBeforeTheConnectionIsShared(t *testing.T) {
	var s SessionStart
	s.WithStampChannel(func(context.Context) StampChannelState {
		return StampChannelState{Shared: false, PerCallStamped: false, HookClient: true}
	})
	got := s.stampChannelNote(context.Background())
	if got == "" {
		t.Fatal("acceptance (b): the gap must be discoverable BEFORE a second agent attaches")
	}
	if strings.Contains(got, "are being refused") {
		t.Errorf("a sole agent is not being refused yet; note must state a future cost: %q", got)
	}
	// No "never locked out" promise any more: once a second agent attaches,
	// unstamped writes are refused whether or not anyone has stamped before.
	if !strings.Contains(got, "once a second agent attaches") || strings.Contains(got, "plumb_agent") {
		t.Errorf("the dormant note must state when refusals start, and not invite typing plumb_agent: %q", got)
	}
}

func TestStampChannelNoteSilentWhenUnwired(t *testing.T) {
	var s SessionStart
	if got := s.stampChannelNote(context.Background()); got != "" {
		t.Fatalf("unwired accessor must stay silent, got %q", got)
	}
}

// check_messages is gated on a shared connection because delivery is
// exactly-once. session_start performs the identical claim and cannot be gated,
// or identity becomes undeclarable — so the claim itself has to honour the same
// rule, otherwise the gate blocks the legitimate read while orientation keeps
// consuming other agents' mail.
func TestMailIsNotClaimableByAnUnattributableCallerOnASharedConnection(t *testing.T) {
	var s SessionStart
	s.WithStampChannel(func(context.Context) StampChannelState {
		return StampChannelState{Shared: true, PerCallStamped: false}
	})
	if s.mailClaimable(context.Background()) {
		t.Error("the caller check_messages would refuse must not consume mail through session_start either")
	}
}

func TestMailStaysClaimableForEveryCallerTheGateAdmits(t *testing.T) {
	cases := map[string]StampChannelState{
		"identified on a shared connection": {Shared: true, PerCallStamped: true},
		"sole agent, unstamped":             {Shared: false, PerCallStamped: false},
		"sole agent, stamped":               {Shared: false, PerCallStamped: true},
	}
	for name, st := range cases {
		t.Run(name, func(t *testing.T) {
			var s SessionStart
			s.WithStampChannel(func(context.Context) StampChannelState { return st })
			if !s.mailClaimable(context.Background()) {
				t.Error("a caller the write gate admits must still receive its mail")
			}
		})
	}
}

func TestMailIsClaimableWhenTheChannelAccessorIsUnwired(t *testing.T) {
	var s SessionStart
	if !s.mailClaimable(context.Background()) {
		t.Error("an unwired accessor must not silently stop mail delivery")
	}
}

// TestStampChannelNote_ReadsThePerCallIdentityNotTheDeclaredOne goes through
// Execute with both channels wired the way the daemon wires them. The hook adds
// session_id to every session_start, and the declared-agent channel puts it on
// the ctx; the note used to read that ctx, so it reported the per-call channel
// live on exactly the client that drops it (Claude desktop's connector, where
// an unattributed edit then landed in another checkout). The note must read the
// identity the call carried per call, before session_id is applied.
func TestStampChannelNote_ReadsThePerCallIdentityNotTheDeclaredOne(t *testing.T) {
	ws := t.TempDir()
	tool := NewSessionStart(func(context.Context) string { return ws }, nil, nil, nil, func() string { return "" }, nil).
		WithDeclaredAgent(func(ctx context.Context, id string) context.Context {
			return context.WithValue(ctx, declaredAgentKeyType{}, id)
		}).
		WithStampChannel(func(ctx context.Context) StampChannelState {
			id, _ := ctx.Value(declaredAgentKeyType{}).(string)
			return StampChannelState{Shared: true, PerCallStamped: id != ""}
		})

	out, err := tool.Execute(context.Background(), json.RawMessage(`{"session_id":"conv-1","detail":"brief"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, stampChannelRefusedNotice) {
		t.Fatalf("an unstamped session_start that only declared session_id must get the notice:\n%s", out)
	}

	stamped := context.WithValue(context.Background(), declaredAgentKeyType{}, "conv-1")
	out, err = tool.Execute(stamped, json.RawMessage(`{"session_id":"conv-1","detail":"brief"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(out, "NOTE: state-changing") || strings.Contains(out, "carried no per-call") {
		t.Fatalf("control: a stamped call must get no notice:\n%s", out)
	}
}

// A client the hook does not stamp (Codex, Gemini, …) on a single-agent
// connection gets no dormant notice: it would repeat on every session_start
// with a remedy that does not apply. A refusal is still reported to anyone.
func TestStampChannelNoteSilentForNonHookClientUntilShared(t *testing.T) {
	var s SessionStart
	st := StampChannelState{}
	s.WithStampChannel(func(context.Context) StampChannelState { return st })
	if got := s.stampChannelNote(context.Background()); got != "" {
		t.Errorf("a single-agent non-hook client got a notice: %q", got)
	}
	st.Shared = true
	if got := s.stampChannelNote(context.Background()); got != stampChannelRefusedNotice {
		t.Errorf("a refused non-hook client must be told: %q", got)
	}
}

// TestStampChannelNotices_PointAnInstalledHookAtItsDiagnosis: a caller whose
// hook IS installed but whose call still arrived unstamped (a daemon too old to
// accept the key Claude desktop's connector passes through) must not be sent to
// install it again. Both notices name `plumb hooks`, which checks the hook and
// the daemon it talks to.
//
// They must not promise it says WHY: with a current hook and a daemon that
// accepts its stamp, bare `plumb hooks` has nothing to report (see
// TestIdentityHookSkewNote in internal/cli), and a promised diagnosis that does
// not come sends the reader round in a circle again.
func TestStampChannelNotices_PointAnInstalledHookAtItsDiagnosis(t *testing.T) {
	for name, notice := range map[string]string{"refused": stampChannelRefusedNotice, "dormant": stampChannelDormantNotice} {
		if !strings.Contains(notice, "plumb hooks install claude-code") {
			t.Errorf("%s notice lost the install remedy: %q", name, notice)
		}
		if !strings.Contains(notice, "if the hook is installed, `plumb hooks` checks it and the daemon.") {
			t.Errorf("%s notice does not point an installed hook at `plumb hooks`: %q", name, notice)
		}
		if strings.Contains(notice, "says why") {
			t.Errorf("%s notice promises a diagnosis bare `plumb hooks` does not always give: %q", name, notice)
		}
	}
}
