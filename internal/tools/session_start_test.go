package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestSessionStart_ClientNameGuidance(t *testing.T) {
	// Verifies that Claude Code tool guidance is emitted for exact "claude-code"
	// and version-qualified "claude-code/<ver>" matches (case-insensitive),
	// but NOT for names that merely share the prefix (e.g. "claude-codegen").
	tests := []struct {
		name        string
		clientName  string
		wantGuidance bool
	}{
		{"exact lowercase", "claude-code", true},
		{"exact uppercase", "Claude-Code", true},
		{"mixed case", "CLAUDE-CODE", true},
		{"version qualified", "claude-code/1.2.3", true},
		{"version qualified mixed case", "Claude-Code/2.0.0", true},
		{"claude desktop", "claude-desktop", false},
		{"empty string", "", false},
		{"unrelated client", "vscode", false},
		{"prefix only similar", "claude", false},
		{"false positive guard", "claude-codegen", false},
		{"false positive guard mixed case", "Claude-Codegen", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			name := tc.clientName
			tool := NewSessionStart(
				func() string { return t.TempDir() },
				nil,
				nil,
				nil,
				func() string { return name },
			)

			out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}

			hasGuidance := strings.Contains(out, "## Tool guidance (Claude Code)")
			if hasGuidance != tc.wantGuidance {
				t.Errorf("clientName=%q: wantGuidance=%v got=%v\noutput:\n%s",
					tc.clientName, tc.wantGuidance, hasGuidance, out)
			}
		})
	}
}
