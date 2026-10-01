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
		WithLSPDiagMode(func(got string) string { note("diagmode", got); return "" })
	for _, detail := range []string{"full", "brief"} {
		if _, err := tool.Execute(context.Background(), json.RawMessage(`{"detail":"`+detail+`"}`)); err != nil {
			t.Fatalf("Execute %s: %v", detail, err)
		}
	}
	for _, what := range []string{"gowork", "warming", "diagmode"} {
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
