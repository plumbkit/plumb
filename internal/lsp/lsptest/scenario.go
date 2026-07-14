// Package lsptest provides a deterministic, scenario-driven fake LSP server at
// the adapter's jsonrpc.Caller boundary. It proves Plumb's protocol behavior;
// real language-server binaries remain the final validation gate.
package lsptest

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// DiagnosticsMode selects what the fake advertises and how it answers
// diagnostics-related methods: Push (publishDiagnostics notifications only),
// Pull (advertises diagnosticProvider and answers textDocument/diagnostic),
// or Hybrid (both — a server that pulls AND still pushes, the shape
// internal/cli's diagnosticsHybridFlip detects and records).
type DiagnosticsMode string

const (
	Push   DiagnosticsMode = "push"
	Pull   DiagnosticsMode = "pull"
	Hybrid DiagnosticsMode = "hybrid"
)

// Scenario is the portable contract exercised against an adapter. Every field
// beyond the original Name/RootURI/DocumentURI/LanguageID/Source/Mode/
// Diagnostic/RegisterWatch is an OPTIONAL growth axis: a conformance test
// leaves it zero-valued unless it specifically exercises that behaviour, so
// existing push-baseline scenarios (gopls/kotlin) need no changes.
type Scenario struct {
	Name          string
	RootURI       string
	DocumentURI   string
	LanguageID    string
	Source        string
	Mode          DiagnosticsMode
	Diagnostic    protocol.Diagnostic // back-compat: the scenario's sole/first diagnostic
	RegisterWatch bool

	// Diagnostics, when non-empty, is the full diagnostic set for
	// DocumentURI in place of the single Diagnostic field — proving a
	// caller handles more than one diagnostic per document. Read the
	// effective set via AllDiagnostics.
	Diagnostics []protocol.Diagnostic

	// DiagnosticOptions, meaningful only when Mode is Pull or Hybrid,
	// advertises the options-object form of diagnosticProvider
	// (identifier / interFileDependencies / workspaceDiagnostics) instead
	// of the bare `true`. Nil keeps today's shape
	// (`"diagnosticProvider":true`).
	DiagnosticOptions *protocol.DiagnosticOptions

	// PullReports scripts a sequence of textDocument/diagnostic responses
	// for DocumentURI: call N is served PullReports[N], clamped to the
	// final entry once the script is exhausted. Nil synthesises a single
	// stable full report (ResultID "scenario-1") from AllDiagnostics(),
	// degrading to "unchanged" ONLY when the incoming
	// DocumentDiagnosticParams.PreviousResultID matches that exact ID —
	// proving a caller (or a real server under the same contract) never
	// fabricates "nothing changed" for a stale or unknown ID.
	PullReports []protocol.DocumentDiagnosticReport

	// RelatedDocuments, when non-nil, decorates every FULL report served
	// for DocumentURI (scripted or synthesised, whichever does not
	// already carry its own RelatedDocuments) with these per-document
	// diagnostics.
	RelatedDocuments map[string]protocol.DocumentDiagnosticReport

	// WorkspaceReports scripts a sequence of workspace/diagnostic
	// responses, served in the same per-call sequence as PullReports. Nil
	// means the fake does not support workspace pulls at all: a call
	// answers method-not-found, matching most real servers (only gopls
	// implements WorkspaceDiagnostic today).
	WorkspaceReports []protocol.WorkspaceDiagnosticReport

	// MethodNotFound lists request methods that must answer with a
	// genuine *jsonrpc.MethodNotFoundError instead of their normal
	// handling — used to simulate a server that advertised a capability
	// at Initialize but does not actually implement it (e.g.
	// typescript-language-server's real -32601 on
	// textDocument/diagnostic), proving the -32601 downgrade path.
	MethodNotFound map[string]bool

	// Delay adds a fixed latency, keyed by method name, before the fake
	// answers a Call — proving a caller's timeout/cancellation handling.
	// The wait is context-aware: a cancelled/expired ctx returns ctx.Err()
	// immediately rather than waiting out the full delay.
	Delay map[string]time.Duration
}

// AllDiagnostics returns the diagnostics this scenario models for its primary
// document: Diagnostics when set, else the single back-compat Diagnostic.
// Exported so conformance assertions and the fake's own synthesis share one
// definition of "the scenario's diagnostics".
func (s Scenario) AllDiagnostics() []protocol.Diagnostic {
	if len(s.Diagnostics) > 0 {
		return s.Diagnostics
	}
	return []protocol.Diagnostic{s.Diagnostic}
}

// allowedNotificationMethods are the client-to-server notifications every
// adapter is expected to send as part of the standard LSP lifecycle this
// fake models. Anything else is recorded as unexpected (see
// Caller.UnexpectedNotifications) rather than rejected outright — Notify has
// no error return path a real server could use to signal rejection, so the
// harness fails the test AFTER the exchange instead (per the card's
// strictness contract: unexpected requests fail immediately with
// method-not-found; unexpected notifications are recorded and fail later).
var allowedNotificationMethods = map[string]bool{
	protocol.MethodInitialized:           true,
	protocol.MethodDidOpen:               true,
	protocol.MethodDidChange:             true,
	protocol.MethodDidClose:              true,
	protocol.MethodDidChangeWatchedFiles: true,
	protocol.MethodExit:                  true,
}

// Caller is a fake server exposed through the adapter-facing Caller interface.
type Caller struct {
	mu                      sync.Mutex
	calls                   []jsonrpc.RecordedCall
	unexpectedNotifications []string
	onNotify                func(string, json.RawMessage)
	onRequest               jsonrpc.RequestHandler
	scenario                Scenario
	pullCalls               int
	wsCalls                 int
}

func NewCaller(s Scenario) *Caller { return &Caller{scenario: s} }

func (c *Caller) Call(ctx context.Context, method string, params, result any) error {
	paramsBytes, err := json.Marshal(params)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.calls = append(c.calls, jsonrpc.RecordedCall{Method: method, Params: paramsBytes})
	c.mu.Unlock()

	if d, ok := c.scenario.Delay[method]; ok && d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if c.scenario.MethodNotFound[method] {
		return &jsonrpc.MethodNotFoundError{Method: method}
	}

	response, handled, err := c.buildResponse(method, paramsBytes, result)
	if handled {
		return err
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

// buildResponse dispatches method to its canned/scripted response. Methods
// whose handling already fully decodes into result (MethodDiagnostic,
// MethodWorkspaceDiagnostic — both need scenario-specific wire-shape care;
// see respondDiagnostic/respondWorkspaceDiagnostic) return handled=true with
// their own error; every other method returns handled=false and a plain Go
// value for Call's shared marshal/unmarshal step. An unrecognised method
// answers a proper method-not-found — the strictness contract for an
// unexpected request. Split out of Call purely to keep cyclomatic complexity
// down; no behavioural difference.
func (c *Caller) buildResponse(method string, paramsBytes []byte, result any) (response any, handled bool, err error) {
	switch method {
	case protocol.MethodInitialize:
		caps := protocol.ServerCapabilities{}
		if c.scenario.Mode == Pull || c.scenario.Mode == Hybrid {
			caps.DiagnosticProvider = c.diagnosticProviderCapability()
		}
		return protocol.InitializeResult{Capabilities: caps}, false, nil
	case protocol.MethodDocumentSymbols:
		return []protocol.DocumentSymbol{}, false, nil
	case protocol.MethodWorkspaceSymbols:
		return []protocol.SymbolInformation{}, false, nil
	case protocol.MethodDefinition, protocol.MethodReferences:
		return []protocol.Location{}, false, nil
	case protocol.MethodHover, protocol.MethodShutdown:
		return nil, false, nil
	case protocol.MethodDiagnostic:
		return nil, true, c.respondDiagnostic(paramsBytes, result)
	case protocol.MethodWorkspaceDiagnostic:
		return nil, true, c.respondWorkspaceDiagnostic(result)
	default:
		return nil, true, &jsonrpc.MethodNotFoundError{Method: method}
	}
}

// respondDiagnostic answers a textDocument/diagnostic call: decode the
// incoming params (best-effort — a malformed/absent params value degrades to
// the zero value, matching the existing pull-request tests that call with
// nil params), compute the report, and encode it via diagnosticReportJSON so
// a full report's items key survives even when empty. Split out of Call
// purely to keep its cyclomatic complexity down; no behavioural difference.
func (c *Caller) respondDiagnostic(paramsBytes []byte, result any) error {
	var p protocol.DocumentDiagnosticParams
	_ = json.Unmarshal(paramsBytes, &p)
	raw, err := diagnosticReportJSON(c.pullReport(p))
	if err != nil {
		return err
	}
	if result == nil {
		return nil
	}
	return json.Unmarshal(raw, result)
}

// respondWorkspaceDiagnostic answers a workspace/diagnostic call: absent any
// scripted WorkspaceReports, method-not-found (matching most real servers —
// only gopls implements workspace pulls today); otherwise the next scripted
// report, encoded via workspaceDiagnosticReportJSON so a full document
// entry's items key survives even when empty. Split out of Call purely to
// keep its cyclomatic complexity down; no behavioural difference.
func (c *Caller) respondWorkspaceDiagnostic(result any) error {
	if len(c.scenario.WorkspaceReports) == 0 {
		return &jsonrpc.MethodNotFoundError{Method: protocol.MethodWorkspaceDiagnostic}
	}
	raw, err := workspaceDiagnosticReportJSON(c.workspaceReport())
	if err != nil {
		return err
	}
	if result == nil {
		return nil
	}
	return json.Unmarshal(raw, result)
}

// diagnosticProviderCapability builds the InitializeResult's
// diagnosticProvider capability for a Pull or Hybrid scenario: the
// options-object form when DiagnosticOptions is set, else the bare `true`
// (today's default shape).
func (c *Caller) diagnosticProviderCapability() *protocol.BoolOrOptions {
	if c.scenario.DiagnosticOptions == nil {
		return &protocol.BoolOrOptions{Enabled: true}
	}
	raw, err := json.Marshal(c.scenario.DiagnosticOptions)
	if err != nil {
		return &protocol.BoolOrOptions{Enabled: true}
	}
	return &protocol.BoolOrOptions{Enabled: true, Raw: raw}
}

// pullReport computes the textDocument/diagnostic response for one call: it
// serves the next entry from a declarative PullReports script when one is
// set, else synthesises a single stable full report that degrades to
// "unchanged" ONLY when params.PreviousResultID matches exactly what this
// fake would have handed out — see Scenario.PullReports.
func (c *Caller) pullReport(params protocol.DocumentDiagnosticParams) protocol.DocumentDiagnosticReport {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.scenario.PullReports) > 0 {
		idx := c.pullCalls
		if idx >= len(c.scenario.PullReports) {
			idx = len(c.scenario.PullReports) - 1
		}
		c.pullCalls++
		report := c.scenario.PullReports[idx]
		if report.Kind == protocol.DiagnosticReportFull && report.RelatedDocuments == nil {
			report.RelatedDocuments = c.scenario.RelatedDocuments
		}
		return report
	}
	const resultID = "scenario-1"
	if params.PreviousResultID == resultID {
		return protocol.DocumentDiagnosticReport{Kind: protocol.DiagnosticReportUnchanged, ResultID: resultID}
	}
	return protocol.DocumentDiagnosticReport{
		Kind:             protocol.DiagnosticReportFull,
		ResultID:         resultID,
		Items:            c.scenario.AllDiagnostics(),
		RelatedDocuments: c.scenario.RelatedDocuments,
	}
}

// workspaceReport returns the next scripted workspace/diagnostic response,
// clamped to the final entry once WorkspaceReports is exhausted. Only called
// when len(WorkspaceReports) > 0 — see Call's MethodWorkspaceDiagnostic case.
func (c *Caller) workspaceReport() protocol.WorkspaceDiagnosticReport {
	c.mu.Lock()
	defer c.mu.Unlock()
	idx := c.wsCalls
	if idx >= len(c.scenario.WorkspaceReports) {
		idx = len(c.scenario.WorkspaceReports) - 1
	}
	c.wsCalls++
	return c.scenario.WorkspaceReports[idx]
}

// diagnosticReportJSON encodes report as textDocument/diagnostic wire bytes.
// protocol.DocumentDiagnosticReport.Items carries `omitempty` — correct for
// an "unchanged" report (no items key at all) but wrong for a "full" report
// with zero diagnostics, a distinct and meaningful signal ("this document has
// no problems") that still requires the items key on the wire per LSP 3.17.
// omitempty cannot distinguish "nil, omit" from "empty, keep", so a full
// report is marshalled through a local mirror type without the tag instead of
// changing the shared protocol type (whose omitempty is exactly right for
// every other caller, which only ever decodes this type).
func diagnosticReportJSON(report protocol.DocumentDiagnosticReport) ([]byte, error) {
	if report.Kind != protocol.DiagnosticReportFull {
		return json.Marshal(report)
	}
	items := report.Items
	if items == nil {
		items = []protocol.Diagnostic{}
	}
	return json.Marshal(struct {
		Kind             string                                       `json:"kind"`
		ResultID         string                                       `json:"resultId,omitempty"`
		Items            []protocol.Diagnostic                        `json:"items"`
		RelatedDocuments map[string]protocol.DocumentDiagnosticReport `json:"relatedDocuments,omitempty"`
	}{report.Kind, report.ResultID, items, report.RelatedDocuments})
}

// workspaceDiagnosticReportJSON encodes a workspace/diagnostic response,
// applying diagnosticReportJSON's full-report items-key fix per document
// entry (protocol.WorkspaceDocumentDiagnosticReport.Items has the identical
// omitempty gap).
func workspaceDiagnosticReportJSON(report protocol.WorkspaceDiagnosticReport) ([]byte, error) {
	rawItems := make([]json.RawMessage, len(report.Items))
	for i, item := range report.Items {
		if item.Kind != protocol.DiagnosticReportFull {
			b, err := json.Marshal(item)
			if err != nil {
				return nil, err
			}
			rawItems[i] = b
			continue
		}
		items := item.Items
		if items == nil {
			items = []protocol.Diagnostic{}
		}
		b, err := json.Marshal(struct {
			Kind     string                `json:"kind"`
			ResultID string                `json:"resultId,omitempty"`
			Items    []protocol.Diagnostic `json:"items"`
			URI      string                `json:"uri"`
			Version  *int32                `json:"version"`
		}{item.Kind, item.ResultID, items, item.URI, item.Version})
		if err != nil {
			return nil, err
		}
		rawItems[i] = b
	}
	return json.Marshal(struct {
		Items []json.RawMessage `json:"items"`
	}{rawItems})
}

func (c *Caller) Notify(_ context.Context, method string, params any) error {
	p, err := json.Marshal(params)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.calls = append(c.calls, jsonrpc.RecordedCall{Method: method, Params: p})
	if !allowedNotificationMethods[method] {
		c.unexpectedNotifications = append(c.unexpectedNotifications, method)
	}
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

// UnexpectedNotifications returns any client-sent notification methods this
// fake did not recognise as part of the standard LSP lifecycle it models
// (see allowedNotificationMethods). RunConformance checks this after every
// subtest's exchange and fails the test if non-empty.
func (c *Caller) UnexpectedNotifications() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.unexpectedNotifications...)
}

// PushDiagnostics delivers a server notification to the adapter subscriber.
func (c *Caller) PushDiagnostics() error {
	c.mu.Lock()
	fn := c.onNotify
	c.mu.Unlock()
	if fn == nil {
		return fmt.Errorf("lsptest: adapter did not install a notification handler")
	}
	p, err := json.Marshal(protocol.PublishDiagnosticsParams{URI: c.scenario.DocumentURI, Diagnostics: c.scenario.AllDiagnostics()})
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

// Refresh simulates a server-initiated workspace/diagnostic/refresh request —
// the mechanism a real server uses to tell the client "something changed;
// re-pull". Production wires the actual HANDLING of this one layer above the
// adapter: internal/cli/pool_diagnostics.go's wrapServerRequest, applied in
// poolOnStart, wraps every adapter's own server-request handler in front to
// intercept refresh before it ever reaches the adapter (see
// internal/lsp/serverreq.go's ServerRequestExtension hook, which stayed
// unused for refresh precisely because that one conn-level wiring point
// covers all nine adapters without touching any of them). This harness sits
// at the SAME adapter/conn seam that wrapper decorates, one layer below
// internal/cli, so it cannot exercise the wrapper itself — calling Refresh
// through the adapter's own (unmediated) request handler instead proves the
// adapter's base-case contract: an adapter that does not special-case
// refresh answers it exactly like any other unhandled server-initiated
// method, with a proper method-not-found — which is exactly the fallback
// wrapServerRequest itself uses for every method it does not intercept.
func (c *Caller) Refresh(ctx context.Context) error {
	c.mu.Lock()
	fn := c.onRequest
	c.mu.Unlock()
	if fn == nil {
		return fmt.Errorf("lsptest: adapter did not install a request handler")
	}
	_, err := fn(ctx, protocol.MethodWorkspaceDiagnosticRefresh, json.RawMessage(`null`))
	return err
}
