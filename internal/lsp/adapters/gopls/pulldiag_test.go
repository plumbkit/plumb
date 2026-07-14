package gopls_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/adapters/gopls"
)

// The gopls adapter satisfies lsp.PullInitializer, so the pool can opt it into
// the pull model when the resolved diagnostics mode is "pull".
func TestAdapter_ImplementsPullInitializer(t *testing.T) {
	var _ lsp.PullInitializer = (*gopls.Adapter)(nil)
}

// EnablePullDiagnostics reconfigures params for the pull model: it advertises
// the textDocument/diagnostic client capability AND sets gopls's experimental
// "pullDiagnostics" initialization option. It must not disturb the existing
// push capability.
func TestAdapter_EnablePullDiagnostics(t *testing.T) {
	params := gopls.DefaultInitParams("file:///tmp/x")

	// Baseline: the default params advertise no pull capability and no
	// pullDiagnostics option.
	if params.Capabilities.TextDocument.Diagnostic != nil {
		t.Fatal("default gopls params must not advertise the pull capability")
	}

	(&gopls.Adapter{}).EnablePullDiagnostics(&params)

	if params.Capabilities.TextDocument.Diagnostic == nil {
		t.Error("EnablePullDiagnostics must advertise the pull client capability")
	}
	if params.Capabilities.TextDocument.PublishDiagnostics == nil {
		t.Error("EnablePullDiagnostics must keep the push capability (pull is additive)")
	}

	raw, err := json.Marshal(params.InitializationOptions)
	if err != nil {
		t.Fatalf("marshal init options: %v", err)
	}
	if !strings.Contains(string(raw), `"pullDiagnostics":true`) {
		t.Errorf("initializationOptions must inject pullDiagnostics:true, got %s", raw)
	}
}
