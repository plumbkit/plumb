package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Both orientation renders name the go.work a Go language server was started
// with GOWORK=off against (#521), so an agent in a worktree can tell why the
// server answers about the worktree; and neither says anything when the server
// runs with the environment it inherited.
func TestSessionStart_GoWorkOffLine(t *testing.T) {
	const work = "/src/go.work"
	const want = "Go LSP:   runs with GOWORK=off — /src/go.work lists another copy of this module"
	for _, detail := range []string{"full", "brief"} {
		for _, tc := range []struct {
			name string
			fn   func(string) string
			on   bool
		}{
			{"switched off", func(string) string { return work }, true},
			{"left alone", func(string) string { return "" }, false},
			{"not wired", nil, false},
		} {
			t.Run(detail+"/"+tc.name, func(t *testing.T) {
				ws := briefGitInit(t)
				tool := NewSessionStart(func(context.Context) string { return ws }, &stubDiagnostics{}, nil, nil,
					func() string { return "claude-code" }, nil).
					WithLSPLanguage(func() string { return "go" }).
					WithLSPGoWorkOff(tc.fn)
				out, err := tool.Execute(context.Background(), json.RawMessage(`{"detail":"`+detail+`"}`))
				if err != nil {
					t.Fatalf("Execute: %v", err)
				}
				if got := strings.Contains(out, want); got != tc.on {
					t.Fatalf("GOWORK=off line present = %v, want %v:\n%s", got, tc.on, out)
				}
				if !tc.on && strings.Contains(out, "GOWORK") {
					t.Fatalf("a server left alone must not be described as GOWORK=off:\n%s", out)
				}
			})
		}
	}
}

// TestSessionStart_LSPStateAsksAboutTheCallersWorkspace is issue #546 item 3 at
// the tool boundary: the GOWORK, warm-up and diagnostics-mode accessors are
// asked about the workspace session_start resolved for the CALLER, the one its
// other lines describe, so a per-agent pin gets its own server's state.
func TestSessionStart_LSPStateAsksAboutTheCallersWorkspace(t *testing.T) {
	ws := briefGitInit(t)
	asked := map[string][]string{}
	note := func(what, got string) { asked[what] = append(asked[what], got) }
	tool := NewSessionStart(func(context.Context) string { return ws }, &stubDiagnostics{}, nil, nil,
		func() string { return "claude-code" }, nil).
		WithLSPLanguage(func() string { return "go" }).
		WithLSPGoWorkOff(func(got string) string { note("gowork", got); return "" }).
		WithLSPWarmup(func(got string) (bool, time.Duration) { note("warming", got); return false, 0 }).
		WithLSPDiagMode(func(got string) string { note("diagmode", got); return "" }).
		WithLSPServer(func(got string) (string, bool) { note("server", got); return "go", true })
	for _, detail := range []string{"full", "brief"} {
		if _, err := tool.Execute(context.Background(), json.RawMessage(`{"detail":"`+detail+`"}`)); err != nil {
			t.Fatalf("Execute %s: %v", detail, err)
		}
	}
	for _, what := range []string{"gowork", "warming", "diagmode", "server"} {
		if len(asked[what]) == 0 {
			t.Errorf("%s: never asked", what)
		}
		for _, got := range asked[what] {
			if got != ws {
				t.Errorf("%s: asked about %q, want the caller's workspace %q", what, got, ws)
			}
		}
	}
}

// TestSessionStart_ServerNotStartedYet is the PR #559 review B2 at the tool
// boundary: when no language server has started for the caller's workspace (a
// subagent's first session_start in a worktree), the packet says so instead of
// "LSP is ready" — which described the connection's server — and names the
// go.work the server WILL start with GOWORK=off against.
func TestSessionStart_ServerNotStartedYet(t *testing.T) {
	ws := briefGitInit(t)
	newTool := func(started bool) *SessionStart {
		return NewSessionStart(func(context.Context) string { return ws }, &stubDiagnostics{}, nil, nil,
			func() string { return "claude-code" }, nil).
			WithLSPLanguage(func() string { return "go" }). // the CONNECTION's server is attached
			WithLSPServer(func(string) (string, bool) { return "go", started }).
			WithLSPGoWorkOff(func(string) string { return "/src/go.work" }).
			WithLSPDiagMode(func(string) string { return "pull" })
	}
	out, err := newTool(false).Execute(context.Background(), json.RawMessage(`{"detail":"full"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, want := range []string{
		"Go LSP:   will start with GOWORK=off — /src/go.work lists another copy of this module",
		"The Go language server for this workspace has not started yet",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("not-started packet lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "LSP is ready") {
		t.Errorf("a server that has not started must not be reported ready:\n%s", out)
	}

	out, err = newTool(true).Execute(context.Background(), json.RawMessage(`{"detail":"full"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, want := range []string{"Go LSP:   runs with GOWORK=off", "LSP is ready (diagnostics: pull)"} {
		if !strings.Contains(out, want) {
			t.Errorf("started packet lacks %q:\n%s", want, out)
		}
	}
}
