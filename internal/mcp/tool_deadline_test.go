package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// fakeExec is the shared Execute body for the deadline test tools: it records
// whether ctx carries a deadline, optionally sleeps (respecting cancellation),
// then returns "ok".
func fakeExec(ctx context.Context, sleep time.Duration, hadDeadline *bool) (string, error) {
	if hadDeadline != nil {
		_, ok := ctx.Deadline()
		*hadDeadline = ok
	}
	if sleep > 0 {
		select {
		case <-time.After(sleep):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return "ok", nil
}

// boundedFake opts into the execution deadline via ExecTimeoutBounded.
type boundedFake struct {
	sleep       time.Duration
	hadDeadline *bool
}

func (*boundedFake) Name() string                 { return "bounded_fake" }
func (*boundedFake) Description() string          { return "bounded fake" }
func (*boundedFake) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (*boundedFake) ExecTimeoutBounded()          {}
func (f *boundedFake) Execute(ctx context.Context, _ json.RawMessage) (string, error) {
	return fakeExec(ctx, f.sleep, f.hadDeadline)
}

// plainFake does NOT implement ExecTimeoutBounded, so the dispatcher must never
// bound or inject a deadline into it.
type plainFake struct {
	sleep       time.Duration
	hadDeadline *bool
}

func (*plainFake) Name() string                 { return "plain_fake" }
func (*plainFake) Description() string          { return "plain fake" }
func (*plainFake) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (f *plainFake) Execute(ctx context.Context, _ json.RawMessage) (string, error) {
	return fakeExec(ctx, f.sleep, f.hadDeadline)
}

func newTestServer(execTimeout time.Duration) *Server {
	s := New(ServerInfo{Name: "test", Version: "0"})
	s.ToolExecTimeout = execTimeout
	return s
}

func TestExecTool_BoundedSlowToolTimesOut(t *testing.T) {
	s := newTestServer(20 * time.Millisecond)
	start := time.Now()
	_, err := s.execTool(context.Background(), &boundedFake{sleep: 2 * time.Second}, "bounded_fake", nil)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected a deadline error, got nil")
	}
	if !strings.Contains(err.Error(), "execution deadline") {
		t.Fatalf("error = %q, want it to mention the execution deadline", err)
	}
	// The dispatcher must return at ~the deadline, not wait out the 2s tool.
	if elapsed > 500*time.Millisecond {
		t.Fatalf("execTool took %v — it should return at the deadline, not block on the tool", elapsed)
	}
}

func TestExecTool_FastToolReturnsResult(t *testing.T) {
	s := newTestServer(time.Second)
	out, err := s.execTool(context.Background(), &boundedFake{}, "bounded_fake", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "ok" {
		t.Fatalf("out = %q, want %q", out, "ok")
	}
}

func TestExecTool_InjectsDeadlineForBoundedTool(t *testing.T) {
	s := newTestServer(time.Second)
	var had bool
	if _, err := s.execTool(context.Background(), &boundedFake{hadDeadline: &had}, "bounded_fake", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !had {
		t.Fatal("a bounded tool should receive the injected execution deadline")
	}
}

func TestExecTool_ZeroTimeoutDisablesBound(t *testing.T) {
	s := newTestServer(0)
	var had bool
	out, err := s.execTool(context.Background(), &boundedFake{hadDeadline: &had}, "bounded_fake", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "ok" {
		t.Fatalf("out = %q, want %q", out, "ok")
	}
	if had {
		t.Fatal("with ToolExecTimeout=0 the tool must run inline with no injected deadline")
	}
}

func TestExecTool_UnboundedToolBypassesDeadline(t *testing.T) {
	s := newTestServer(10 * time.Millisecond)
	var had bool
	// The tool sleeps longer than the timeout but, not being opted in, must run
	// to completion uncancelled and never see an injected deadline.
	out, err := s.execTool(context.Background(), &plainFake{sleep: 50 * time.Millisecond, hadDeadline: &had}, "plain_fake", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "ok" {
		t.Fatalf("out = %q, want %q", out, "ok")
	}
	if had {
		t.Fatal("an unopted tool must not receive an injected deadline")
	}
}

func TestExecTool_ParentDeadlineNotShortened(t *testing.T) {
	s := newTestServer(10 * time.Millisecond)
	// The parent already bounds the work with a generous deadline; execTool must
	// defer to it rather than imposing its own shorter 10ms bound.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := s.execTool(ctx, &boundedFake{sleep: 30 * time.Millisecond}, "bounded_fake", nil)
	if err != nil {
		t.Fatalf("unexpected error (the tool's 30ms run should fit the parent's 5s): %v", err)
	}
	if out != "ok" {
		t.Fatalf("out = %q, want %q", out, "ok")
	}
}
