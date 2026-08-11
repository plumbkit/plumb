package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/paths"
)

// errTransport is the sentinel failure every fake transport in this file
// returns. Asserting against a sentinel (rather than an anonymous error) lets
// each assertion prove BOTH halves of the contract at once: the rendered
// message carries the adapter's label, and errors.Is still reaches the cause,
// so the %w wrapping cannot silently degrade to %v.
var errTransport = errors.New("conformance: transport unavailable")

// notifyErrorLabels and callErrorLabels are the golden label sets: one entry
// per lsp.Client method that can fail on that half of the transport, spelled
// exactly as plumb surfaces it. assertLabelSet compares the cases actually
// built against these and fails in BOTH directions, so deleting a case cannot
// quietly unpin a user-visible string and a newly added error-returning method
// cannot arrive unpinned. Grow either list only alongside the case producing
// the label.
var (
	notifyErrorLabels = []string{
		"initialized",
		"exit",
		"didOpen",
		"didChange",
		"didClose",
		"didChangeWatchedFiles",
	}

	callErrorLabels = []string{
		"initialize",
		"shutdown",
		"documentSymbol",
		"workspaceSymbol",
		"definition",
		"references",
		"hover",
		"prepareRename",
		"rename",
		"prepareCallHierarchy",
		"callHierarchy/incomingCalls",
		"callHierarchy/outgoingCalls",
		"prepareTypeHierarchy",
		"typeHierarchy/supertypes",
		"typeHierarchy/subtypes",
	}
)

// Labels an adapter carries only when it exposes the matching optional pull
// surface, so they join the golden set conditionally (see runCallErrorContract).
const (
	pullDiagnosticLabel          = "diagnostic"
	workspacePullDiagnosticLabel = "workspace diagnostic"
)

// failingCaller is a jsonrpc.Caller whose two directions fail independently.
// jsonrpc.MockCaller cannot serve this purpose: its Notify never returns an
// error, so the notification labels would be unreachable.
//
// Concurrency: none. Each harness run drives one adapter from a single
// goroutine and never delivers a server notification, so onRequest is written
// (by the adapter's New) and read on that same goroutine.
type failingCaller struct {
	failCall   bool
	failNotify bool

	onRequest jsonrpc.RequestHandler
}

func (c *failingCaller) Call(_ context.Context, _ string, _, _ any) error {
	if c.failCall {
		return errTransport
	}
	return nil
}

func (c *failingCaller) Notify(_ context.Context, _ string, _ any) error {
	if c.failNotify {
		return errTransport
	}
	return nil
}

func (c *failingCaller) SetNotificationHandler(_ func(string, json.RawMessage)) {}

func (c *failingCaller) SetRequestHandler(fn jsonrpc.RequestHandler) { c.onRequest = fn }

func (c *failingCaller) Close() error { return nil }

// registerWatchGlob drives a client/registerCapability request through the
// adapter's OWN server-request handler — the same path a real server takes —
// so the adapter's watcher filter ends up holding glob. Without a registered
// glob the filter passes every event through unchanged, which is a different
// branch of DidChangeWatchedFiles than the one this pins.
func (c *failingCaller) registerWatchGlob(ctx context.Context, id, glob string) error {
	if c.onRequest == nil {
		return errors.New("conformance: adapter installed no server-request handler")
	}
	params := json.RawMessage(fmt.Sprintf(
		`{"registrations":[{"id":%q,"method":%q,"registerOptions":{"watchers":[{"globPattern":%q}]}}]}`,
		id, protocol.MethodDidChangeWatchedFiles, glob))
	if _, err := c.onRequest(ctx, protocol.MethodRegisterCapability, params); err != nil {
		return fmt.Errorf("conformance: registering watcher glob %q: %w", glob, err)
	}
	return nil
}

// labelledCall pairs one adapter method with the error label it must emit when
// the transport underneath it fails.
type labelledCall struct {
	label string
	call  func() error
}

// RunErrorContract drives every lsp.Client method against a transport that
// always fails and asserts the adapter wrapped the failure as
// "<server> <label>: <cause>". These strings are the ones plumb surfaces to
// agents; nothing else in the tree pins them, so a refactor that hoists the
// wrapping into a shared base package could otherwise reword every diagnostic
// plumb emits without turning a single test red.
//
// docName and docContents describe the fixture the harness writes to disk on
// the caller's behalf; both halves need a readable file (see
// writeContractDocument), so the harness owns it rather than each adapter test.
//
// Two passes are needed because notifications and requests fail through
// different halves of jsonrpc.Caller and an adapter that only ever sends
// notifications on one path would otherwise go unpinned.
func RunErrorContract(t *testing.T, factory Factory, initParams InitParamsFactory, server, docName, docContents string) {
	t.Helper()
	docURI := writeContractDocument(t, docName, docContents)
	t.Run("notifications", func(t *testing.T) { runNotifyErrorContract(t, factory, server, docURI) })
	t.Run("requests", func(t *testing.T) { runCallErrorContract(t, factory, initParams, server, docURI) })
}

// writeContractDocument materialises name in a fresh temp directory and returns
// its URI. The file MUST exist: the sourcekit-lsp, zls, vscode-html-language-server
// and kotlin-lsp adapters lazily os.ReadFile the document and send
// didOpen before their per-document queries, so a missing file would pin their
// "open <uri>" label instead of the request label under test. Owning the fixture
// here means no future adapter test can forget that precondition — the "open"
// label has its own pin in RunLazyOpenErrorContract.
func writeContractDocument(t *testing.T, name, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	WriteFixture(t, map[string]string{path: contents})
	return paths.PathToURI(path)
}

// RunLazyOpenErrorContract pins the "<server> open <uri>: <cause>" label the
// lazy-open adapters attach when the document they must read from disk is not
// there. RunErrorContract deliberately hands every adapter a real file so it
// pins the REQUEST labels, which leaves this branch unpinned — and it is
// exactly the branch a shared base adapter would normalise away.
//
// Call this from the adapters that open lazily (swift, zig, html, kotlin) and
// nowhere else. The per-adapter call is deliberate: auto-detecting lazy-open
// behaviour inside the shared harness would make an adapter's participation
// invisible at its own call site.
//
// The transport used here never fails, so the unreadable document is the only
// possible source of an error.
func RunLazyOpenErrorContract(t *testing.T, factory Factory, server, docName string) {
	t.Helper()
	missing := paths.PathToURI(filepath.Join(t.TempDir(), docName))
	adapter := factory(&failingCaller{})
	_, err := adapter.DocumentSymbols(context.Background(), protocol.DocumentSymbolParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: missing},
	})
	if err == nil {
		t.Fatalf("DocumentSymbols on a document that is not on disk = nil error, want a %q wrapping", server+" open "+missing)
	}
	// The cause is the operating system's own open error, whose wording is
	// platform-specific: the prefix pins the label, errors.Is pins the %w.
	if want := server + " open " + missing + ": "; !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("error = %q, want prefix %q", err.Error(), want)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("error %q does not unwrap to fs.ErrNotExist — the %%w wrapping was lost", err)
	}
}

// assertWrapped fails t unless err renders exactly as "<server> <label>: <cause>"
// and still unwraps to the sentinel.
func assertWrapped(t *testing.T, err error, server, label string) {
	t.Helper()
	if err == nil {
		t.Fatalf("got nil error, want %q", server+" "+label+": "+errTransport.Error())
	}
	if want := server + " " + label + ": " + errTransport.Error(); err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
	if !errors.Is(err, errTransport) {
		t.Fatalf("error %q does not unwrap to the transport cause — the %%w wrapping was lost", err)
	}
}

// assertLabelSet fails t unless cases carry exactly want's labels. It is the
// shrink guard: without it, deleting one case (or one whole append) would
// silently unpin user-visible strings across all nine adapters with a green
// tree, and a newly added error-returning lsp.Client method would arrive
// unpinned with nothing objecting. Mismatches fail in BOTH directions on
// purpose — an extra label must be added to the golden list deliberately, not
// absorbed.
func assertLabelSet(t *testing.T, cases []labelledCall, want []string) {
	t.Helper()
	got := make(map[string]bool, len(cases))
	for _, c := range cases {
		if got[c.label] {
			t.Errorf("label %q is built twice; one of the two cases is dead", c.label)
		}
		got[c.label] = true
	}
	wanted := make(map[string]bool, len(want))
	for _, l := range want {
		wanted[l] = true
		if !got[l] {
			t.Errorf("label %q is no longer pinned — restore its case rather than shrinking the golden set", l)
		}
	}
	for _, c := range cases {
		if !wanted[c.label] {
			t.Errorf("label %q is exercised but absent from the golden set — add it there deliberately", c.label)
		}
	}
	if len(got) != len(wanted) {
		t.Fatalf("contract covers %d label(s), want %d", len(got), len(wanted))
	}
}

// runLabelledCalls runs each case as its own subtest so one changed label does
// not mask the rest of the set.
func runLabelledCalls(t *testing.T, cases []labelledCall, server string) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) { assertWrapped(t, tc.call(), server, tc.label) })
	}
}

// runNotifyErrorContract pins the labels an adapter attaches to a failed
// outbound notification.
func runNotifyErrorContract(t *testing.T, factory Factory, server, docURI string) {
	t.Helper()
	conn := &failingCaller{failNotify: true}
	adapter := factory(conn)

	cases := notifyErrorCases(context.Background(), adapter, docURI)
	cases = append(cases, watchedFilesErrorCase(t, adapter, conn, docURI))
	assertLabelSet(t, cases, notifyErrorLabels)
	runLabelledCalls(t, cases, server)
}

func notifyErrorCases(ctx context.Context, adapter lsp.Client, docURI string) []labelledCall {
	return []labelledCall{
		{"initialized", func() error { return adapter.Initialized(ctx) }},
		{"exit", func() error { return adapter.Exit(ctx) }},
		{"didOpen", func() error {
			return adapter.DidOpen(ctx, protocol.DidOpenTextDocumentParams{
				TextDocument: protocol.TextDocumentItem{URI: docURI, Version: 1},
			})
		}},
		{"didChange", func() error {
			return adapter.DidChange(ctx, protocol.DidChangeTextDocumentParams{
				TextDocument: protocol.VersionedTextDocumentIdentifier{URI: docURI, Version: 2},
			})
		}},
		{"didClose", func() error {
			return adapter.DidClose(ctx, protocol.DidCloseTextDocumentParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: docURI},
			})
		}},
	}
}

// watchedFilesErrorCase pins both branches of DidChangeWatchedFiles. The method
// filters its events through the adapter's watcher registrations and returns nil
// early when nothing survives, so the label is observable only once a glob that
// MATCHES the document has been registered. Asserting the short-circuit here
// keeps a future refactor from "fixing" the silent nil into an error, which
// would make plumb noisy on every unwatched write.
func watchedFilesErrorCase(t *testing.T, adapter lsp.Client, conn *failingCaller, docURI string) labelledCall {
	t.Helper()
	ctx := context.Background()
	params := protocol.DidChangeWatchedFilesParams{
		Changes: []protocol.FileEvent{{URI: docURI, Type: protocol.FileChanged}},
	}
	if err := conn.registerWatchGlob(ctx, "contract-nomatch", "**/*.plumb-no-such-suffix"); err != nil {
		t.Fatal(err)
	}
	if err := adapter.DidChangeWatchedFiles(ctx, params); err != nil {
		t.Fatalf("DidChangeWatchedFiles with only a non-matching glob = %v, want nil (filtered out before the transport)", err)
	}
	if err := conn.registerWatchGlob(ctx, "contract-match", "**/*"); err != nil {
		t.Fatal(err)
	}
	return labelledCall{"didChangeWatchedFiles", func() error { return adapter.DidChangeWatchedFiles(ctx, params) }}
}

// runCallErrorContract pins the labels an adapter attaches to a failed
// outbound request. Notify succeeds here so the lazy-open adapters reach the
// request under test rather than failing in their didOpen.
func runCallErrorContract(t *testing.T, factory Factory, initParams InitParamsFactory, server, docURI string) {
	t.Helper()
	conn := &failingCaller{failCall: true}
	adapter := factory(conn)
	ctx := context.Background()

	cases := lifecycleErrorCases(ctx, adapter, initParams, docURI)
	cases = append(cases, queryErrorCases(ctx, adapter, docURI)...)
	cases = append(cases, hierarchyErrorCases(ctx, adapter, docURI)...)
	cases = append(cases, pullErrorCases(ctx, adapter, docURI)...)
	assertLabelSet(t, cases, wantCallErrorLabels(adapter))
	runLabelledCalls(t, cases, server)
}

// wantCallErrorLabels states the expected set independently of the code that
// builds the cases, branching on the same optional interfaces pullErrorCases
// does. Two independent statements of the same fact is the point: dropping
// either side is a mismatch.
func wantCallErrorLabels(adapter lsp.Client) []string {
	want := append([]string(nil), callErrorLabels...)
	if _, ok := adapter.(pullClient); ok {
		want = append(want, pullDiagnosticLabel)
	}
	if _, ok := adapter.(workspacePullClient); ok {
		want = append(want, workspacePullDiagnosticLabel)
	}
	return want
}

func lifecycleErrorCases(ctx context.Context, adapter lsp.Client, initParams InitParamsFactory, docURI string) []labelledCall {
	return []labelledCall{
		{"initialize", func() error {
			_, err := adapter.Initialize(ctx, initParams(rootURIFor(docURI)))
			return err
		}},
		{"shutdown", func() error { return adapter.Shutdown(ctx) }},
	}
}

func queryErrorCases(ctx context.Context, adapter lsp.Client, docURI string) []labelledCall {
	doc := protocol.TextDocumentIdentifier{URI: docURI}
	return []labelledCall{
		{"documentSymbol", func() error {
			_, err := adapter.DocumentSymbols(ctx, protocol.DocumentSymbolParams{TextDocument: doc})
			return err
		}},
		{"workspaceSymbol", func() error {
			_, err := adapter.WorkspaceSymbols(ctx, protocol.WorkspaceSymbolParams{Query: "sample"})
			return err
		}},
		{"definition", func() error {
			_, err := adapter.Definition(ctx, protocol.DefinitionParams{TextDocument: doc})
			return err
		}},
		{"references", func() error {
			_, err := adapter.References(ctx, protocol.ReferenceParams{TextDocument: doc})
			return err
		}},
		{"hover", func() error {
			_, err := adapter.Hover(ctx, protocol.HoverParams{TextDocument: doc})
			return err
		}},
		{"prepareRename", func() error {
			_, err := adapter.PrepareRename(ctx, protocol.PrepareRenameParams{TextDocument: doc})
			return err
		}},
		{"rename", func() error {
			_, err := adapter.Rename(ctx, protocol.RenameParams{TextDocument: doc, NewName: "renamed"})
			return err
		}},
	}
}

// hierarchyErrorCases needs docURI on the two prepare* params: the lazy-open
// adapters ensure the document is open there (base.OpenTracker.Ensure), and an
// empty URI would pin their "open <uri>" label instead of the request label.
func hierarchyErrorCases(ctx context.Context, adapter lsp.Client, docURI string) []labelledCall {
	doc := protocol.TextDocumentIdentifier{URI: docURI}
	return []labelledCall{
		{"prepareCallHierarchy", func() error {
			_, err := adapter.PrepareCallHierarchy(ctx, protocol.PrepareCallHierarchyParams{TextDocument: doc})
			return err
		}},
		{"callHierarchy/incomingCalls", func() error {
			_, err := adapter.IncomingCalls(ctx, protocol.CallHierarchyIncomingCallsParams{})
			return err
		}},
		{"callHierarchy/outgoingCalls", func() error {
			_, err := adapter.OutgoingCalls(ctx, protocol.CallHierarchyOutgoingCallsParams{})
			return err
		}},
		{"prepareTypeHierarchy", func() error {
			_, err := adapter.PrepareTypeHierarchy(ctx, protocol.PrepareTypeHierarchyParams{TextDocument: doc})
			return err
		}},
		{"typeHierarchy/supertypes", func() error {
			_, err := adapter.Supertypes(ctx, protocol.TypeHierarchySupertypesParams{})
			return err
		}},
		{"typeHierarchy/subtypes", func() error {
			_, err := adapter.Subtypes(ctx, protocol.TypeHierarchySubtypesParams{})
			return err
		}},
	}
}

// pullErrorCases adds the diagnostics labels an adapter only has when it
// implements the optional pull surfaces — branching on the same structural
// interfaces RunConformance uses, so an adapter that gains or loses pull is
// covered without editing this file.
func pullErrorCases(ctx context.Context, adapter lsp.Client, docURI string) []labelledCall {
	var out []labelledCall
	if pull, ok := adapter.(pullClient); ok {
		out = append(out, labelledCall{pullDiagnosticLabel, func() error {
			_, err := pull.Diagnostic(ctx, protocol.DocumentDiagnosticParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: docURI},
			})
			return err
		}})
	}
	if wp, ok := adapter.(workspacePullClient); ok {
		// gopls spells this label with a SPACE — "gopls workspace diagnostic" —
		// where every other label is the camelCase LSP method name. The
		// inconsistency is pinned as-is: this harness records what plumb emits
		// today so the base-package refactor is provably a no-op, and changing
		// the wording is a separate, deliberate decision.
		out = append(out, labelledCall{workspacePullDiagnosticLabel, func() error {
			_, err := wp.WorkspaceDiagnostic(ctx, protocol.WorkspaceDiagnosticParams{})
			return err
		}})
	}
	return out
}

// rootURIFor returns docURI's parent directory URI. Every adapter's
// DefaultInitParams stores rootURI verbatim and initialize fails at the
// transport in this pass, so the value only has to look like a workspace root.
func rootURIFor(docURI string) string {
	if i := strings.LastIndexByte(docURI, '/'); i > 0 {
		return docURI[:i]
	}
	return docURI
}
