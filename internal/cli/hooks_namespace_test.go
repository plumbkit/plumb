package cli

import (
	"bufio"
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/tools"
)

// These tests pin that the Claude Code / Codex hooks speak for their session's
// namespace: the token they run under travels with every daemon request, and
// the daemon answers only from that namespace's connections — never, for a
// refused token, from the default one.

func TestHookNamespaceToken_IsTheSessionsPLUMBNamespace(t *testing.T) {
	t.Setenv("PLUMB_NAMESPACE", " pns1-abc ")
	if got := hookNamespaceToken(); got != "pns1-abc" {
		t.Fatalf("hookNamespaceToken = %q, want the PLUMB_NAMESPACE token", got)
	}
	t.Setenv("PLUMB_NAMESPACE", "")
	if got := hookNamespaceToken(); got != "" {
		t.Fatalf("hookNamespaceToken = %q with no token, want the default namespace", got)
	}
}

// fakeResolve resolves "tok-1" to ns-1, "" to the default namespace, and
// refuses everything else.
func fakeResolve(token string) (string, bool) {
	switch token {
	case "":
		return "", true
	case "tok-1":
		return "ns-1", true
	}
	return "", false
}

func TestMailboxCommand_ScopesToTheTokensNamespace(t *testing.T) {
	var got []string
	h := ctrlHandlers{
		resolveNamespace: fakeResolve,
		mailbox: func(_ context.Context, r hookMailboxRequest) (hookMailboxReply, error) {
			got = append(got, r.namespace)
			return hookMailboxReply{}, nil
		},
	}
	for _, payload := range []string{
		`{"session_id":"s","namespace_token":"tok-1"}`,
		`{"session_id":"s"}`,
	} {
		var out bytes.Buffer
		handleMailboxCommand(&out, ctrlMailboxCommand+payload, h)
	}
	if len(got) != 2 || got[0] != "ns-1" || got[1] != "" {
		t.Fatalf("probed namespaces = %q, want [ns-1, default]", got)
	}
	var out bytes.Buffer
	handleMailboxCommand(&out, ctrlMailboxCommand+`{"session_id":"s","namespace_token":"revoked"}`, h)
	if !strings.HasPrefix(out.String(), "error:") || len(got) != 2 {
		t.Fatalf("a refused token was answered (%q); it must match nobody, not the default namespace", out.String())
	}
}

func TestMailboxProbe_AsksOnlyTheNamespacesConnections(t *testing.T) {
	reg := newConnRegistry()
	asked := map[string]int{}
	probe := func(name string) func(string) (tools.Inbox, bool) {
		return func(string) (tools.Inbox, bool) { asked[name]++; return tools.Inbox{}, false }
	}
	reg.add("default", connHandle{mailboxInbox: probe("default")})
	reg.add("ns", connHandle{mailboxInbox: probe("ns"), namespace: func() string { return "ns-1" }})
	_, _ = reg.mailboxProbe(context.Background(), hookMailboxRequest{SessionID: "s", namespace: "ns-1"})
	if asked["ns"] != 1 || asked["default"] != 0 {
		t.Fatalf("a namespaced probe asked %v; want only the namespace's connection", asked)
	}
	_, _ = reg.mailboxProbe(context.Background(), hookMailboxRequest{SessionID: "s"})
	if asked["default"] != 1 || asked["ns"] != 1 {
		t.Fatalf("a default-namespace probe asked %v; want only the default connection", asked)
	}
}

func TestHintRootFor_AnswersOnlyFromTheNamespace(t *testing.T) {
	reg := newConnRegistry()
	reg.add("default", connHandle{
		hintRoot:  func(string) (string, bool, bool) { return "/default", false, true },
		workspace: func() string { return "/default" },
	})
	reg.add("ns", connHandle{
		hintRoot:  func(string) (string, bool, bool) { return "/ns", false, true },
		workspace: func() string { return "/ns" },
		namespace: func() string { return "ns-1" },
	})
	if root, ok := reg.hintRootFor("s", "", "ns-1"); !ok || root.Path != "/ns" {
		t.Fatalf("namespaced hint root = %+v, %v; want /ns alone", root, ok)
	}
	if root, ok := reg.hintRootFor("s", "", ""); !ok || root.Path != "/default" {
		t.Fatalf("default hint root = %+v, %v; want /default alone", root, ok)
	}
	if _, ok := reg.hintRootFor("s", "", "ns-2"); ok {
		t.Fatal("a namespace with no connection was answered by another's")
	}
}

func TestContextHintCommand_ScopesToTheTokensNamespace(t *testing.T) {
	var got []string
	h := ctrlHandlers{
		resolveNamespace: fakeResolve,
		contextHint: func(_ context.Context, r contextHintRequest) contextHintReply {
			got = append(got, r.namespace)
			return contextHintReply{}
		},
	}
	var out bytes.Buffer
	handleContextHintCommand(&out, ctrlContextHintCommand+`{"session_id":"s","event":"UserPromptSubmit","namespace_token":"tok-1"}`, h)
	out.Reset()
	handleContextHintCommand(&out, ctrlContextHintCommand+`{"session_id":"s","event":"UserPromptSubmit","namespace_token":"revoked"}`, h)
	if len(got) != 1 || got[0] != "ns-1" || !strings.HasPrefix(out.String(), "error:") {
		t.Fatalf("hint namespaces = %q, refused reply %q; want [ns-1] and the refused token unanswered", got, out.String())
	}
}

func TestClearCommand_MarksInTheTokensNamespace(t *testing.T) {
	var marked []string
	h := ctrlHandlers{resolveNamespace: fakeResolve, conversationCleared: func(key string) { marked = append(marked, key) }}
	for _, line := range []string{
		ctrlConversationClearedCommand + " conv-a tok-1",
		ctrlConversationClearedCommand + " conv-b",
		ctrlConversationClearedCommand + " conv-c revoked",
	} {
		server, client := net.Pipe()
		go func() { handleClearCommand(server, line, h); server.Close() }()
		_, _ = bufio.NewReader(client).ReadString('\n')
		client.Close()
	}
	want := []string{clearMarkerKey("ns-1", "conv-a"), "conv-b"}
	if len(marked) != 2 || marked[0] != want[0] || marked[1] != want[1] {
		t.Fatalf("marked = %q, want %q (a refused token marks nothing)", marked, want)
	}
}

// TestHooks_SendTheirNamespaceToken pins the hook side: the Stop mailbox
// probe, the /clear notice and both hosts' context-hint requests carry the
// session's PLUMB_NAMESPACE.
func TestHooks_SendTheirNamespaceToken(t *testing.T) {
	probeTestEnv(t)
	t.Setenv("PLUMB_NAMESPACE", "tok-1")
	lines := make(chan string, 4)
	fakeCtrlDaemon(t, func(c net.Conn, line string) { lines <- line; _, _ = c.Write([]byte("error: no\n")) })

	probeHookMailbox("conv-1", true)
	if line := <-lines; !strings.Contains(line, `"namespace_token":"tok-1"`) {
		t.Errorf("the mailbox probe went without its namespace: %q", line)
	}
	notifyConversationCleared("conv-1")
	if line := <-lines; line != ctrlConversationClearedCommand+" conv-1 tok-1" {
		t.Errorf("the /clear notice = %q, want its namespace token appended", line)
	}

	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, ".plumb"), 0o755); err != nil {
		t.Fatal(err)
	}
	var reqs []contextHintRequest
	ask := func(r contextHintRequest) string { reqs = append(reqs, r); return "" }
	claudeContextHintOutput(claudeHookInput{CWD: ws, SessionID: "s", Event: "UserPromptSubmit"}, ask)
	codexContextHintText(codexHookInput{CWD: ws, SessionID: "s", Event: "UserPromptSubmit"}, ask)
	if len(reqs) != 2 || reqs[0].NamespaceToken != "tok-1" || reqs[1].NamespaceToken != "tok-1" {
		t.Errorf("context-hint requests = %+v, want both carrying tok-1", reqs)
	}
}

// TestClearMarkerKey_SeparatesNamespaces: a marker set for a
// conversation in one namespace is not consumed by a connection in another.
func TestClearMarkerKey_SeparatesNamespaces(t *testing.T) {
	m := newClearMarkers()
	m.mark(clearMarkerKey("ns-1", "conv"))
	if m.take(clearMarkerKey("", "conv")) {
		t.Fatal("a default-namespace connection took another namespace's /clear marker")
	}
	if !m.take(clearMarkerKey("ns-1", "conv")) {
		t.Fatal("control: the namespace's own connection could not take its marker")
	}
}

// TestClearCommand_AnIDCannotSpellANamespacedKey: a default-namespace notice
// whose id carries ':' is dropped, so it cannot forge another namespace's
// marker key.
func TestClearCommand_AnIDCannotSpellANamespacedKey(t *testing.T) {
	var marked []string
	h := ctrlHandlers{resolveNamespace: fakeResolve, conversationCleared: func(key string) { marked = append(marked, key) }}
	server, client := net.Pipe()
	go func() { handleClearCommand(server, ctrlConversationClearedCommand+" ns-1:conv", h); server.Close() }()
	_, _ = bufio.NewReader(client).ReadString('\n')
	client.Close()
	if len(marked) != 0 {
		t.Fatalf("an id spelling a namespaced key was marked: %q", marked)
	}
}

// TestNamespaceForToken_ResolvesLiveTokensOnly runs against the real registry:
// a live token names its namespace, no token is the default namespace, and a
// released token is refused.
func TestNamespaceForToken_ResolvesLiveTokensOnly(t *testing.T) {
	r, _, _, _ := newTestNamespaces(t)
	token, ns, err := r.create(time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	conns := r.conns
	if id, ok := conns.namespaceForToken(token); !ok || id != ns.ID {
		t.Fatalf("live token = %q, %v; want %s", id, ok, ns.ID)
	}
	if id, ok := conns.namespaceForToken(""); !ok || id != "" {
		t.Fatalf("no token = %q, %v; want the default namespace", id, ok)
	}
	if _, err := r.release(token); err != nil {
		t.Fatal(err)
	}
	if _, ok := conns.namespaceForToken(token); ok {
		t.Fatal("a released token was still resolved")
	}
}

// TestHookRequestNamespace_WithoutAResolverRefusesATokenedRequest: a daemon
// that wired no resolver never answers a namespaced hook from the default
// namespace.
func TestHookRequestNamespace_WithoutAResolverRefusesATokenedRequest(t *testing.T) {
	if _, ok := hookRequestNamespace(ctrlHandlers{}, "tok-1"); ok {
		t.Fatal("a tokened request was answered with no resolver wired")
	}
	if ns, ok := hookRequestNamespace(ctrlHandlers{}, ""); !ok || ns != "" {
		t.Fatal("control: a request without a token is the default namespace")
	}
}

// TestHookPeerCount_CountsOnlyTheCallersNamespace: a session in another
// namespace on the same workspace is not a peer (it cannot write to this
// mailbox, and counting it would reveal it).
func TestHookPeerCount_CountsOnlyTheCallersNamespace(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ws := t.TempDir()
	for _, info := range []session.Info{
		{Name: "self-ns", Folder: ws, Namespace: "ns-1"},
		{Name: "peer-ns", Folder: ws, Namespace: "ns-1"},
		{Name: "self-default", Folder: ws},
		{Name: "peer-default", Folder: ws},
		{Name: "other-default", Folder: ws},
	} {
		reg, err := session.Register(info)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { session.Unregister(reg.ID) })
	}
	if got := hookPeerCount(ws, "self-ns"); got != 1 {
		t.Errorf("a namespaced session counts %d peers, want 1 (its own namespace's)", got)
	}
	if got := hookPeerCount(ws, "self-default"); got != 2 {
		t.Errorf("a default-namespace session counts %d peers, want 2 (none of the namespace's)", got)
	}
}

// TestHandOverOnClear_TakesOnlyItsOwnNamespacesMarker drives the connection
// side of the /clear handover: a namespaced connection takes the marker its
// namespace's hook set, and not a default-namespace one for the same id.
func TestHandOverOnClear_TakesOnlyItsOwnNamespacesMarker(t *testing.T) {
	const old, now = "conv-OLD", "conv-NEW"
	for name, c := range map[string]struct {
		markNS string
		moves  bool
	}{
		"its own namespace's marker": {"ns-1", true},
		"a default-namespace marker": {"", false},
		"another namespace's marker": {"ns-2", false},
	} {
		t.Run(name, func(t *testing.T) {
			s := newIdentitySession(t)
			s.mutate(func(v *sessionView) { v.namespace = "ns-1" })
			s.linkExternalID(stampedCtx(old), old)
			s.recordLogicalAgentCall(old)
			s.registry = newConnRegistry()
			s.registry.clears.mark(clearMarkerKey(c.markNS, now))
			s.handOverOnClear(now)
			if moved := s.externalID() == now; moved != c.moves {
				t.Fatalf("relinked = %v, want %v", moved, c.moves)
			}
		})
	}
}

// TestContextHintService_ResolvesInTheRequestsNamespace: both resolve calls
// (admission and the ledger's root) ask in the request's namespace.
func TestContextHintService_ResolvesInTheRequestsNamespace(t *testing.T) {
	f := newHintFixture(t)
	var asked []string
	f.svc.resolve = func(_, _, ns string) (hintRoot, bool) {
		asked = append(asked, ns)
		return hintRoot{}, false
	}
	req := prompt("conv-1", "Cart.Total")
	req.namespace = "ns-1"
	f.svc.serve(context.Background(), req)
	if len(asked) < 2 {
		t.Fatalf("resolve asked %d times, want both admission and the ledger root", len(asked))
	}
	for _, ns := range asked {
		if ns != "ns-1" {
			t.Fatalf("resolve asked in namespaces %q, want only ns-1", asked)
		}
	}
}

// TestContextHintState_IsPerNamespace: two namespaces reusing a session id do
// not share a hint window or remembered seeds.
func TestContextHintState_IsPerNamespace(t *testing.T) {
	f := newHintFixture(t)
	f.svc.remember(hintAgentKey("ns-1", "conv", ""), []tools.ContextSeed{{Symbol: "Mine"}})
	if got := f.svc.recall(hintAgentKey("", "conv", "")); len(got) != 0 {
		t.Fatalf("the default namespace recalled another namespace's seeds: %v", got)
	}
	if got := f.svc.recall(hintAgentKey("ns-1", "conv", "")); len(got) != 1 {
		t.Fatal("control: the namespace's own seeds were not recalled")
	}
}

// TestRegistryCtrlHandlers_ScopeHooksThroughTheRegistry pins the daemon's
// wiring: the handlers it serves resolve a hook's token through the namespace
// registry, so a live token reaches its namespace rather than being refused.
func TestRegistryCtrlHandlers_ScopeHooksThroughTheRegistry(t *testing.T) {
	r, _, _, _ := newTestNamespaces(t)
	token, ns, err := r.create(time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	h := registryCtrlHandlers(r.conns)
	if id, ok := hookRequestNamespace(h, token); !ok || id != ns.ID {
		t.Fatalf("the daemon's handlers resolved a live token to %q, %v; want %s", id, ok, ns.ID)
	}
	if h.mailbox == nil || h.conversationCleared == nil || h.namespace == nil || h.reloadProject == nil {
		t.Fatal("a registry-backed handler is unwired")
	}
}
