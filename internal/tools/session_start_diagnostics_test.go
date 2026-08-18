// Diagnostics rendering in the session_start orientation packet: the
// staleness note's cold-module-cache partition (gopls reports every go.mod
// package as missing until the cache warms, which would otherwise bury real
// errors) and its unit-level partition function.
// Split out of session_start_test.go, which had grown past the test-file size cap.
package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

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
				func(context.Context) string { return t.TempDir() },
				&stubDiagnostics{all: tc.diags},
				nil,
				nil,
				func() string { return "" },
				nil,
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
			realDiags, coldCount := partitionColdCacheGoMod(tc.input)
			if coldCount != tc.wantColdCount {
				t.Errorf("coldCount: want %d got %d", tc.wantColdCount, coldCount)
			}
			if len(tc.wantRealURIs) == 0 {
				if len(realDiags) != 0 {
					t.Errorf("want empty real map, got %v", realDiags)
				}
				return
			}
			for _, uri := range tc.wantRealURIs {
				if _, ok := realDiags[uri]; !ok {
					t.Errorf("want URI %q in real map, got keys %v", uri, mapKeys(realDiags))
				}
			}
			if len(realDiags) != len(tc.wantRealURIs) {
				t.Errorf("real map len: want %d got %d (keys %v)", len(tc.wantRealURIs), len(realDiags), mapKeys(realDiags))
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
