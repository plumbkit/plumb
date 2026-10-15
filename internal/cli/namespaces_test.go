package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/sessionstate"
)

// fakeClock is a settable clock for the namespace registry.
type fakeClock struct{ t atomic.Int64 }

func (c *fakeClock) now() time.Time          { return time.Unix(0, c.t.Load()) }
func (c *fakeClock) advance(d time.Duration) { c.t.Add(int64(d)) }

// newTestNamespaces builds a registry over a fresh store with a fake clock and
// an owner process that is alive (start 1000) until the test says otherwise.
func newTestNamespaces(t *testing.T) (*namespaceRegistry, *sessionstate.Store, *fakeClock, *atomic.Bool) {
	t.Helper()
	_, ss := newOriginStore(t)
	conns := newConnRegistry()
	r := newNamespaceRegistry(ss, conns)
	conns.namespaces = r
	clock := &fakeClock{}
	clock.t.Store(time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC).UnixNano())
	r.now = clock.now
	ownerAlive := &atomic.Bool{}
	ownerAlive.Store(true)
	r.ownerStart = func(int) (time.Time, bool) {
		if !ownerAlive.Load() {
			return time.Time{}, false
		}
		return time.UnixMilli(1000), true
	}
	return r, ss, clock, ownerAlive
}

func mustCreate(t *testing.T, r *namespaceRegistry, ttl time.Duration, owner int) (string, sessionstate.Namespace) {
	t.Helper()
	token, ns, err := r.create(ttl, owner)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return token, ns
}

func TestNamespaces_RefusalsNeverFallBack(t *testing.T) {
	r, _, clock, _ := newTestNamespaces(t)
	token, ns := mustCreate(t, r, time.Hour, 0)

	if got, err := r.gate(token, "psid-a"); err != nil || got != ns.ID {
		t.Fatalf("gate(valid) = %q, %v; want %s", got, err, ns.ID)
	}
	if _, err := r.gate("pns1-not-a-token", "psid-b"); err == nil || !strings.Contains(err.Error(), "never falls back") {
		t.Fatalf("gate(unknown) err = %v, want an explicit no-fallback refusal", err)
	}
	// A namespaced proxy session that drops its token is refused, not let into
	// the default namespace.
	if _, err := r.gate("", "psid-a"); err == nil {
		t.Fatal("a namespace member reconnecting without its token was admitted")
	}
	if got, err := r.gate("", "psid-default"); err != nil || got != "" {
		t.Fatalf("a plain connection: %q, %v; want the default namespace", got, err)
	}
	clock.advance(2 * time.Hour)
	if _, err := r.gate(token, "psid-c"); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("gate(expired) err = %v", err)
	}
	if _, err := r.gate(token, "psid-c"); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("gate after expiry err = %v, want revoked", err)
	}
}

func TestNamespaces_CreateValidatesTTLAndOwner(t *testing.T) {
	r, _, _, alive := newTestNamespaces(t)
	if _, _, err := r.create(time.Second, 0); err == nil {
		t.Fatal("a 1 s TTL was accepted")
	}
	if _, ns := mustCreate(t, r, 0, 0); ns.TTL != namespaceDefaultTTL {
		t.Fatalf("default TTL = %s", ns.TTL)
	}
	alive.Store(false)
	if _, _, err := r.create(time.Hour, 4242); err == nil {
		t.Fatal("a dead owner pid was accepted")
	}
}

func TestNamespaces_UseRenewsTheTTL(t *testing.T) {
	r, ss, clock, _ := newTestNamespaces(t)
	token, ns := mustCreate(t, r, time.Hour, 0)
	if _, err := r.gate(token, "psid-a"); err != nil {
		t.Fatal(err)
	}
	clock.advance(50 * time.Minute)
	r.touch(ns.ID)
	got, _ := ss.NamespaceByID(ns.ID)
	if want := clock.now().Add(time.Hour); !got.ExpiresAt.Equal(want) {
		t.Fatalf("expiry after use = %v, want %v", got.ExpiresAt, want)
	}
	clock.advance(50 * time.Minute) // past the original expiry, inside the renewed one
	r.tick()
	if got, _ := ss.NamespaceByID(ns.ID); got.State != sessionstate.NamespaceActive {
		t.Fatal("a namespace in use expired at its original TTL")
	}
}

// TestNamespaces_OwnerGoneExpiresAfterGrace is the SIGKILL'd-runner case: no
// release ever comes, the owner hint stops matching, and the namespace expires
// a grace later; an owner that comes back inside the grace clears the mark.
func TestNamespaces_OwnerGoneExpiresAfterGrace(t *testing.T) {
	r, ss, clock, alive := newTestNamespaces(t)
	_, ns := mustCreate(t, r, 24*time.Hour, 4242)

	alive.Store(false)
	r.tick()
	if got, _ := ss.NamespaceByID(ns.ID); got.State != sessionstate.NamespaceActive || got.OwnerMismatchAt.IsZero() {
		t.Fatalf("first miss: state %s mismatch %v; want active and marked", got.State, got.OwnerMismatchAt)
	}
	alive.Store(true)
	r.tick()
	if got, _ := ss.NamespaceByID(ns.ID); !got.OwnerMismatchAt.IsZero() {
		t.Fatal("an owner that came back left its mismatch mark")
	}
	alive.Store(false)
	r.tick()
	clock.advance(namespaceOwnerGrace)
	r.tick()
	if got, _ := ss.NamespaceByID(ns.ID); got.State != sessionstate.NamespaceRevoked {
		t.Fatalf("state %s after the owner was gone for the grace, want revoked", got.State)
	}
}

// TestNamespaces_ReleaseDrainsThenPurges: release refuses the token at once,
// live connections get the drain, are cancelled, and the purge follows once
// they are gone; the default namespace's rows are untouched throughout.
func TestNamespaces_ReleaseDrainsThenPurges(t *testing.T) {
	r, ss, clock, _ := newTestNamespaces(t)
	token, ns := mustCreate(t, r, time.Hour, 0)
	if _, err := r.gate(token, "psid-ns"); err != nil {
		t.Fatal(err)
	}
	row, err := session.Register(session.Info{Folder: t.TempDir(), Namespace: ns.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := ss.SaveIdentity("psid-ns", sessionstate.Identity{Name: row.Name, SessionID: row.ID}); err != nil {
		t.Fatal(err)
	}
	if err := ss.SaveIdentity("psid-default", sessionstate.Identity{Name: "calm-otter", SessionID: "sid-default"}); err != nil {
		t.Fatal(err)
	}
	var cancelled atomic.Int32
	live := atomic.Bool{}
	live.Store(true)
	r.conns.add("conn-ns", connHandle{
		namespace: func() string { return ns.ID },
		cancel:    func() { cancelled.Add(1) },
	})
	r.conns.add("conn-default", connHandle{namespace: func() string { return "" }, cancel: func() { t.Error("a default-namespace connection was cancelled") }})

	if _, err := r.release(token); err != nil {
		t.Fatal(err)
	}
	if _, err := r.gate(token, "psid-late"); err == nil {
		t.Fatal("a released namespace's token was still accepted")
	}
	r.tick() // inside the drain: nothing cancelled, nothing purged
	if cancelled.Load() != 0 {
		t.Fatal("a connection was cancelled before the drain was over")
	}
	clock.advance(namespaceDrain)
	r.tick() // drain over: cancel; the connection is still registered, so no purge yet
	if cancelled.Load() == 0 {
		t.Fatal("the drain ended without cancelling the namespace's connection")
	}
	if _, err := ss.NamespaceByID(ns.ID); err != nil {
		t.Fatal("purged while a cancelled connection was still tearing down")
	}
	r.conns.remove("conn-ns")
	r.tick()
	if _, err := ss.NamespaceByID(ns.ID); !errors.Is(err, sessionstate.ErrNamespaceUnknown) {
		t.Fatalf("namespace survived its purge: %v", err)
	}
	if _, ok, _ := ss.LoadIdentity("psid-ns"); ok {
		t.Fatal("the member's identity survived the purge")
	}
	all, _ := session.List()
	for _, info := range all {
		if info.ID == row.ID {
			t.Fatal("the member's session row survived the purge")
		}
	}
	if _, ok, _ := ss.LoadIdentity("psid-default"); !ok {
		t.Fatal("the purge removed a default-namespace identity")
	}
}

// TestNamespaces_RestorePurgesWhatEndedWhileDown: a daemon restart keeps an
// active namespace and purges one that was revoked or expired meanwhile.
func TestNamespaces_RestorePurgesWhatEndedWhileDown(t *testing.T) {
	r, ss, clock, _ := newTestNamespaces(t)
	_, keep := mustCreate(t, r, 3*time.Hour, 0)
	_, expired := mustCreate(t, r, time.Hour, 0)
	_, revoked := mustCreate(t, r, 3*time.Hour, 0)
	if err := ss.RevokeNamespace(revoked.ID); err != nil {
		t.Fatal(err)
	}
	clock.advance(2 * time.Hour)

	fresh := newNamespaceRegistry(ss, newConnRegistry())
	fresh.now = clock.now
	fresh.restore()
	if _, err := ss.NamespaceByID(keep.ID); err != nil {
		t.Fatal("restore dropped an active namespace")
	}
	for _, gone := range []string{expired.ID, revoked.ID} {
		if _, err := ss.NamespaceByID(gone); !errors.Is(err, sessionstate.ErrNamespaceUnknown) {
			t.Fatalf("restore kept %s", gone)
		}
	}
}

// TestNamespaces_ControlCommandRoundTrip drives the ctrl-socket command the CLI
// sends: create returns a token once, list never shows it, release revokes.
func TestNamespaces_ControlCommandRoundTrip(t *testing.T) {
	r, _, _, _ := newTestNamespaces(t)
	ask := func(req namespaceRequest) namespaceReply {
		t.Helper()
		server, client := net.Pipe()
		defer client.Close()
		data, _ := json.Marshal(req)
		go func() {
			defer server.Close()
			handleNamespaceCommand(server, "namespace "+string(data), ctrlHandlers{namespace: r.handleCtrl})
		}()
		buf := make([]byte, 64<<10)
		n, _ := client.Read(buf)
		var reply namespaceReply
		if err := json.Unmarshal(buf[:n], &reply); err != nil {
			t.Fatalf("reply %q: %v", buf[:n], err)
		}
		return reply
	}
	created := ask(namespaceRequest{Op: "create", TTL: "30m"})
	if created.Error != "" || !strings.HasPrefix(created.Token, sessionstate.NamespaceTokenPrefix) || created.Namespace == nil {
		t.Fatalf("create = %+v", created)
	}
	listed := ask(namespaceRequest{Op: "list"})
	if len(listed.Namespaces) != 1 || listed.Namespaces[0].ID != created.Namespace.ID || listed.Token != "" {
		t.Fatalf("list = %+v", listed)
	}
	if got := ask(namespaceRequest{Op: "release", Token: created.Token}); got.Error != "" || got.Namespace.State != sessionstate.NamespaceRevoked {
		t.Fatalf("release = %+v", got)
	}
	if got := ask(namespaceRequest{Op: "bogus"}); got.Error == "" {
		t.Fatal("an unknown operation was accepted")
	}
}

// TestConnSession_NamespaceGateBindsAndStamps: a connection admitted to a
// namespace carries it in its view and on its session row; a refused one
// changes nothing.
func TestConnSession_NamespaceGateBindsAndStamps(t *testing.T) {
	r, ss, _, _ := newTestNamespaces(t)
	token, ns := mustCreate(t, r, time.Hour, 0)
	s := newConnSession(context.Background(), detectTestPool(), nil, config.NewStore(config.Defaults()), nil, ss, newSharedBudgets())
	t.Cleanup(s.close)
	s.registry = r.conns

	if err := s.onNamespace("pns1-bogus", "psid-x"); err == nil {
		t.Fatal("a bogus token was admitted")
	}
	if s.view().namespace != "" {
		t.Fatal("a refused gate bound a namespace")
	}
	if err := s.onNamespace(token, "psid-x"); err != nil {
		t.Fatal(err)
	}
	if s.view().namespace != ns.ID {
		t.Fatalf("view namespace = %q, want %s", s.view().namespace, ns.ID)
	}
	s.stampNamespace()
	all, err := session.List()
	if err != nil {
		t.Fatal(err)
	}
	var mine session.Info
	for _, info := range all {
		if info.ID == s.sessionID() {
			mine = info
		}
	}
	if mine.Namespace != ns.ID {
		t.Fatalf("session row namespace = %q, want %s", mine.Namespace, ns.ID)
	}
	if got := session.InNamespace(all, ""); len(got) != len(all)-1 {
		t.Fatalf("the default namespace still lists the namespaced session (%d of %d)", len(got), len(all))
	}
}

// nsTestSession builds a connection in namespace ns ("" for the default).
func nsTestSession(t *testing.T, ss *sessionstate.Store, ns string) *connSession {
	t.Helper()
	s := newConnSession(context.Background(), detectTestPool(), nil, config.NewStore(config.Defaults()), nil, ss, newSharedBudgets())
	t.Cleanup(s.close)
	s.mutate(func(v *sessionView) { v.namespace = ns })
	s.stampNamespace()
	return s
}

// TestResolvePeer_OnlyInsideTheNamespace: mail is addressed only to a peer of
// the sender's own namespace, in both directions.
func TestResolvePeer_OnlyInsideTheNamespace(t *testing.T) {
	_, ss := newOriginStore(t)
	def := nsTestSession(t, ss, "")
	nsd := nsTestSession(t, ss, "ns-aaaa")
	defName, nsName := def.view().sessName, nsd.view().sessName
	if _, ok := def.resolvePeer(nsName); ok {
		t.Fatal("the default namespace resolved a namespaced peer")
	}
	if _, ok := nsd.resolvePeer(defName); ok {
		t.Fatal("a namespace resolved a default-namespace peer")
	}
	other := nsTestSession(t, ss, "ns-aaaa")
	if _, ok := nsd.resolvePeer(other.view().sessName); !ok {
		t.Fatal("control: a peer in the same namespace did not resolve")
	}
}

// TestResumeCredential_NotAcrossNamespaces: a presented credential whose
// identity belongs to another namespace is not resumed; the same credential
// inside its own namespace reaches the claim (positive control).
func TestResumeCredential_NotAcrossNamespaces(t *testing.T) {
	r, ss, _, _ := newTestNamespaces(t)
	token, ns := mustCreate(t, r, time.Hour, 0)
	if err := ss.SaveIdentity("psid-old", sessionstate.Identity{Name: "old-wren", SessionID: "sid-old"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.gate(token, "psid-old"); err != nil { // the identity is the namespace's
		t.Fatal(err)
	}
	for _, tc := range []struct {
		ns      string
		reaches bool
	}{{"", false}, {ns.ID, true}} {
		s := nsTestSession(t, ss, tc.ns)
		reached := false
		s.resumeSeams.afterLookup = func() { reached = true }
		s.acceptResumeCredential(context.Background(),
			presentation{linkage: "conv-1", hash: "h", proxyID: "psid-new"},
			sessionstate.CredentialLookup{ProxySessionID: "psid-old"})
		if reached != tc.reaches {
			t.Fatalf("connection in %q: reached the claim = %v, want %v", tc.ns, reached, tc.reaches)
		}
	}
}

// TestBeginResume_NotAcrossNamespaces: a name resume by conversation ID never
// adopts an ended session of another namespace; inside its own it does.
func TestBeginResume_NotAcrossNamespaces(t *testing.T) {
	_, ss := newOriginStore(t)
	prev := nsTestSession(t, ss, "")
	session.SetExternalID(prev.sessionID(), "conv-resume")
	session.Unregister(prev.sessionID())

	nsd := nsTestSession(t, ss, "ns-bbbb")
	nsd.beginResume("conv-resume")
	if nsd.view().pendingResume != nil {
		t.Fatal("a namespaced connection resumed a default-namespace session")
	}
	def := nsTestSession(t, ss, "")
	def.beginResume("conv-resume")
	if def.view().pendingResume == nil {
		t.Fatal("control: a default connection did not resume its own namespace's session")
	}
}

func sessionRow(t *testing.T, id string) session.Info {
	t.Helper()
	all, err := session.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, info := range all {
		if info.ID == id {
			return info
		}
	}
	t.Fatalf("no session row %s", id)
	return session.Info{}
}

// TestRegisterHooks_NamespaceGateAndStampAreWired drives the hooks a real
// connection gets: the gate binds the namespace and the initialize meta stamps
// it on the session row.
func TestRegisterHooks_NamespaceGateAndStampAreWired(t *testing.T) {
	r, ss, _, _ := newTestNamespaces(t)
	token, ns := mustCreate(t, r, time.Hour, 0)
	s := newConnSession(context.Background(), detectTestPool(), nil, config.NewStore(config.Defaults()), nil, ss, newSharedBudgets())
	t.Cleanup(s.close)
	s.registry = r.conns
	srv := mcp.New(mcp.ServerInfo{Name: "plumb", Version: "test"})
	s.registerHooks(srv)
	if srv.OnNamespace == nil {
		t.Fatal("registerHooks wired no namespace gate")
	}
	if err := srv.OnNamespace(context.Background(), token, "psid-hooks"); err != nil {
		t.Fatal(err)
	}
	srv.InitializeMeta(context.Background())
	if got := sessionRow(t, s.sessionID()).Namespace; got != ns.ID {
		t.Fatalf("session row namespace after initialize = %q, want %s", got, ns.ID)
	}
}

// TestOnAfterTool_RenewsTheNamespace: every tool call renews its namespace.
func TestOnAfterTool_RenewsTheNamespace(t *testing.T) {
	r, ss, clock, _ := newTestNamespaces(t)
	token, ns := mustCreate(t, r, time.Hour, 0)
	s := newConnSession(context.Background(), detectTestPool(), nil, config.NewStore(config.Defaults()), nil, ss, newSharedBudgets())
	t.Cleanup(s.close)
	s.registry = r.conns
	if err := s.onNamespace(token, "psid-touch"); err != nil {
		t.Fatal(err)
	}
	clock.advance(30 * time.Minute)
	s.onAfterTool("read_file", nil, "", "", 0, false, nil, "", "")
	got, _ := ss.NamespaceByID(ns.ID)
	if want := clock.now().Add(time.Hour); !got.ExpiresAt.Equal(want) {
		t.Fatalf("expiry after a call = %v, want %v", got.ExpiresAt, want)
	}
}

// TestAgentRosterRow_CarriesTheNamespace: a logical agent's own roster row on a
// namespaced connection belongs to that namespace.
func TestAgentRosterRow_CarriesTheNamespace(t *testing.T) {
	_, ss := newOriginStore(t)
	s := nsTestSession(t, ss, "ns-cccc")
	sh := &agentShard{id: "agent-ns"}
	s.registerAgentRow(sh, freshTempDir(t), "go")
	sh.mu.RLock()
	id := sh.rosterID
	sh.mu.RUnlock()
	if id == "" {
		t.Fatal("control: no roster row was registered")
	}
	if got := sessionRow(t, id).Namespace; got != "ns-cccc" {
		t.Fatalf("agent roster row namespace = %q, want ns-cccc", got)
	}
}

// TestActivePeerNames_OnlyTheCallersNamespace: the peer names a write warning
// cites come from the caller's namespace only.
func TestActivePeerNames_OnlyTheCallersNamespace(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ws := freshTempDir(t)
	reg := func(ns string) session.Info {
		info, err := session.Register(session.Info{Folder: ws, Namespace: ns})
		if err != nil {
			t.Fatal(err)
		}
		return info
	}
	self, peer, outsider := reg("ns-1"), reg("ns-1"), reg("")
	got := activePeerNames(ws, self.ID)
	if _, ok := got[peer.ID]; !ok {
		t.Fatalf("control: the namespace's peer is missing: %v", got)
	}
	if _, ok := got[outsider.ID]; ok {
		t.Fatalf("a default-namespace session counted as a peer: %v", got)
	}
}

// TestRunSessions_ShowsTheDefaultNamespace: `plumb sessions` lists the default
// namespace unless asked for another (or "*").
func TestRunSessions_ShowsTheDefaultNamespace(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ws := freshTempDir(t)
	def, err := session.Register(session.Info{Folder: ws})
	if err != nil {
		t.Fatal(err)
	}
	nsd, err := session.Register(session.Info{Folder: ws, Namespace: "ns-9"})
	if err != nil {
		t.Fatal(err)
	}
	run := func(ns string) string {
		t.Helper()
		oldJSON, oldNS := sessionsFlagJSON, sessionsFlagNamespace
		t.Cleanup(func() { sessionsFlagJSON, sessionsFlagNamespace = oldJSON, oldNS })
		sessionsFlagJSON, sessionsFlagNamespace = true, ns
		return captureStdout(t, func() {
			if err := runSessions(nil, nil); err != nil {
				t.Fatal(err)
			}
		})
	}
	if out := run(""); !strings.Contains(out, def.ID) || strings.Contains(out, nsd.ID) {
		t.Fatalf("default listing:\n%s", out)
	}
	if out := run("ns-9"); strings.Contains(out, def.ID) || !strings.Contains(out, nsd.ID) {
		t.Fatalf("--namespace ns-9 listing:\n%s", out)
	}
	if out := run("*"); !strings.Contains(out, def.ID) || !strings.Contains(out, nsd.ID) {
		t.Fatalf("--namespace * listing:\n%s", out)
	}
}
