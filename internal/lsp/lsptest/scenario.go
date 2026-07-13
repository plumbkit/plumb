// Package lsptest provides a deterministic, scenario-driven fake LSP server at
// the adapter's jsonrpc.Caller boundary. It proves Plumb's protocol behavior;
// real language-server binaries remain the final validation gate.
package lsptest

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

type DiagnosticsMode string

const (
	Push DiagnosticsMode = "push"
	Pull DiagnosticsMode = "pull"
)

// Scenario is the portable contract exercised against an adapter.
type Scenario struct {
	Name          string
	RootURI       string
	DocumentURI   string
	LanguageID    string
	Source        string
	Mode          DiagnosticsMode
	Diagnostic    protocol.Diagnostic
	RegisterWatch bool
}

// Caller is a fake server exposed through the adapter-facing Caller interface.
type Caller struct {
	mu        sync.Mutex
	calls     []jsonrpc.RecordedCall
	onNotify  func(string, json.RawMessage)
	onRequest jsonrpc.RequestHandler
	scenario  Scenario
}

func NewCaller(s Scenario) *Caller { return &Caller{scenario: s} }

func (c *Caller) Call(_ context.Context, method string, params, result any) error {
	p, err := json.Marshal(params)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.calls = append(c.calls, jsonrpc.RecordedCall{Method: method, Params: p})
	c.mu.Unlock()
	var response any
	switch method {
	case protocol.MethodInitialize:
		caps := protocol.ServerCapabilities{}
		if c.scenario.Mode == Pull {
			caps.DiagnosticProvider = &protocol.BoolOrOptions{Enabled: true}
		}
		response = protocol.InitializeResult{Capabilities: caps}
	case protocol.MethodDocumentSymbols:
		response = []protocol.DocumentSymbol{}
	case protocol.MethodWorkspaceSymbols:
		response = []protocol.SymbolInformation{}
	case protocol.MethodDefinition, protocol.MethodReferences:
		response = []protocol.Location{}
	case protocol.MethodHover:
		response = nil
	case protocol.MethodDiagnostic:
		response = protocol.DocumentDiagnosticReport{Kind: protocol.DiagnosticReportFull, ResultID: "scenario-1", Items: []protocol.Diagnostic{c.scenario.Diagnostic}}
	case protocol.MethodShutdown:
		response = nil
	default:
		return fmt.Errorf("lsptest: unexpected request %q", method)
	}
	if result != nil && response != nil {
		b, err := json.Marshal(response)
		if err != nil {
			return err
		}
		return json.Unmarshal(b, result)
	}
	return nil
}

func (c *Caller) Notify(_ context.Context, method string, params any) error {
	p, err := json.Marshal(params)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.calls = append(c.calls, jsonrpc.RecordedCall{Method: method, Params: p})
	c.mu.Unlock()
	return nil
}

func (c *Caller) SetNotificationHandler(fn func(string, json.RawMessage)) {
	c.mu.Lock()
	c.onNotify = fn
	c.mu.Unlock()
}

func (c *Caller) SetRequestHandler(fn jsonrpc.RequestHandler) {
	c.mu.Lock()
	c.onRequest = fn
	c.mu.Unlock()
}

func (c *Caller) Close() error { return nil }

func (c *Caller) Calls() []jsonrpc.RecordedCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]jsonrpc.RecordedCall(nil), c.calls...)
}

// PushDiagnostics delivers a server notification to the adapter subscriber.
func (c *Caller) PushDiagnostics() error {
	c.mu.Lock()
	fn := c.onNotify
	c.mu.Unlock()
	if fn == nil {
		return fmt.Errorf("lsptest: adapter did not install a notification handler")
	}
	p, err := json.Marshal(protocol.PublishDiagnosticsParams{URI: c.scenario.DocumentURI, Diagnostics: []protocol.Diagnostic{c.scenario.Diagnostic}})
	if err != nil {
		return err
	}
	fn(protocol.MethodPublishDiagnostics, p)
	return nil
}

// RegisterWatcher simulates the dynamic watcher request used by Kotlin and
// several other servers and returns the adapter's response.
func (c *Caller) RegisterWatcher(ctx context.Context) error {
	c.mu.Lock()
	fn := c.onRequest
	c.mu.Unlock()
	if fn == nil {
		return fmt.Errorf("lsptest: adapter did not install a request handler")
	}
	params := json.RawMessage(`{"registrations":[{"id":"watch-1","method":"workspace/didChangeWatchedFiles","registerOptions":{"watchers":[{"globPattern":"**/*.kt"}]}}]}`)
	_, err := fn(ctx, protocol.MethodRegisterCapability, params)
	return err
}
