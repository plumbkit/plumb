package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/collab"
	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/stats"
)

// namespacedAttached is a connection admitted to a namespace and pinned to a
// fresh git workspace, with a collab pool and a stats store of its own.
func namespacedAttached(t *testing.T, r *namespaceRegistry, token, psid string) (*connSession, string) {
	t.Helper()
	root := freshTempDir(t)
	mustGitDir(t, root)
	s := newConnSession(context.Background(), detectTestPool(), nil, config.NewStore(config.Defaults()), nil, r.store, newSharedBudgets())
	t.Cleanup(s.close)
	s.registry = r.conns
	s.collabPool = newCollabPool()
	t.Cleanup(s.collabPool.closeAll)
	if token != "" {
		if err := s.onNamespace(token, psid); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.repinWorkspace(context.Background(), "file://"+root, "", false, false); err != nil {
		t.Fatalf("pin: %v", err)
	}
	return s, root
}

// TestCollabViews_CarryTheConnectionsNamespaceAndRecordTheRoot: every store a
// namespaced connection is handed reads and writes its namespace's rows, and a
// write path records the root for the purge.
func TestCollabViews_CarryTheConnectionsNamespaceAndRecordTheRoot(t *testing.T) {
	r, ss, _, _ := newTestNamespaces(t)
	token, ns := mustCreate(t, r, time.Hour, 0)
	s, root := namespacedAttached(t, r, token, "psid-collab")

	w := s.collabStoreCreate()
	if w == nil || w.Namespace() != ns.ID {
		t.Fatalf("write store namespace = %v, want %s", w, ns.ID)
	}
	roots, _ := ss.NamespaceRoots(ns.ID)
	if len(roots) != 1 || roots[0] != root {
		t.Fatalf("recorded roots = %v, want [%s]", roots, root)
	}
	for name, st := range map[string]*collab.Store{
		"IfExists": s.collabStoreIfExists(),
		"global":   s.collabGlobalCreate(),
	} {
		if st == nil || st.Namespace() != ns.ID {
			t.Fatalf("%s store is not scoped to %s", name, ns.ID)
		}
	}
	if st, err := s.nsGlobalReader(); err != nil || st == nil || st.Namespace() != ns.ID {
		t.Fatalf("global reader = %v, %v; want scoped to %s", st, err, ns.ID)
	}
	in := s.resolvedInbox(context.Background(), false)
	if st := in.Workspace(); st == nil || st.Namespace() != ns.ID {
		t.Fatal("the inbox's workspace store is not scoped to the namespace")
	}
	if st, _ := in.WorkspaceReader(); st == nil || st.Namespace() != ns.ID {
		t.Fatal("the inbox's workspace reader is not scoped to the namespace")
	}
}

// TestPurgeNamespaceMail_RemovesOnlyTheNamespacesRows: the purge reaches the
// recorded roots and the daemon-level store, and the default rows stay.
func TestPurgeNamespaceMail_RemovesOnlyTheNamespacesRows(t *testing.T) {
	r, ss, _, _ := newTestNamespaces(t)
	token, ns := mustCreate(t, r, time.Hour, 0)
	s, root := namespacedAttached(t, r, token, "psid-purge")
	ctx := context.Background()
	note := func(st *collab.Store, body string) {
		t.Helper()
		in := collab.NoteInput{AuthorSession: "a", AuthorID: "a", Body: body, Addressee: "peer", TTL: time.Hour}
		if st.IsGlobal() {
			in.OriginWorkspace, in.TargetWorkspace = root, root
		}
		if _, err := st.PutNote(ctx, in, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	note(s.collabStoreCreate(), "ns local")
	note(s.collabGlobalCreate(), "ns global")
	base := s.collabPool.get(root)
	note(base, "default local")
	if sent, _ := s.collabPool.getGlobal().InNamespace(ns.ID).SentBy(ctx, "a", time.Now(), 10); len(sent) != 1 {
		t.Fatalf("control: %d namespaced notes in the daemon-level store before the purge, want 1", len(sent))
	}

	purgeNamespaceMail(ctx, ss, s.collabPool, ns.ID)
	count := func(st *collab.Store) int {
		n, err := st.PendingCount(ctx, collab.Claimant{Name: "peer"}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(base.InNamespace(ns.ID)); n != 0 {
		t.Fatalf("%d namespaced notes survived in the root's store", n)
	}
	g := s.collabPool.getGlobal().InNamespace(ns.ID)
	if sent, err := g.SentBy(ctx, "a", time.Now(), 10); err != nil || len(sent) != 0 {
		t.Fatalf("%d namespaced notes survived in the daemon-level store (%v)", len(sent), err)
	}
	if n := count(base); n != 1 {
		t.Fatalf("the default note count is %d after the purge, want 1", n)
	}
}

// TestStatsRows_CarryTheNamespace: a namespaced connection's tool calls are
// stored under its namespace, so the default feed never shows them.
func TestStatsRows_CarryTheNamespace(t *testing.T) {
	r, _, _, _ := newTestNamespaces(t)
	token, ns := mustCreate(t, r, time.Hour, 0)
	s, root := namespacedAttached(t, r, token, "psid-stats")
	s.statsStore = newStatsStore()
	args, _ := json.Marshal(map[string]any{"file_path": root + "/a.txt", "content": "x"})
	s.afterToolFromCtx(context.Background(), "write_file", args, "wrote", "", time.Millisecond, false, nil)
	s.statsStore.Close()
	db, err := stats.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if got, _ := db.RecentWritesByWorkspace(root, ns.ID, []string{"write_file"}, 10); len(got) != 1 {
		t.Fatalf("namespace feed has %d writes, want 1 (positive control)", len(got))
	}
	if got, _ := db.RecentWritesByWorkspace(root, "", []string{"write_file"}, 10); len(got) != 0 {
		t.Fatalf("the default feed shows %d namespaced writes", len(got))
	}
}

// TestMailWaitingNS_CountsOnlyTheNamespace: `plumb mail` counts the session's
// own namespace's mail.
func TestMailWaitingNS_CountsOnlyTheNamespace(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ws := freshTempDir(t)
	st, err := collab.Open(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if _, err := st.InNamespace("ns-1").PutNote(ctx, collab.NoteInput{AuthorSession: "a", AuthorID: "a", Body: "e2e", Addressee: "peer", TTL: time.Hour}, time.Now()); err != nil {
		t.Fatal(err)
	}
	who := collab.Claimant{Name: "peer", Workspace: ws}
	if ages, _ := mailWaitingNS(who, "", noGlobalMailbox); len(ages) != 0 {
		t.Fatalf("the default namespace counts %d namespaced notes", len(ages))
	}
	if ages, _ := mailWaitingNS(who, "ns-1", noGlobalMailbox); len(ages) != 1 {
		t.Fatalf("ns-1 counts %d of its own notes, want 1", len(ages))
	}
}

// TestHookInbox_ReadersAreTheNamespaces: the Stop hook's mail probe reads the
// connection's namespace only.
func TestHookInbox_ReadersAreTheNamespaces(t *testing.T) {
	world := newIdentityWorld(t)
	conn := world.conn("")
	root := identityRepo(t)
	conn.start("owner", root, "owner", nil)
	world.pool.acquire(root) // the probe reads an existing store only
	conn.s.mutate(func(v *sessionView) { v.namespace = "ns-hook" })
	inbox, ok := conn.s.hookInbox("owner")
	if !ok {
		t.Fatal("owner not resolved")
	}
	st, err := inbox.WorkspaceReader()
	if err != nil || st == nil || st.Namespace() != "ns-hook" {
		t.Fatalf("hook workspace reader = %v, %v; want the namespace's view", st, err)
	}
}

// TestRegisteredTools_UseTheConnectionsNamespace: the tools a connection
// registers scope by its namespace even when its session row is not (yet)
// stamped — the fail-open fallback to the row is not what they rely on.
func TestRegisteredTools_UseTheConnectionsNamespace(t *testing.T) {
	world := newIdentityWorld(t)
	conn := world.conn("")
	root := identityRepo(t)
	conn.start("owner", root, "owner", nil)
	conn.s.mutate(func(v *sessionView) { v.namespace = "ns-tools" }) // the row stays unstamped
	ws := conn.s.workspace()
	if ws == "" {
		ws = root
	}
	inNS, err := session.Register(session.Info{Folder: ws, Namespace: "ns-tools"})
	if err != nil {
		t.Fatal(err)
	}
	// A second namespaced peer: with the caller's own row unstamped, a lone
	// peer would read as the caller itself.
	if _, err := session.Register(session.Info{Folder: ws, Namespace: "ns-tools"}); err != nil {
		t.Fatal(err)
	}
	outside, err := session.Register(session.Info{Folder: ws})
	if err != nil {
		t.Fatal(err)
	}
	out, _ := conn.call("", "workspace_sessions", nil)
	if !strings.Contains(out, inNS.Name) {
		t.Fatalf("control: the namespace's peer is missing:\n%s", out)
	}
	if strings.Contains(out, outside.Name) {
		t.Fatalf("a default-namespace peer appears for a namespaced connection:\n%s", out)
	}
}

// TestStartNamespaces_WiresTheMailAndStatsPurge: the daemon's registry purges a
// namespace's mail and stats rows, not just its session state.
func TestStartNamespaces_WiresTheMailAndStatsPurge(t *testing.T) {
	_, ss := newOriginStore(t)
	conns := newConnRegistry()
	st := newStatsStore()
	pool := newCollabPool()
	t.Cleanup(pool.closeAll)
	ctx := t.Context() // the daemon's context: live while the test runs
	startNamespaces(ctx, ss, conns, st, pool)
	r := conns.namespaces
	if r == nil {
		t.Fatal("startNamespaces gave the registry no namespace registry")
	}
	_, ns, err := r.create(time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	root := freshTempDir(t)
	if err := ss.AddNamespaceRoot(ns.ID, root); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.acquire(root).InNamespace(ns.ID).PutNote(ctx, collab.NoteInput{AuthorSession: "a", AuthorID: "a", Body: "e2e", Addressee: "peer", TTL: time.Hour}, time.Now()); err != nil {
		t.Fatal(err)
	}
	st.Record(root, stats.Call{Namespace: ns.ID, SessionID: "sid", Tool: "write_file", CalledAt: time.Now(), Success: true})

	r.purge(ns.ID)
	st.Close() // drains the writer, purge included
	if n, _ := pool.get(root).InNamespace(ns.ID).PendingCount(context.Background(), collab.Claimant{Name: "peer"}, time.Now()); n != 0 {
		t.Fatalf("%d namespaced notes survived the daemon's purge", n)
	}
	db, err := stats.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if n := db.TotalCalls(stats.Filter{Namespace: ns.ID}); n != 0 {
		t.Fatalf("%d namespaced stats rows survived the daemon's purge", n)
	}
}

// TestLatestEpisodic_IsTheNamespaces: session_start's "last session" summary
// comes from the caller's namespace only.
func TestLatestEpisodic_IsTheNamespaces(t *testing.T) {
	r, _, _, _ := newTestNamespaces(t)
	token, ns := mustCreate(t, r, time.Hour, 0)
	s, root := namespacedAttached(t, r, token, "psid-epi")
	w := newStatsStore()
	w.RecordEpisodic(stats.Episodic{Workspace: root, SessionID: "d", Summary: "default work", GeneratedAt: time.Now()})
	w.RecordEpisodic(stats.Episodic{Namespace: ns.ID, Workspace: root, SessionID: "n", Summary: "e2e work", GeneratedAt: time.Now().Add(-time.Hour)})
	w.Close()
	if got, ok := s.latestEpisodic(root); !ok || got != "e2e work" {
		t.Fatalf("a namespaced session's last-session summary = %q, want its own namespace's", got)
	}
}

// TestGeneratedSummary_CarriesTheNamespace: the idle summary a namespaced
// session leaves is stored under its namespace, so the default namespace's
// "last session" never shows it.
func TestGeneratedSummary_CarriesTheNamespace(t *testing.T) {
	r, _, _, _ := newTestNamespaces(t)
	token, ns := mustCreate(t, r, time.Hour, 0)
	s, root := namespacedAttached(t, r, token, "psid-summary")
	s.statsStore = newStatsStore()
	for _, f := range []string{"a.txt", "b.txt"} {
		args, _ := json.Marshal(map[string]any{"file_path": root + "/" + f, "content": "x"})
		s.afterToolFromCtx(context.Background(), "write_file", args, "wrote", "", time.Millisecond, false, nil)
	}
	s.statsStore.Close()
	s.statsStore = newStatsStore()
	s.generateEpisodicSummary()
	s.statsStore.Close()

	db, err := stats.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, ok, _ := db.LatestEpisodic(root, ns.ID); !ok {
		t.Fatal("control: the namespaced session left no summary under its namespace")
	}
	if _, ok, _ := db.LatestEpisodic(root, ""); ok {
		t.Fatal("a namespaced session's summary is visible to the default namespace")
	}
}
