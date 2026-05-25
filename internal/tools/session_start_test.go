package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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

func TestPartitionColdCacheGoMod(t *testing.T) {
	cold := func(pkg string) protocol.Diagnostic {
		return makeDiag(0, 0, pkg+" is not in your go.mod file", protocol.SevError)
	}
	realDiag := makeDiag(5, 0, "syntax error", protocol.SevError)

	tests := []struct {
		name          string
		input         map[string][]protocol.Diagnostic
		wantColdCount int
		wantRealURIs  []string
	}{
		{
			name:          "empty input",
			input:         map[string][]protocol.Diagnostic{},
			wantColdCount: 0,
			wantRealURIs:  nil,
		},
		{
			name: "no go.mod URIs pass through unchanged",
			input: map[string][]protocol.Diagnostic{
				"file:///ws/main.go": {realDiag},
			},
			wantColdCount: 0,
			wantRealURIs:  []string{"file:///ws/main.go"},
		},
		{
			name: "all cold entries removed, count returned",
			input: map[string][]protocol.Diagnostic{
				"file:///ws/go.mod": {cold("github.com/a/b"), cold("github.com/c/d")},
			},
			wantColdCount: 2,
			wantRealURIs:  nil,
		},
		{
			name: "go.mod URI with only real diagnostic kept",
			input: map[string][]protocol.Diagnostic{
				"file:///ws/go.mod": {realDiag},
			},
			wantColdCount: 0,
			wantRealURIs:  []string{"file:///ws/go.mod"},
		},
		{
			name: "mixed go.mod: cold removed, real kept, count correct",
			input: map[string][]protocol.Diagnostic{
				"file:///ws/go.mod": {cold("github.com/a/b"), realDiag},
			},
			wantColdCount: 1,
			wantRealURIs:  []string{"file:///ws/go.mod"},
		},
		{
			name: "cold match requires position 0,0 — non-zero line not matched",
			input: map[string][]protocol.Diagnostic{
				"file:///ws/go.mod": {makeDiag(1, 0, "github.com/a/b is not in your go.mod file", protocol.SevError)},
			},
			wantColdCount: 0,
			wantRealURIs:  []string{"file:///ws/go.mod"},
		},
		{
			name: "cold match requires position 0,0 — non-zero col not matched",
			input: map[string][]protocol.Diagnostic{
				"file:///ws/go.mod": {makeDiag(0, 1, "github.com/a/b is not in your go.mod file", protocol.SevError)},
			},
			wantColdCount: 0,
			wantRealURIs:  []string{"file:///ws/go.mod"},
		},
		{
			name: "nested go.mod in submodule matched correctly",
			input: map[string][]protocol.Diagnostic{
				"file:///ws/sub/go.mod": {cold("github.com/a/b")},
			},
			wantColdCount: 1,
			wantRealURIs:  nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			real, coldCount := partitionColdCacheGoMod(tc.input)
			if coldCount != tc.wantColdCount {
				t.Errorf("coldCount: want %d got %d", tc.wantColdCount, coldCount)
			}
			if len(tc.wantRealURIs) == 0 {
				if len(real) != 0 {
					t.Errorf("want empty real map, got %v", real)
				}
				return
			}
			for _, uri := range tc.wantRealURIs {
				if _, ok := real[uri]; !ok {
					t.Errorf("want URI %q in real map, got keys %v", uri, mapKeys(real))
				}
			}
			if len(real) != len(tc.wantRealURIs) {
				t.Errorf("real map len: want %d got %d (keys %v)", len(tc.wantRealURIs), len(real), mapKeys(real))
			}
		})
	}
}

func mapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func TestSessionStart_RecommendedFirstStep(t *testing.T) {
	// writes a minimal go.mod so detectLanguage returns "Go" for the temp workspace.
	makeGoWorkspace := func(t *testing.T) string {
		t.Helper()
		ws := t.TempDir()
		if err := os.WriteFile(filepath.Join(ws, "go.mod"), []byte("module test\ngo 1.21\n"), 0o644); err != nil {
			t.Fatalf("write go.mod: %v", err)
		}
		return ws
	}

	t.Run("active errors suggest diagnostics", func(t *testing.T) {
		ws := makeGoWorkspace(t)
		diag := &stubDiagnostics{all: map[string][]protocol.Diagnostic{
			"file:///ws/main.go": {makeDiag(0, 0, "undefined: foo", protocol.SevError)},
		}}
		tool := NewSessionStart(func() string { return ws }, diag, nil, nil, func() string { return "" })
		out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if !strings.Contains(out, "Active errors detected") {
			t.Errorf("want 'Active errors detected' in output\n%s", out)
		}
	})

	t.Run("LSP available no errors suggests workspace_symbols", func(t *testing.T) {
		ws := makeGoWorkspace(t)
		diag := &stubDiagnostics{all: nil}
		tool := NewSessionStart(func() string { return ws }, diag, nil, nil, func() string { return "" })
		out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if !strings.Contains(out, "workspace_symbols") {
			t.Errorf("want 'workspace_symbols' in output\n%s", out)
		}
	})

	t.Run("no LSP with language suggests list_files", func(t *testing.T) {
		ws := makeGoWorkspace(t)
		tool := NewSessionStart(func() string { return ws }, nil, nil, nil, func() string { return "" })
		out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if !strings.Contains(out, "list_files") {
			t.Errorf("want 'list_files' in output\n%s", out)
		}
	})

	t.Run("no LSP no language uses default", func(t *testing.T) {
		ws := t.TempDir()
		tool := NewSessionStart(func() string { return ws }, nil, nil, nil, func() string { return "" })
		out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if !strings.Contains(out, "list_files") {
			t.Errorf("want 'list_files' in output\n%s", out)
		}
	})

	t.Run("warning-only diags still suggest workspace_symbols", func(t *testing.T) {
		ws := makeGoWorkspace(t)
		diag := &stubDiagnostics{all: map[string][]protocol.Diagnostic{
			"file:///ws/main.go": {makeDiag(1, 0, "unused variable", protocol.SevWarning)},
		}}
		tool := NewSessionStart(func() string { return ws }, diag, nil, nil, func() string { return "" })
		out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if !strings.Contains(out, "workspace_symbols") {
			t.Errorf("want 'workspace_symbols' (not error path) in output\n%s", out)
		}
	})
}

func TestSessionStart_WorkspaceScale(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "go.mod"), []byte("module test\ngo 1.21\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "util.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("write util.go: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "README.md"), []byte("# test\n"), 0o644); err != nil {
		t.Fatalf("write README.md: %v", err)
	}

	tool := NewSessionStart(func() string { return ws }, nil, nil, nil, func() string { return "" })
	out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// 4 files total, 2 Go (.go) — go.mod is not a .go file
	if !strings.Contains(out, "Scale:") {
		t.Errorf("want 'Scale:' in output\n%s", out)
	}
	if !strings.Contains(out, "Go") {
		t.Errorf("want 'Go' in Scale line\n%s", out)
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

// TestSessionStart_DesktopGuidance verifies the Desktop guidance fires for the
// real Claude Desktop client name ("claude-ai", not "claude-desktop") and
// leads with the workspace-pinning instruction.
func TestSessionStart_DesktopGuidance(t *testing.T) {
	for _, name := range []string{"claude-ai", "claude-ai/0.1.0", "claude-desktop"} {
		t.Run(name, func(t *testing.T) {
			tool := NewSessionStart(func() string { return t.TempDir() }, nil, nil, nil, func() string { return name })
			out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if !strings.Contains(out, "## Tool guidance (Claude Desktop)") {
				t.Errorf("clientName=%q: want Desktop guidance\n%s", name, out)
			}
			if !strings.Contains(out, "Pin your project first") {
				t.Errorf("clientName=%q: want workspace-pinning instruction\n%s", name, out)
			}
		})
	}
}

// TestSessionStart_WorkspaceResolution covers the resolution chain after the
// os.Getwd() phantom was removed: the daemon's attached root wins, an explicit
// arg is honoured only when nothing is attached, and nothing-resolves errors
// (no daemon-cwd guess).
func TestSessionStart_WorkspaceResolution(t *testing.T) {
	t.Run("mismatch between attached root and explicit arg returns error", func(t *testing.T) {
		attached := t.TempDir()
		tool := NewSessionStart(func() string { return attached }, nil, nil, nil, func() string { return "" })
		_, err := tool.Execute(context.Background(), json.RawMessage(`{"workspace":"/some/other/path"}`))
		if err == nil {
			t.Fatal("want mismatch error when explicit workspace differs from attached root, got nil")
		}
		if !strings.Contains(err.Error(), "already pinned") {
			t.Errorf("error should mention pinned workspace, got: %v", err)
		}
	})

	t.Run("attached root returned when explicit arg matches", func(t *testing.T) {
		attached := t.TempDir()
		tool := NewSessionStart(func() string { return attached }, nil, nil, nil, func() string { return "" })
		out, err := tool.Execute(context.Background(), json.RawMessage(`{"workspace":"`+attached+`"}`))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if !strings.Contains(out, "# Workspace: "+attached) {
			t.Errorf("attached root should be returned; want %q\n%s", attached, out)
		}
	})

	t.Run("explicit arg used when nothing attached", func(t *testing.T) {
		explicit := t.TempDir()
		tool := NewSessionStart(func() string { return "" }, nil, nil, nil, func() string { return "" })
		out, err := tool.Execute(context.Background(), json.RawMessage(`{"workspace":"`+explicit+`"}`))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if !strings.Contains(out, "# Workspace: "+explicit) {
			t.Errorf("explicit arg should resolve; want %q\n%s", explicit, out)
		}
	})

	t.Run("nothing resolves errors (no cwd guess)", func(t *testing.T) {
		tool := NewSessionStart(func() string { return "" }, nil, nil, nil, func() string { return "" })
		if _, err := tool.Execute(context.Background(), json.RawMessage(`{}`)); err == nil {
			t.Fatal("want noWorkspaceError when nothing resolves, got nil")
		}
	})
}
