package tools_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/tools"
)

func TestWorkspaceSymbolsXcodeProofRequiresSwiftResult(t *testing.T) {
	proofs := 0
	mock := &mockLSP{wsSymbols: []protocol.SymbolInformation{{
		Name:     "App",
		Location: protocol.Location{URI: "file:///tmp/App.go"},
	}}}
	tool := tools.NewWorkspaceSymbols(mock, nil, time.Minute, 0, nil).WithXcodeProof(func() { proofs++ })
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"query":"App"}`)); err != nil {
		t.Fatal(err)
	}
	if proofs != 0 {
		t.Fatalf("non-Swift workspace result produced %d proof callback(s)", proofs)
	}
}

func TestXcodeProofCallbacksRequireNonEmptyLSPResults(t *testing.T) {
	location := protocol.Location{URI: "file:///tmp/App.swift"}
	symbol := protocol.SymbolInformation{
		Name:     "App",
		Location: protocol.Location{URI: "file:///tmp/App.swift"},
	}

	tests := []struct {
		name string
		run  func(*mockLSP, func()) error
	}{
		{
			name: "workspace symbols",
			run: func(mock *mockLSP, proof func()) error {
				tool := tools.NewWorkspaceSymbols(mock, nil, time.Minute, 0, nil).WithXcodeProof(proof)
				_, err := tool.Execute(context.Background(), json.RawMessage(`{"query":"App"}`))
				return err
			},
		},
		{
			name: "definition",
			run: func(mock *mockLSP, proof func()) error {
				tool := tools.NewGetDefinition(mock, nil, time.Minute, 0).WithXcodeProof(proof)
				_, err := tool.Execute(context.Background(), json.RawMessage(`{"uri":"file:///tmp/App.swift","line":0,"character":0}`))
				return err
			},
		},
		{
			name: "references",
			run: func(mock *mockLSP, proof func()) error {
				tool := tools.NewFindReferences(mock, nil, time.Minute, 0).WithXcodeProof(proof)
				_, err := tool.Execute(context.Background(), json.RawMessage(`{"uri":"file:///tmp/App.swift","line":0,"character":0}`))
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proofs := 0
			full := &mockLSP{wsSymbols: []protocol.SymbolInformation{symbol}, locations: []protocol.Location{location}}
			if err := tt.run(full, func() { proofs++ }); err != nil {
				t.Fatal(err)
			}
			if proofs != 1 {
				t.Fatalf("proof callbacks = %d, want 1", proofs)
			}

			proofs = 0
			if err := tt.run(&mockLSP{}, func() { proofs++ }); err != nil {
				t.Fatal(err)
			}
			if proofs != 0 {
				t.Fatalf("empty result produced %d proof callback(s)", proofs)
			}
		})
	}
}
