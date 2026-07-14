package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

// ClientCapabilitiesFor(false) must be byte-for-byte the push-first default:
// publishDiagnostics advertised, the pull-diagnostics client capability absent.
// This is the dormant-by-default guarantee — a default (auto/push) setup must
// negotiate exactly today's capabilities.
func TestClientCapabilitiesFor_PushIsDefault(t *testing.T) {
	push := ClientCapabilitiesFor(false)
	if push.TextDocument.PublishDiagnostics == nil {
		t.Error("push caps must advertise publishDiagnostics")
	}
	if push.TextDocument.Diagnostic != nil {
		t.Error("push caps must NOT advertise the pull-diagnostics client capability")
	}

	// DefaultClientCapabilities is the safe push wrapper: it must equal the
	// builder's push form exactly.
	def, err := json.Marshal(DefaultClientCapabilities())
	if err != nil {
		t.Fatalf("marshal default: %v", err)
	}
	built, err := json.Marshal(push)
	if err != nil {
		t.Fatalf("marshal built: %v", err)
	}
	if string(def) != string(built) {
		t.Errorf("DefaultClientCapabilities must equal ClientCapabilitiesFor(false)\n default: %s\n built:   %s", def, built)
	}
}

// ClientCapabilitiesFor(true) additionally advertises the pull-diagnostics
// client capability (with related-document support) while keeping every push
// capability intact — pull is additive, not a replacement.
func TestClientCapabilitiesFor_PullAddsDiagnostic(t *testing.T) {
	pull := ClientCapabilitiesFor(true)
	if pull.TextDocument.PublishDiagnostics == nil {
		t.Error("pull caps must still advertise publishDiagnostics (push stays ingested in every mode)")
	}
	if pull.TextDocument.Diagnostic == nil {
		t.Fatal("pull caps must advertise the textDocument/diagnostic client capability")
	}
	if !pull.TextDocument.Diagnostic.RelatedDocumentSupport {
		t.Error("pull caps must declare relatedDocumentSupport")
	}
	// The watched-files dynamic registration (load-bearing for gopls) must be
	// unchanged in pull mode.
	if pull.Workspace.DidChangeWatchedFiles == nil || !pull.Workspace.DidChangeWatchedFiles.DynamicRegistration {
		t.Error("pull caps must preserve didChangeWatchedFiles.dynamicRegistration")
	}

	raw, err := json.Marshal(pull)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"diagnostic"`) {
		t.Errorf("serialised pull capabilities missing the diagnostic key: %s", raw)
	}
}
