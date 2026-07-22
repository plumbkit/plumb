package cli

import (
	"reflect"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/lsp/adapters/gopls"
	"github.com/plumbkit/plumb/internal/lsp/adapters/zig"
	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
)

// TestInitParamsFor_ZigVerbatimWhenConfigured verifies a configured
// [lsp.zig] initialization_options table reaches the zig init params verbatim.
func TestInitParamsFor_ZigVerbatimWhenConfigured(t *testing.T) {
	ad := zig.New(jsonrpc.NewMockCaller())
	opts := map[string]any{
		"enable_build_on_save": true,
		"build_on_save_step":   "check",
	}
	lspCfg := config.LSPConfig{Command: "zls", InitializationOptions: opts}

	params := initParamsFor(ad, "zig", "file:///project", diagModePush, lspCfg)
	if !reflect.DeepEqual(params.InitializationOptions, opts) {
		t.Fatalf("InitializationOptions = %#v, want verbatim %#v", params.InitializationOptions, opts)
	}
}

// TestInitParamsFor_ZigUnchangedWhenUnconfigured verifies that with no
// initialization_options configured, the zig init params are byte-identical to
// the adapter's own DefaultInitParams — the "never default-on" guarantee.
func TestInitParamsFor_ZigUnchangedWhenUnconfigured(t *testing.T) {
	ad := zig.New(jsonrpc.NewMockCaller())
	const rootURI = "file:///project"

	got := initParamsFor(ad, "zig", rootURI, diagModePush, config.LSPConfig{Command: "zls"})
	want := zig.DefaultInitParams(rootURI)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unconfigured zig params diverged from DefaultInitParams:\n got=%#v\nwant=%#v", got, want)
	}
	if got.InitializationOptions != nil {
		t.Errorf("InitializationOptions = %#v, want nil (no options sent by default)", got.InitializationOptions)
	}
}

// TestInitParamsFor_NonZigAdapterUnaffected verifies the generic overlay is a
// no-op for another adapter when unconfigured: gopls keeps its own typed default
// initialization options untouched.
func TestInitParamsFor_NonZigAdapterUnaffected(t *testing.T) {
	ad := gopls.New(jsonrpc.NewMockCaller())
	const rootURI = "file:///project"

	got := initParamsFor(ad, "go", rootURI, diagModePush, config.LSPConfig{Command: "gopls"})
	want := gopls.DefaultInitParams(rootURI)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unconfigured go params diverged from gopls.DefaultInitParams:\n got=%#v\nwant=%#v", got, want)
	}
	if got.InitializationOptions == nil {
		t.Error("gopls InitializationOptions is nil; the overlay must not clobber the adapter's typed default")
	}
}
