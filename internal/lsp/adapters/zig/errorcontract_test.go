package zig_test

import (
	"testing"

	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/adapters/zig"
	"github.com/plumbkit/plumb/internal/lsp/conformance"
	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
)

// TestErrorContract pins the "zls <label>: <cause>" strings this adapter wraps
// every transport failure in. They are what plumb surfaces to agents and
// nothing else asserts them, so this is the net under any refactor that moves
// the wrapping elsewhere. The harness writes the named document to disk itself
// (see conformance.RunErrorContract).
func TestErrorContract(t *testing.T) {
	conformance.RunErrorContract(t,
		func(c jsonrpc.Caller) lsp.Client { return zig.New(c) },
		zig.DefaultInitParams, "zls", "main.zig", "pub fn main() void {}\n")
}

// TestLazyOpenErrorContract pins "zls open <uri>: <cause>", the label this
// adapter emits when the document it opens lazily is not readable.
// TestErrorContract cannot reach it: it hands the adapter a real file on
// purpose, so the request labels rather than this one are what fail there.
func TestLazyOpenErrorContract(t *testing.T) {
	conformance.RunLazyOpenErrorContract(t,
		func(c jsonrpc.Caller) lsp.Client { return zig.New(c) },
		"zls", "absent.zig")
}
