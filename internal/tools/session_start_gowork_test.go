package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
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
			fn   func() string
			on   bool
		}{
			{"switched off", func() string { return work }, true},
			{"left alone", func() string { return "" }, false},
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
