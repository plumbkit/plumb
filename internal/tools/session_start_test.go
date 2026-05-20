package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/golimpio/plumb/internal/lsp/protocol"
)

// stubDiagnostics implements diagnosticsSource for tests.
type stubDiagnostics struct {
	all map[string][]protocol.Diagnostic
}

func (s *stubDiagnostics) Diagnostics(uri string) []protocol.Diagnostic     { return s.all[uri] }
func (s *stubDiagnostics) AllDiagnostics() map[string][]protocol.Diagnostic { return s.all }

func makeDiag(line, col uint32, msg string, sev protocol.DiagnosticSeverity) protocol.Diagnostic {
	return protocol.Diagnostic{
		Range:    protocol.Range{Start: protocol.Position{Line: line, Character: col}},
		Message:  msg,
		Severity: sev,
	}
}

func TestSessionStart_ColdCacheGoModDiagnostics(t *testing.T) {
	coldMsg := func(pkg string) protocol.Diagnostic {
		return makeDiag(0, 0, pkg+" is not in your go.mod file", protocol.SevError)
	}
	realMsg := makeDiag(24, 0, "could not import modernc.org/sqlite", protocol.SevError)

	tests := []struct {
		name          string
		diags         map[string][]protocol.Diagnostic
		wantNote      bool
		wantNoteCount string
		wantReal      bool
	}{
		{
			name: "only cold-cache entries collapsed to note",
			diags: map[string][]protocol.Diagnostic{
				"file:///ws/go.mod": {coldMsg("github.com/a/b"), coldMsg("github.com/c/d")},
			},
			wantNote:      true,
			wantNoteCount: "2 go.mod",
			wantReal:      false,
		},
		{
			name: "real error in .go file preserved alongside note",
			diags: map[string][]protocol.Diagnostic{
				"file:///ws/go.mod":                     {coldMsg("github.com/a/b")},
				"file:///ws/internal/storage/sqlite.go": {realMsg},
			},
			wantNote:      true,
			wantNoteCount: "1 go.mod",
			wantReal:      true,
		},
		{
			name: "non-1:1 go.mod diagnostic treated as real",
			diags: map[string][]protocol.Diagnostic{
				"file:///ws/go.mod": {makeDiag(5, 0, "syntax error", protocol.SevError)},
			},
			wantNote: false,
			wantReal: true,
		},
		{
			name: "mixed go.mod: some cold-cache, some real",
			diags: map[string][]protocol.Diagnostic{
				"file:///ws/go.mod": {
					coldMsg("github.com/a/b"),
					makeDiag(5, 0, "syntax error", protocol.SevError),
				},
			},
			wantNote:      true,
			wantNoteCount: "1 go.mod",
			wantReal:      true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tool := NewSessionStart(
				func() string { return t.TempDir() },
				&stubDiagnostics{all: tc.diags},
				nil,
				nil,
				func() string { return "" },
			)
			out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}

			hasNote := strings.Contains(out, "cold module cache")
			if hasNote != tc.wantNote {
				t.Errorf("wantNote=%v got=%v\noutput:\n%s", tc.wantNote, hasNote, out)
			}
			if tc.wantNoteCount != "" && !strings.Contains(out, tc.wantNoteCount) {
				t.Errorf("want %q in output\noutput:\n%s", tc.wantNoteCount, out)
			}
			hasReal := strings.Contains(out, "could not import") || strings.Contains(out, "syntax error")
			if hasReal != tc.wantReal {
				t.Errorf("wantReal=%v got=%v\noutput:\n%s", tc.wantReal, hasReal, out)
			}
		})
	}
}

func TestSessionStart_ClientNameGuidance(t *testing.T) {
	// Verifies that Claude Code tool guidance is emitted for exact "claude-code"
	// and version-qualified "claude-code/<ver>" matches (case-insensitive),
	// but NOT for names that merely share the prefix (e.g. "claude-codegen").
	tests := []struct {
		name         string
		clientName   string
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
