package protocol

import (
	"encoding/json"
	"strings"
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

// plumb must advertise textDocument.publishDiagnostics (typescript-language-server
// stays silent without it) but must NOT advertise the pull-diagnostics client
// capability (nothing consumes pull; advertising it risks a dual-mode server
// going pull-only and never pushing).
func TestDefaultClientCapabilities_Diagnostics(t *testing.T) {
	caps := DefaultClientCapabilities()
	if caps.TextDocument.PublishDiagnostics == nil {
		t.Error("publishDiagnostics capability must be advertised so servers push diagnostics")
	}
	if caps.TextDocument.Diagnostic != nil {
		t.Error("pull-diagnostics client capability must NOT be advertised while the tool consumes only push")
	}

	// It must also serialise with the publishDiagnostics key present and no
	// diagnostic key.
	raw, err := json.Marshal(caps)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(raw)
	if !strings.Contains(s, `"publishDiagnostics"`) {
		t.Errorf("serialised capabilities missing publishDiagnostics: %s", s)
	}
	if strings.Contains(s, `"diagnostic"`) {
		t.Errorf("serialised capabilities unexpectedly advertise pull diagnostic: %s", s)
	}
}

// diagnosticProvider in ServerCapabilities is `boolean | DiagnosticOptions |
// DiagnosticRegistrationOptions`. typescript-language-server ≥ 5.3 returns the
// options-object form; PullDiagnosticsEnabled must recognise all variants.
func TestServerCapabilities_PullDiagnostics(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want bool
	}{
		{name: "options object (typescript-language-server)", raw: `{"diagnosticProvider":{"interFileDependencies":true,"workspaceDiagnostics":false}}`, want: true},
		{name: "bare true", raw: `{"diagnosticProvider":true}`, want: true},
		{name: "bare false", raw: `{"diagnosticProvider":false}`, want: false},
		{name: "absent (push-only server)", raw: `{}`, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var caps ServerCapabilities
			if err := json.Unmarshal([]byte(tt.raw), &caps); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got := caps.PullDiagnosticsEnabled(); got != tt.want {
				t.Errorf("PullDiagnosticsEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

// The textDocument/diagnostic result is a union of "full" and "unchanged"
// reports, optionally carrying related-document diagnostics.
func TestDocumentDiagnosticReport_Decode(t *testing.T) {
	t.Run("full with items", func(t *testing.T) {
		raw := `{"kind":"full","resultId":"a1","items":[{"range":{"start":{"line":1,"character":0},"end":{"line":1,"character":5}},"severity":1,"message":"boom"}]}`
		var r DocumentDiagnosticReport
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if r.Kind != DiagnosticReportFull {
			t.Errorf("Kind = %q, want full", r.Kind)
		}
		if len(r.Items) != 1 || r.Items[0].Severity != SevError || r.Items[0].Message != "boom" {
			t.Fatalf("unexpected items: %#v", r.Items)
		}
	})

	t.Run("unchanged carries only resultId", func(t *testing.T) {
		raw := `{"kind":"unchanged","resultId":"a1"}`
		var r DocumentDiagnosticReport
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if r.Kind != DiagnosticReportUnchanged {
			t.Errorf("Kind = %q, want unchanged", r.Kind)
		}
		if len(r.Items) != 0 {
			t.Errorf("expected no items, got %d", len(r.Items))
		}
	})

	t.Run("related documents", func(t *testing.T) {
		raw := `{"kind":"full","items":[],"relatedDocuments":{"file:///p/other.ts":{"kind":"full","items":[{"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":1}},"severity":2,"message":"unused"}]}}}`
		var r DocumentDiagnosticReport
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		other, ok := r.RelatedDocuments["file:///p/other.ts"]
		if !ok {
			t.Fatal("missing related document")
		}
		if len(other.Items) != 1 || other.Items[0].Severity != SevWarning {
			t.Fatalf("unexpected related items: %#v", other.Items)
		}
	})
}
