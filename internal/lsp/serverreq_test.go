package lsp_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/lsp/watcher"
)

// registerParams is a client/registerCapability params blob registering one
// workspace/didChangeWatchedFiles watcher for Go files, keyed as "watch-1".
const registerParams = `{"registrations":[{"id":"watch-1","method":"workspace/didChangeWatchedFiles","registerOptions":{"watchers":[{"globPattern":"**/*.go"}]}}]}`

// unregisterParams removes the "watch-1" registration made by registerParams.
const unregisterParams = `{"unregistrations":[{"id":"watch-1"}]}`

// filterIsActive reports whether f has recorded watcher patterns: with the
// registerParams glob in place only the .go event survives filtering; with no
// patterns recorded both events pass through unchanged.
func filterIsActive(f *watcher.Filter) bool {
	events := []protocol.FileEvent{
		{URI: "file:///p/main.go", Type: protocol.FileChanged},
		{URI: "file:///p/notes.txt", Type: protocol.FileChanged},
	}
	return len(f.FilterEvents(events)) == 1
}

func TestHandleServerRequest_RegisterCapability(t *testing.T) {
	var f watcher.Filter
	result, err := lsp.HandleServerRequest(&f, protocol.MethodRegisterCapability, json.RawMessage(registerParams))
	if err != nil {
		t.Fatalf("registerCapability: unexpected error: %v", err)
	}
	if result != nil {
		t.Fatalf("registerCapability: got result %v, want nil", result)
	}
	if !filterIsActive(&f) {
		t.Fatal("registerCapability did not record the watcher patterns")
	}
}

func TestHandleServerRequest_UnregisterCapability(t *testing.T) {
	var f watcher.Filter
	if _, err := lsp.HandleServerRequest(&f, protocol.MethodRegisterCapability, json.RawMessage(registerParams)); err != nil {
		t.Fatalf("registerCapability: unexpected error: %v", err)
	}
	result, err := lsp.HandleServerRequest(&f, protocol.MethodUnregisterCapability, json.RawMessage(unregisterParams))
	if err != nil {
		t.Fatalf("unregisterCapability: unexpected error: %v", err)
	}
	if result != nil {
		t.Fatalf("unregisterCapability: got result %v, want nil", result)
	}
	if filterIsActive(&f) {
		t.Fatal("unregisterCapability did not remove the watcher patterns")
	}
}

func TestHandleServerRequest_UnknownMethod(t *testing.T) {
	var f watcher.Filter
	result, err := lsp.HandleServerRequest(&f, "workspace/configuration", nil)
	if result != nil {
		t.Fatalf("unknown method: got result %v, want nil", result)
	}
	var mnf *jsonrpc.MethodNotFoundError
	if !errors.As(err, &mnf) {
		t.Fatalf("unknown method: got error %T (%v), want *jsonrpc.MethodNotFoundError", err, err)
	}
	if mnf.Method != "workspace/configuration" {
		t.Fatalf("MethodNotFoundError carries method %q, want %q", mnf.Method, "workspace/configuration")
	}
}
