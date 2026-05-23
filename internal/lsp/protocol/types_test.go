package protocol

import (
	"encoding/json"
	"testing"
)

// TextDocumentSync in ServerCapabilities is `TextDocumentSyncOptions |
// TextDocumentSyncKind` per the LSP spec. Pyright returns the bare-number
// form, which previously failed to decode.
func TestServerCapabilities_TextDocumentSync(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		wantChange TextDocumentSyncKind
		wantOpen   bool
	}{
		{
			name:       "object form",
			raw:        `{"textDocumentSync":{"openClose":true,"change":2}}`,
			wantChange: SyncIncremental,
			wantOpen:   true,
		},
		{
			name:       "bare number form (pyright)",
			raw:        `{"textDocumentSync":2}`,
			wantChange: SyncIncremental,
			wantOpen:   false,
		},
		{
			name:       "bare number with whitespace",
			raw:        `{"textDocumentSync": 1 }`,
			wantChange: SyncFull,
			wantOpen:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var caps ServerCapabilities
			if err := json.Unmarshal([]byte(tt.raw), &caps); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if caps.TextDocumentSync == nil {
				t.Fatal("TextDocumentSync is nil")
			}
			if caps.TextDocumentSync.Change != tt.wantChange {
				t.Errorf("Change = %d, want %d", caps.TextDocumentSync.Change, tt.wantChange)
			}
			if caps.TextDocumentSync.OpenClose != tt.wantOpen {
				t.Errorf("OpenClose = %v, want %v", caps.TextDocumentSync.OpenClose, tt.wantOpen)
			}
		})
	}
}
