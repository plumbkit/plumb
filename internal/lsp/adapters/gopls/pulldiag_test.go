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

// A user-configured [lsp.go] initialization_options table replaces the typed
// goplsOptions default with a map. Pull mode must still inject the
// experimental pullDiagnostics flag — into a clone, never the user's map —
// and must honour an explicit user-set value either way.
func TestAdapter_EnablePullDiagnostics_UserOptionsMap(t *testing.T) {
	userOpts := map[string]any{"staticcheck": true}
	params := gopls.DefaultInitParams("file:///tmp/x")
	params.InitializationOptions = userOpts

	(&gopls.Adapter{}).EnablePullDiagnostics(&params)

	got, ok := params.InitializationOptions.(map[string]any)
	if !ok {
		t.Fatalf("init options type = %T, want map[string]any", params.InitializationOptions)
	}
	if got["pullDiagnostics"] != true {
		t.Errorf("pullDiagnostics = %v, want true", got["pullDiagnostics"])
	}
	if got["staticcheck"] != true {
		t.Errorf("user key staticcheck = %v, want preserved true", got["staticcheck"])
	}
	if _, mutated := userOpts["pullDiagnostics"]; mutated {
		t.Error("user's original options map was mutated — the flag must be injected into a clone")
	}

	// An explicit user choice wins, even pullDiagnostics = false.
	optOut := map[string]any{"pullDiagnostics": false}
	params2 := gopls.DefaultInitParams("file:///tmp/x")
	params2.InitializationOptions = optOut
	(&gopls.Adapter{}).EnablePullDiagnostics(&params2)
	got2, ok := params2.InitializationOptions.(map[string]any)
	if !ok {
		t.Fatalf("init options type = %T, want map[string]any", params2.InitializationOptions)
	}
	if got2["pullDiagnostics"] != false {
		t.Errorf("explicit pullDiagnostics=false was overridden: got %v", got2["pullDiagnostics"])
	}
}
