package base_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/adapters/base"
	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// errTransport stands in for any transport failure. Asserting against a
// sentinel proves both halves of the wrapping at once: the rendered message
// carries the server label, and errors.Is still reaches the cause.
var errTransport = errors.New("base_test: transport unavailable")

// recorded is one outbound message captured by stubCaller. Params is the value
// the adapter handed the transport, unmarshalled — nil stays nil, which is how
// the Shutdown assertion tells "no params" from "empty object".
type recorded struct {
	method string
	params any
}

// stubCaller is a jsonrpc.Caller whose two directions fail independently and
// which keeps the handlers base.New installs, so a test can drive a
// server-initiated request or notification back through the adapter.
//
// Concurrency: safe for concurrent use — the fan-out test pushes notifications
// from several goroutines.
type stubCaller struct {
	mu        sync.Mutex
	calls     []recorded
	notifies  []recorded
	callErr   error
	notifyErr error
	reply     string // raw JSON decoded into the Call result when non-empty
	onNotify  func(string, json.RawMessage)
	onRequest jsonrpc.RequestHandler
}

func (c *stubCaller) Call(_ context.Context, method string, params, result any) error {
	c.mu.Lock()
	c.calls = append(c.calls, recorded{method: method, params: params})
	reply, err := c.reply, c.callErr
	c.mu.Unlock()
	if err != nil {
		return err
	}
	if reply == "" || result == nil {
		return nil
	}
	return json.Unmarshal([]byte(reply), result)
}

func (c *stubCaller) Notify(_ context.Context, method string, params any) error {
	c.mu.Lock()
	c.notifies = append(c.notifies, recorded{method: method, params: params})
	err := c.notifyErr
	c.mu.Unlock()
	return err
}

func (c *stubCaller) SetNotificationHandler(fn func(string, json.RawMessage)) {
	c.mu.Lock()
	c.onNotify = fn
	c.mu.Unlock()
}

func (c *stubCaller) SetRequestHandler(fn jsonrpc.RequestHandler) {
	c.mu.Lock()
	c.onRequest = fn
	c.mu.Unlock()
}

func (c *stubCaller) Close() error { return nil }

func (c *stubCaller) sent() []recorded {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append(append([]recorded(nil), c.calls...), c.notifies...)
}

// registerWatchGlob drives a client/registerCapability request through the
// handler base.New installed — the path a real server takes — so the adapter's
// watcher filter ends up holding glob.
func (c *stubCaller) registerWatchGlob(t *testing.T, id, glob string) {
	t.Helper()
	c.mu.Lock()
	fn := c.onRequest
	c.mu.Unlock()
	if fn == nil {
		t.Fatal("base.New installed no server-request handler")
	}
	params := json.RawMessage(fmt.Sprintf(
		`{"registrations":[{"id":%q,"method":%q,"registerOptions":{"watchers":[{"globPattern":%q}]}}]}`,
		id, protocol.MethodDidChangeWatchedFiles, glob))
	if _, err := fn(context.Background(), protocol.MethodRegisterCapability, params); err != nil {
		t.Fatalf("registering watcher glob %q: %v", glob, err)
	}
}

func (c *stubCaller) push(t *testing.T, method string, params json.RawMessage) {
	t.Helper()
	c.mu.Lock()
	fn := c.onNotify
	c.mu.Unlock()
	if fn == nil {
		t.Fatal("base.New installed no notification handler")
	}
	fn(method, params)
}

// TestExportedSurface_IsExactlyLSPClient is the guard behind this package's
// central rule: an exported method here is promoted into every embedding
// adapter, and plumb resolves optional capabilities structurally, so one extra
// exported method would silently opt six language servers into pull
// diagnostics. conformance.TestAdapters_OptionalInterfaceSurface catches the
// consequence; this catches the cause, in the package that caused it.
func TestExportedSurface_IsExactlyLSPClient(t *testing.T) {
	adapterType := reflect.TypeOf((*base.Adapter)(nil))
	clientType := reflect.TypeOf((*lsp.Client)(nil)).Elem()

	want := make(map[string]bool, clientType.NumMethod())
	for i := range clientType.NumMethod() {
		want[clientType.Method(i).Name] = true
	}
	for i := range adapterType.NumMethod() {
		if name := adapterType.Method(i).Name; !want[name] {
			t.Errorf("*base.Adapter exports %s, which is not an lsp.Client method — "+
				"it would be promoted into every adapter; make it a package-level function instead", name)
		}
	}
	if got, wantN := adapterType.NumMethod(), clientType.NumMethod(); got != wantN {
		t.Fatalf("*base.Adapter exports %d methods, want exactly the %d of lsp.Client", got, wantN)
	}
}

func TestNew_InstallsBothTransportHandlers(t *testing.T) {
	conn := &stubCaller{}
	base.New(conn, "stubls")

	conn.mu.Lock()
	gotNotify, gotRequest := conn.onNotify != nil, conn.onRequest != nil
	conn.mu.Unlock()

	if !gotNotify {
		t.Error("New did not install a notification handler; Subscribe would never fire")
	}
	if !gotRequest {
		t.Error("New did not install a server-request handler; watcher registrations would be refused")
	}
}

// TestErrorLabels_RenderAsServerLabelCause pins the wrapping shape every plumb
// adapter inherits. The per-adapter labels themselves live in
// conformance.RunErrorContract; this pins the format they are rendered in.
func TestErrorLabels_RenderAsServerLabelCause(t *testing.T) {
	a := base.New(&stubCaller{callErr: errTransport, notifyErr: errTransport}, "stubls")
	ctx := context.Background()

	cases := []struct {
		label string
		call  func() error
	}{
		{"initialize", func() error { _, err := a.Initialize(ctx, protocol.InitializeParams{}); return err }},
		{"shutdown", func() error { return a.Shutdown(ctx) }},
		{"initialized", func() error { return a.Initialized(ctx) }},
		{"exit", func() error { return a.Exit(ctx) }},
		{"didOpen", func() error { return a.DidOpen(ctx, protocol.DidOpenTextDocumentParams{}) }},
		{"documentSymbol", func() error {
			_, err := a.DocumentSymbols(ctx, protocol.DocumentSymbolParams{})
			return err
		}},
		{"hover", func() error { _, err := a.Hover(ctx, protocol.HoverParams{}); return err }},
	}

	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			err := tc.call()
			if want := "stubls " + tc.label + ": " + errTransport.Error(); err == nil || err.Error() != want {
				t.Fatalf("error = %v, want %q", err, want)
			}
			if !errors.Is(err, errTransport) {
				t.Fatalf("error %q does not unwrap to the cause — the %%w wrapping was lost", err)
			}
		})
	}
}

// TestHelpers_WrapWithTheAdapterLabel covers the escape hatches an adapter uses
// for methods beyond lsp.Client (typescript's Diagnostic, the lazy-open
// adapters' "open <uri>" failures).
func TestHelpers_WrapWithTheAdapterLabel(t *testing.T) {
	conn := &stubCaller{callErr: errTransport, notifyErr: errTransport}
	a := base.New(conn, "stubls")
	ctx := context.Background()

	cases := []struct {
		name string
		err  error
	}{
		{"Call", func() error {
			_, err := base.Call[[]protocol.Location](ctx, a, "custom", "custom/method", nil)
			return err
		}()},
		{"CallPtr", func() error {
			_, err := base.CallPtr[protocol.Hover](ctx, a, "custom", "custom/method", nil)
			return err
		}()},
		{"CallRaw", func() error {
			_, err := base.CallRaw(ctx, a, "custom", "custom/method", nil)
			return err
		}()},
		{"Notify", base.Notify(ctx, a, "custom", "custom/method", nil)},
		{"Wrap", base.Wrap(a, "custom", errTransport)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if want := "stubls custom: " + errTransport.Error(); tc.err == nil || tc.err.Error() != want {
				t.Fatalf("error = %v, want %q", tc.err, want)
			}
			if !errors.Is(tc.err, errTransport) {
				t.Fatalf("error %q does not unwrap to the cause", tc.err)
			}
		})
	}

	if err := base.Wrap(a, "custom", nil); err != nil {
		t.Errorf("Wrap(nil) = %v, want nil — a nil cause must not become an error", err)
	}
}

// TestCall_NullReplyYieldsNilSlice pins the decode contract the generic helper
// must preserve: T is the decode target verbatim, so a server answering null
// leaves a slice result nil rather than an empty non-nil slice.
func TestCall_NullReplyYieldsNilSlice(t *testing.T) {
	a := base.New(&stubCaller{reply: "null"}, "stubls")

	syms, err := a.DocumentSymbols(context.Background(), protocol.DocumentSymbolParams{})
	if err != nil {
		t.Fatalf("DocumentSymbols = %v, want nil error", err)
	}
	if syms != nil {
		t.Fatalf("DocumentSymbols = %#v, want a nil slice", syms)
	}
}

func TestCall_DecodesResult(t *testing.T) {
	a := base.New(&stubCaller{reply: `[{"name":"main","kind":12}]`}, "stubls")

	syms, err := a.DocumentSymbols(context.Background(), protocol.DocumentSymbolParams{})
	if err != nil {
		t.Fatalf("DocumentSymbols = %v, want nil error", err)
	}
	if len(syms) != 1 || syms[0].Name != "main" {
		t.Fatalf("DocumentSymbols = %#v, want one symbol named main", syms)
	}
}

func TestCallPtr_ReturnsNonNilOnSuccessAndNilOnFailure(t *testing.T) {
	ok := base.New(&stubCaller{reply: `{"contents":{"kind":"markdown","value":"doc"}}`}, "stubls")
	hover, err := ok.Hover(context.Background(), protocol.HoverParams{})
	if err != nil || hover == nil {
		t.Fatalf("Hover = (%v, %v), want a non-nil result and no error", hover, err)
	}

	bad := base.New(&stubCaller{callErr: errTransport}, "stubls")
	hover, err = bad.Hover(context.Background(), protocol.HoverParams{})
	if hover != nil || err == nil {
		t.Fatalf("Hover on a failing transport = (%v, %v), want (nil, error)", hover, err)
	}
}

func TestCallRaw_ReturnsTheReplyUndecoded(t *testing.T) {
	conn := &stubCaller{reply: `{"kind":"full","items":[]}`}
	a := base.New(conn, "stubls")

	raw, err := base.CallRaw(context.Background(), a, "custom", "custom/method", nil)
	if err != nil {
		t.Fatalf("CallRaw = %v, want nil error", err)
	}
	if string(raw) != conn.reply {
		t.Fatalf("CallRaw = %s, want %s", raw, conn.reply)
	}
}

func TestInitialize_StoresCapabilities(t *testing.T) {
	a := base.New(&stubCaller{reply: `{"capabilities":{"hoverProvider":true}}`}, "stubls")

	if caps := a.Capabilities(); caps != nil {
		t.Fatalf("Capabilities before Initialize = %#v, want nil", caps)
	}
	result, err := a.Initialize(context.Background(), protocol.InitializeParams{})
	if err != nil {
		t.Fatalf("Initialize = %v, want nil error", err)
	}
	caps := a.Capabilities()
	if caps == nil {
		t.Fatal("Capabilities after Initialize = nil, want the negotiated capabilities")
	}
	if caps == &result.Capabilities {
		t.Error("Capabilities aliases the returned result — a caller mutating the result would rewrite adapter state")
	}
}

func TestInitialize_FailureLeavesCapabilitiesUnset(t *testing.T) {
	a := base.New(&stubCaller{callErr: errTransport}, "stubls")

	if _, err := a.Initialize(context.Background(), protocol.InitializeParams{}); err == nil {
		t.Fatal("Initialize on a failing transport = nil error, want the wrapped failure")
	}
	if caps := a.Capabilities(); caps != nil {
		t.Fatalf("Capabilities = %#v, want nil after a failed Initialize", caps)
	}
}

// TestShutdown_SendsNoParams pins the longhand Shutdown: it must hand the
// transport nil params and no result pointer. Routing it through the generic
// Call helper would change both, and a real server sees the difference.
func TestShutdown_SendsNoParams(t *testing.T) {
	conn := &stubCaller{}
	a := base.New(conn, "stubls")

	if err := a.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown = %v, want nil error", err)
	}
	sent := conn.sent()
	if len(sent) != 1 || sent[0].method != protocol.MethodShutdown {
		t.Fatalf("sent %#v, want a single %s request", sent, protocol.MethodShutdown)
	}
	if sent[0].params != nil {
		t.Fatalf("shutdown params = %#v, want nil", sent[0].params)
	}
}

func TestSubscribe_FansOutAndUnsubscribes(t *testing.T) {
	conn := &stubCaller{}
	a := base.New(conn, "stubls")

	var mu sync.Mutex
	seen := map[string]int{}
	record := func(name string) func(string, json.RawMessage) {
		return func(method string, _ json.RawMessage) {
			mu.Lock()
			seen[name+":"+method]++
			mu.Unlock()
		}
	}

	unsubA := a.Subscribe(record("a"))
	a.Subscribe(record("b"))

	conn.push(t, protocol.MethodPublishDiagnostics, json.RawMessage(`{}`))
	unsubA()
	conn.push(t, protocol.MethodPublishDiagnostics, json.RawMessage(`{}`))

	mu.Lock()
	defer mu.Unlock()
	if got := seen["a:"+protocol.MethodPublishDiagnostics]; got != 1 {
		t.Errorf("first subscriber saw %d notifications, want 1 (it unsubscribed after the first)", got)
	}
	if got := seen["b:"+protocol.MethodPublishDiagnostics]; got != 2 {
		t.Errorf("second subscriber saw %d notifications, want 2", got)
	}
}

// TestSubscribe_ConcurrentWithDispatch is a race-detector target: subscribers
// come and go while the transport fans notifications out.
func TestSubscribe_ConcurrentWithDispatch(t *testing.T) {
	conn := &stubCaller{}
	a := base.New(conn, "stubls")

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			unsub := a.Subscribe(func(string, json.RawMessage) {})
			unsub()
		}()
		go func() {
			defer wg.Done()
			conn.push(t, protocol.MethodPublishDiagnostics, json.RawMessage(`{}`))
		}()
	}
	wg.Wait()
}

// TestDidChangeWatchedFiles_FiltersThroughRegisteredGlobs pins both branches:
// a non-matching glob short-circuits before the transport (so plumb stays quiet
// on unwatched writes) and a matching one sends only the surviving events.
func TestDidChangeWatchedFiles_FiltersThroughRegisteredGlobs(t *testing.T) {
	conn := &stubCaller{}
	a := base.New(conn, "stubls")
	ctx := context.Background()
	params := protocol.DidChangeWatchedFilesParams{
		Changes: []protocol.FileEvent{{URI: "file:///project/main.go", Type: protocol.FileChanged}},
	}

	conn.registerWatchGlob(t, "no-match", "**/*.plumb-no-such-suffix")
	if err := a.DidChangeWatchedFiles(ctx, params); err != nil {
		t.Fatalf("DidChangeWatchedFiles with only a non-matching glob = %v, want nil", err)
	}
	if sent := conn.sent(); len(sent) != 0 {
		t.Fatalf("sent %#v, want nothing — the event was filtered out", sent)
	}

	conn.registerWatchGlob(t, "match", "**/*.go")
	if err := a.DidChangeWatchedFiles(ctx, params); err != nil {
		t.Fatalf("DidChangeWatchedFiles with a matching glob = %v, want nil", err)
	}
	sent := conn.sent()
	if len(sent) != 1 || sent[0].method != protocol.MethodDidChangeWatchedFiles {
		t.Fatalf("sent %#v, want a single %s notification", sent, protocol.MethodDidChangeWatchedFiles)
	}
}

// TestServerName_PrefixesEveryError proves the label is per-instance state, not
// a package constant: two adapters over the same transport must not share it.
func TestServerName_PrefixesEveryError(t *testing.T) {
	ctx := context.Background()
	for _, server := range []string{"pyright", "rust-analyzer", "jdtls"} {
		a := base.New(&stubCaller{notifyErr: errTransport}, server)
		err := a.Exit(ctx)
		if want := server + " exit: " + errTransport.Error(); err == nil || err.Error() != want {
			t.Errorf("error = %v, want %q", err, want)
		}
	}
}
