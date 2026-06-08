package cli

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/golimpio/plumb/internal/cache"
	"github.com/golimpio/plumb/internal/lsp/protocol"
)

func TestURIUnderRoot(t *testing.T) {
	cases := []struct {
		name, uri, root string
		want            bool
	}{
		{"exact match", "file:///proj", "/proj", true},
		{"direct child", "file:///proj/main.go", "/proj", true},
		{"nested child", "file:///proj/pkg/sub/x.go", "/proj", true},
		{"sibling prefix is not under root", "file:///projector/main.go", "/proj", false},
		{"path without file:// prefix", "/proj/main.go", "/proj", true},
		{"completely unrelated", "file:///other/main.go", "/proj", false},
		{"parent of root is not under it", "file:///", "/proj", false},
		{"trailing slash root child", "file:///proj/", "/proj", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := uriUnderRoot(tc.uri, tc.root); got != tc.want {
				t.Errorf("uriUnderRoot(%q, %q) = %v, want %v", tc.uri, tc.root, got, tc.want)
			}
		})
	}
}

// pushDiag is the cli-package equivalent of the tools test helper; it injects
// a publishDiagnostics notification into an Invalidator without the JSON-RPC
// plumbing.
func pushDiag(t *testing.T, inv *cache.Invalidator, uri string, diags []protocol.Diagnostic) {
	t.Helper()
	b, err := json.Marshal(protocol.PublishDiagnosticsParams{URI: uri, Diagnostics: diags})
	if err != nil {
		t.Fatal(err)
	}
	inv.Handle(protocol.MethodPublishDiagnostics, b)
}

// TestRoutingInvProxy_AllDiagnostics_FiltersOutOfRoot is the 0.8.12 regression
// guard. gopls can publish diagnostics for URIs it transitively analyses
// (dependency packages, stdlib, files outside the workspace). The primary
// invalidator stores them all; AllDiagnostics() must filter them down to the
// workspace root so a no-args `diagnostics` call cannot report errors from
// files the user never wrote.
func TestRoutingInvProxy_AllDiagnostics_FiltersOutOfRoot(t *testing.T) {
	c := cache.New(time.Hour)
	defer c.Close()
	inv := cache.NewInvalidator(c)

	const root = "/proj"
	inRoot := "file://" + root + "/main.go"
	outOfRoot := "file:///some/dependency/x.go"
	pushDiag(t, inv, inRoot, []protocol.Diagnostic{
		{Severity: protocol.SevError, Message: "in-workspace error"},
	})
	pushDiag(t, inv, outOfRoot, []protocol.Diagnostic{
		{Severity: protocol.SevError, Message: "transitive analysis error"},
	})

	rp := newRoutingInvProxy(newTestPool())
	rp.setPrimary(root, "go", inv)

	all := rp.AllDiagnostics()
	if _, ok := all[inRoot]; !ok {
		t.Errorf("AllDiagnostics dropped in-root URI %q", inRoot)
	}
	if _, ok := all[outOfRoot]; ok {
		t.Errorf("AllDiagnostics leaked out-of-root URI %q (%d entries total)", outOfRoot, len(all))
	}
	if len(all) != 1 {
		t.Errorf("AllDiagnostics returned %d entries; want 1", len(all))
	}
}

// TestRoutingInvProxy_AllDiagnosticTimes_FiltersOutOfRoot mirrors the above
// guard for timestamps — the staleness annotation in `diagnostics` uses these,
// so a leak here would re-surface out-of-root entries via the "modified after
// analysis" path.
func TestRoutingInvProxy_AllDiagnosticTimes_FiltersOutOfRoot(t *testing.T) {
	c := cache.New(time.Hour)
	defer c.Close()
	inv := cache.NewInvalidator(c)

	const root = "/proj"
	inRoot := "file://" + root + "/main.go"
	outOfRoot := "file:///some/dependency/x.go"
	pushDiag(t, inv, inRoot, []protocol.Diagnostic{{Severity: protocol.SevError, Message: "x"}})
	pushDiag(t, inv, outOfRoot, []protocol.Diagnostic{{Severity: protocol.SevError, Message: "y"}})

	rp := newRoutingInvProxy(newTestPool())
	rp.setPrimary(root, "go", inv)

	times := rp.AllDiagnosticTimes()
	if _, ok := times[inRoot]; !ok {
		t.Errorf("AllDiagnosticTimes dropped in-root URI %q", inRoot)
	}
	if _, ok := times[outOfRoot]; ok {
		t.Errorf("AllDiagnosticTimes leaked out-of-root URI %q", outOfRoot)
	}
}

// TestRoutingInvProxy_AllDiagnostics_NoPrimary verifies the nil-safe path:
// before setPrimary runs, the proxy must not panic.
func TestRoutingInvProxy_AllDiagnostics_NoPrimary(t *testing.T) {
	rp := newRoutingInvProxy(newTestPool())
	if got := rp.AllDiagnostics(); got != nil {
		t.Errorf("AllDiagnostics without primary: got %v, want nil", got)
	}
	if got := rp.AllDiagnosticTimes(); got != nil {
		t.Errorf("AllDiagnosticTimes without primary: got %v, want nil", got)
	}
}
