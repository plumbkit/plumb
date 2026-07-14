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

// DocumentDiagnosticParams uses the singular previousResultId (per document,
// per the LSP 3.17 DocumentDiagnosticParams shape); verify the wire tag
// exactly since it is easy to confuse with the plural workspace-level
// previousResultIds used by WorkspaceDiagnosticParams below.
func TestDocumentDiagnosticParams_Encode(t *testing.T) {
	params := DocumentDiagnosticParams{
		TextDocument:     TextDocumentIdentifier{URI: "file:///p/a.go"},
		PreviousResultID: "r1",
	}
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(raw)
	if !strings.Contains(s, `"previousResultId":"r1"`) {
		t.Errorf("expected singular previousResultId tag: %s", s)
	}
	if strings.Contains(s, `"previousResultIds"`) {
		t.Errorf("document-level params must not use the plural workspace tag: %s", s)
	}
}

// workspace/diagnostic requests carry previousResultIds (plural), one entry
// per URI the client already knows a result ID for — distinct from the
// document-level request's singular previousResultId.
func TestWorkspaceDiagnosticParams_Encode(t *testing.T) {
	params := WorkspaceDiagnosticParams{
		PreviousResultIDs: []PreviousResultID{
			{URI: "file:///p/a.go", Value: "r1"},
		},
	}
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(raw)
	if !strings.Contains(s, `"previousResultIds":[{"uri":"file:///p/a.go","value":"r1"}]`) {
		t.Errorf("previousResultIds round-trip mismatch: %s", s)
	}
	if strings.Contains(s, `"identifier"`) {
		t.Errorf("identifier should be omitted when empty: %s", s)
	}
	if strings.Contains(s, `"partialResultToken"`) {
		t.Errorf("partialResultToken should be omitted when unset: %s", s)
	}

	var decoded WorkspaceDiagnosticParams
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(decoded.PreviousResultIDs) != 1 ||
		decoded.PreviousResultIDs[0].URI != "file:///p/a.go" ||
		decoded.PreviousResultIDs[0].Value != "r1" {
		t.Errorf("round-trip mismatch: %#v", decoded.PreviousResultIDs)
	}
}

// workspace/diagnostic's result batches per-document reports; each carries
// its own uri/version alongside the familiar full/unchanged report shape.
// Cover every legal form: full-with-items, full-with-empty-items, unchanged
// (required resultId, no items), a mixed batch, and a missing/null version.
func TestWorkspaceDiagnosticReport_Decode(t *testing.T) {
	t.Run("full with items and version", testWorkspaceReportFullWithItemsAndVersion)
	t.Run("full with empty items array and null version", testWorkspaceReportFullEmptyItemsNullVersion)
	t.Run("unchanged requires resultId and carries no items", testWorkspaceReportUnchangedRequiresResultID)
	t.Run("mixed batch of full and unchanged", testWorkspaceReportMixedBatch)
	t.Run("missing version field decodes to nil", testWorkspaceReportMissingVersion)
}

// Each of the following holds one subtest body of
// TestWorkspaceDiagnosticReport_Decode as its own named function — kept out
// of the parent test to stay under the gocyclo limit while still asserting
// every field for every legal report form.

func testWorkspaceReportFullWithItemsAndVersion(t *testing.T) {
	raw := `{"items":[{"kind":"full","resultId":"r1","items":[{"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":1}},"severity":1,"message":"boom"}],"uri":"file:///p/a.go","version":3}]}`
	var r WorkspaceDiagnosticReport
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(r.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(r.Items))
	}
	item := r.Items[0]
	if item.Kind != DiagnosticReportFull || item.URI != "file:///p/a.go" {
		t.Fatalf("unexpected item: %#v", item)
	}
	if item.Version == nil || *item.Version != 3 {
		t.Fatalf("expected version 3, got %v", item.Version)
	}
	if len(item.Items) != 1 || item.Items[0].Message != "boom" {
		t.Fatalf("unexpected diagnostics: %#v", item.Items)
	}
}

func testWorkspaceReportFullEmptyItemsNullVersion(t *testing.T) {
	raw := `{"items":[{"kind":"full","items":[],"uri":"file:///p/b.go","version":null}]}`
	var r WorkspaceDiagnosticReport
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	item := r.Items[0]
	if item.Kind != DiagnosticReportFull {
		t.Errorf("Kind = %q, want full", item.Kind)
	}
	if len(item.Items) != 0 {
		t.Errorf("expected no items, got %d", len(item.Items))
	}
	if item.Version != nil {
		t.Errorf("expected nil version for null, got %v", *item.Version)
	}
}

func testWorkspaceReportUnchangedRequiresResultID(t *testing.T) {
	raw := `{"items":[{"kind":"unchanged","resultId":"r1","uri":"file:///p/c.go","version":5}]}`
	var r WorkspaceDiagnosticReport
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	item := r.Items[0]
	if item.Kind != DiagnosticReportUnchanged {
		t.Errorf("Kind = %q, want unchanged", item.Kind)
	}
	if item.ResultID != "r1" {
		t.Errorf("ResultID = %q, want r1", item.ResultID)
	}
	if len(item.Items) != 0 {
		t.Errorf("unchanged report must carry no items, got %#v", item.Items)
	}
}

func testWorkspaceReportMixedBatch(t *testing.T) {
	raw := `{"items":[` +
		`{"kind":"full","items":[{"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":1}},"severity":2,"message":"warn"}],"uri":"file:///p/d.go","version":1},` +
		`{"kind":"unchanged","resultId":"r2","uri":"file:///p/e.go","version":2}` +
		`]}`
	var r WorkspaceDiagnosticReport
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(r.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(r.Items))
	}
	if r.Items[0].Kind != DiagnosticReportFull || r.Items[1].Kind != DiagnosticReportUnchanged {
		t.Fatalf("unexpected kinds: %q, %q", r.Items[0].Kind, r.Items[1].Kind)
	}
}

func testWorkspaceReportMissingVersion(t *testing.T) {
	raw := `{"items":[{"kind":"full","items":[],"uri":"file:///p/f.go"}]}`
	var r WorkspaceDiagnosticReport
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if r.Items[0].Version != nil {
		t.Errorf("expected nil version when field absent, got %v", *r.Items[0].Version)
	}
}

// DiagnosticOptions exposes the identifier/interFileDependencies/
// workspaceDiagnostics detail instead of reducing diagnosticProvider to a
// bool, for callers that need to know whether the server also supports
// workspace/diagnostic (DiagnosticOptions.WorkspaceDiagnostics).
func TestServerCapabilities_DiagnosticOptions(t *testing.T) {
	t.Run("bare true yields enabled with nil options", testDiagnosticOptionsBareTrue)
	t.Run("options object populates fields", testDiagnosticOptionsObjectPopulatesFields)
	t.Run("absent provider is disabled", testDiagnosticOptionsAbsentIsDisabled)
	t.Run("bare false is disabled", testDiagnosticOptionsBareFalseIsDisabled)
}

// Each of the following holds one subtest body of
// TestServerCapabilities_DiagnosticOptions as its own named function — kept
// out of the parent test to stay under the gocyclo limit.

func testDiagnosticOptionsBareTrue(t *testing.T) {
	var caps ServerCapabilities
	if err := json.Unmarshal([]byte(`{"diagnosticProvider":true}`), &caps); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	opts, enabled := caps.DiagnosticOptions()
	if !enabled {
		t.Fatal("expected enabled")
	}
	if opts != nil {
		t.Errorf("expected nil options for bool-form provider, got %#v", opts)
	}
}

func testDiagnosticOptionsObjectPopulatesFields(t *testing.T) {
	var caps ServerCapabilities
	raw := `{"diagnosticProvider":{"identifier":"gopls","interFileDependencies":true,"workspaceDiagnostics":true}}`
	if err := json.Unmarshal([]byte(raw), &caps); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	opts, enabled := caps.DiagnosticOptions()
	if !enabled {
		t.Fatal("expected enabled")
	}
	if opts == nil {
		t.Fatal("expected populated options")
	}
	if opts.Identifier != "gopls" || !opts.InterFileDependencies || !opts.WorkspaceDiagnostics {
		t.Errorf("unexpected options: %#v", opts)
	}
}

func testDiagnosticOptionsAbsentIsDisabled(t *testing.T) {
	var caps ServerCapabilities
	if err := json.Unmarshal([]byte(`{}`), &caps); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	opts, enabled := caps.DiagnosticOptions()
	if enabled {
		t.Error("expected disabled")
	}
	if opts != nil {
		t.Errorf("expected nil options when disabled, got %#v", opts)
	}
}

func testDiagnosticOptionsBareFalseIsDisabled(t *testing.T) {
	var caps ServerCapabilities
	if err := json.Unmarshal([]byte(`{"diagnosticProvider":false}`), &caps); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	opts, enabled := caps.DiagnosticOptions()
	if enabled {
		t.Error("expected disabled")
	}
	if opts != nil {
		t.Errorf("expected nil options when disabled, got %#v", opts)
	}
}

// DiagnosticWorkspaceClientCapabilities plumbs workspace.diagnostics.refreshSupport
// (workspace/diagnostic/refresh) as a type. It is NOT wired into
// DefaultClientCapabilities in this task — advertising it is a later task's
// job once something actually handles the refresh request.
func TestDiagnosticWorkspaceClientCapabilities_Encode(t *testing.T) {
	wc := WorkspaceClientCapabilities{
		Diagnostics: &DiagnosticWorkspaceClientCapabilities{RefreshSupport: true},
	}
	raw, err := json.Marshal(wc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"diagnostics":{"refreshSupport":true}`) {
		t.Errorf("unexpected encoding: %s", raw)
	}

	if caps := DefaultClientCapabilities(); caps.Workspace.Diagnostics != nil {
		t.Error("workspace diagnostics refresh capability must not be advertised yet")
	}
}

// The two workspace/diagnostic method constants must match the LSP 3.17
// wire method names exactly.
func TestWorkspaceDiagnosticMethodConstants(t *testing.T) {
	if MethodWorkspaceDiagnostic != "workspace/diagnostic" {
		t.Errorf("MethodWorkspaceDiagnostic = %q, want workspace/diagnostic", MethodWorkspaceDiagnostic)
	}
	if MethodWorkspaceDiagnosticRefresh != "workspace/diagnostic/refresh" {
		t.Errorf("MethodWorkspaceDiagnosticRefresh = %q, want workspace/diagnostic/refresh", MethodWorkspaceDiagnosticRefresh)
	}
}
