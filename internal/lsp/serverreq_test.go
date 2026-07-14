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
	result, err := lsp.HandleServerRequest(&f, protocol.MethodRegisterCapability, json.RawMessage(registerParams), nil)
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
	if _, err := lsp.HandleServerRequest(&f, protocol.MethodRegisterCapability, json.RawMessage(registerParams), nil); err != nil {
		t.Fatalf("registerCapability: unexpected error: %v", err)
	}
	result, err := lsp.HandleServerRequest(&f, protocol.MethodUnregisterCapability, json.RawMessage(unregisterParams), nil)
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
	result, err := lsp.HandleServerRequest(&f, "workspace/configuration", nil, nil)
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

func TestHandleServerRequest_ExtensionHandlesMethod(t *testing.T) {
	var f watcher.Filter
	extra := func(method string, params json.RawMessage) (any, bool, error) {
		if method == "workspace/diagnostic/refresh" {
			return "refreshed", true, nil
		}
		return nil, false, nil
	}
	result, err := lsp.HandleServerRequest(&f, "workspace/diagnostic/refresh", nil, extra)
	if err != nil {
		t.Fatalf("extension-handled method: unexpected error: %v", err)
	}
	if result != "refreshed" {
		t.Fatalf("extension-handled method: got result %v, want %q", result, "refreshed")
	}
}

func TestHandleServerRequest_ExtensionErrorPropagates(t *testing.T) {
	var f watcher.Filter
	wantErr := errors.New("refresh failed")
	extra := func(_ string, _ json.RawMessage) (any, bool, error) {
		return nil, true, wantErr
	}
	result, err := lsp.HandleServerRequest(&f, "workspace/diagnostic/refresh", nil, extra)
	if result != nil {
		t.Fatalf("extension error: got result %v, want nil", result)
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("extension error: got %v, want %v", err, wantErr)
	}
}

func TestHandleServerRequest_ExtensionDeclinesUnknownMethod(t *testing.T) {
	var f watcher.Filter
	extra := func(_ string, _ json.RawMessage) (any, bool, error) {
		return nil, false, nil
	}
	_, err := lsp.HandleServerRequest(&f, "window/showMessageRequest", nil, extra)
	var mnf *jsonrpc.MethodNotFoundError
	if !errors.As(err, &mnf) {
		t.Fatalf("declined method: got error %T (%v), want *jsonrpc.MethodNotFoundError", err, err)
	}
	if mnf.Method != "window/showMessageRequest" {
		t.Fatalf("MethodNotFoundError carries method %q, want %q", mnf.Method, "window/showMessageRequest")
	}
}

func TestHandleServerRequest_RegistrationTakesPrecedenceOverExtension(t *testing.T) {
	var f watcher.Filter
	hookCalled := false
	extra := func(_ string, _ json.RawMessage) (any, bool, error) {
		hookCalled = true
		return "hijacked", true, nil
	}
	result, err := lsp.HandleServerRequest(&f, protocol.MethodRegisterCapability, json.RawMessage(registerParams), extra)
	if err != nil {
		t.Fatalf("registerCapability with extension: unexpected error: %v", err)
	}
	if result != nil {
		t.Fatalf("registerCapability with extension: got result %v, want nil", result)
	}
	if hookCalled {
		t.Fatal("extension hook was consulted for client/registerCapability — registration handling must stay built in")
	}
	if !filterIsActive(&f) {
		t.Fatal("registerCapability did not record the watcher patterns when an extension hook was supplied")
	}
}
