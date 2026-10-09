package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/collab"
	"github.com/plumbkit/plumb/internal/tools"
)

func mailboxTestDaemon(t *testing.T, registry *connRegistry) {
	t.Helper()
	probeTestEnv(t)
	listener, err := net.Listen("unix", daemonCtrlSocketPath())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		serveControlSocket(listener, "info", "text", ctrlHandlers{mailbox: registry.mailboxProbe})
	}()
	t.Cleanup(func() { _ = listener.Close(); <-done })
}

func registerMailboxConn(c *identityConn) {
	c.w.registry.add(c.s.sessionID(), connHandle{mailboxInbox: c.s.hookInbox})
}

func codexStopOutput(t *testing.T, sessionID string, active bool) string {
	t.Helper()
	input, err := json.Marshal(codexHookInput{Event: "Stop", SessionID: sessionID, StopHookActive: active})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runCodexHookIO(bytes.NewReader(input), &out); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestCodexMailboxAdapter_DeliveryAndBoundedContinuation(t *testing.T) {
	world := newIdentityWorld(t)
	root := identityRepo(t)
	recipient, peer := world.conn(""), world.conn("")
	recipient.start("conversation", root, "conversation", nil)
	peer.start("peer-conversation", root, "peer-conversation", nil)
	registerMailboxConn(recipient)
	mailboxTestDaemon(t, world.registry)

	if out := codexStopOutput(t, "conversation", false); out != "" {
		t.Fatalf("empty mailbox blocked Stop: %s", out)
	}
	send := func(body string) {
		t.Helper()
		if out, isErr := peer.call("peer-conversation", "leave_note", map[string]any{"to": recipient.s.sessionName(), "body": body}); isErr {
			t.Fatalf("send: %s", out)
		}
	}
	send("private peer body")
	if out := codexStopOutput(t, "conversation", true); out != "" {
		t.Fatalf("active Stop queried or blocked: %s", out)
	}
	first := codexStopOutput(t, "conversation", false)
	if !strings.Contains(first, "\"decision\":\"block\"") || strings.Contains(first, "private peer body") {
		t.Fatalf("new mail did not give a metadata-only notice: %s", first)
	}
	// Even if no read follows, the same row set cannot repeatedly keep a turn alive.
	for range 3 {
		if out := codexStopOutput(t, "conversation", false); out != "" {
			t.Fatalf("unchanged mail blocked again: %s", out)
		}
	}
	read, isErr := recipient.call("conversation", "check_messages", nil)
	if isErr || !strings.Contains(read, "private peer body") {
		t.Fatalf("hook consumed or lost delivery: %s (error=%v)", read, isErr)
	}
	read, isErr = recipient.call("conversation", "check_messages", nil)
	if isErr || strings.Contains(read, "private peer body") {
		t.Fatalf("message delivered twice: %s (error=%v)", read, isErr)
	}
	if out := codexStopOutput(t, "conversation", false); out != "" {
		t.Fatalf("delivered note blocked Stop: %s", out)
	}
	send("new arrival after empty check")
	if out := codexStopOutput(t, "conversation", false); !strings.Contains(out, "\"decision\":\"block\"") {
		t.Fatalf("new eligible state did not notify: %s", out)
	}
}

func TestCodexMailboxAdapter_UsesPooledViewInsteadOfStaleDisk(t *testing.T) {
	world := newIdentityWorld(t)
	root := identityRepo(t)
	recipient := world.conn("")
	recipient.start("conversation", root, "conversation", nil)
	registerMailboxConn(recipient)
	offline, err := collab.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = offline.Close() })
	_, err = offline.PutNote(context.Background(), collab.NoteInput{
		AuthorSession: "peer", AuthorID: "peer-id", Addressee: recipient.s.sessionName(),
		AddresseeID: recipient.s.sessionID(), Body: "stale main fixture", TTL: time.Hour,
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// Inject the incident's disagreement using two isolated stores, never by
	// unlinking live SQLite sidecars or relying on platform lock loss.
	active, err := collab.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	world.pool.stores[root] = active
	mailboxTestDaemon(t, world.registry)
	if report, ok := hookMailReport("conversation", root); !ok || report.Count != 0 {
		t.Fatalf("hook reopened stale disk view: report=%+v ok=%v", report, ok)
	}
	if out := codexStopOutput(t, "conversation", false); out != "" {
		t.Fatalf("stale main continued the turn: %s", out)
	}
	read, isErr := recipient.call("conversation", "check_messages", nil)
	if isErr || !strings.Contains(read, "No messages") {
		t.Fatalf("authoritative empty read disagreed: %s error=%v", read, isErr)
	}
	rows, err := offline.ClaimableNotes(context.Background(), collab.Claimant{
		Name: recipient.s.sessionName(), ID: recipient.s.sessionID(), Workspace: root,
	}, time.Now(), 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("hook altered offline fixture: rows=%d err=%v", len(rows), err)
	}
}

func TestCodexMailboxAdapter_UnavailableAndReadFailureAllowStop(t *testing.T) {
	world := newIdentityWorld(t)
	root := identityRepo(t)
	recipient := world.conn("")
	recipient.start("conversation", root, "conversation", nil)
	registerMailboxConn(recipient)
	mailboxTestDaemon(t, world.registry)
	for _, id := range []string{"", "unregistered", recipient.s.sessionName()} {
		if out := codexStopOutput(t, id, false); out != "" {
			t.Fatalf("unproven recipient %q blocked Stop: %s", id, out)
		}
	}
	store := world.pool.acquire(root)
	_ = store.Close()
	if out := codexStopOutput(t, "conversation", false); out != "" {
		t.Fatalf("unhealthy store blocked Stop: %s", out)
	}
	read, isErr := recipient.call("conversation", "check_messages", nil)
	if !isErr || strings.Contains(read, "No messages") {
		t.Fatalf("explicit failed read claimed emptiness: %s error=%v", read, isErr)
	}
}

func TestMailboxProbe_AmbiguousRecipientNeverQueries(t *testing.T) {
	registry := newConnRegistry()
	resolve := func(string) (tools.Inbox, bool) {
		return tools.Inbox{
			Self: "alice", SelfID: "id", Root: "/ws",
			WorkspaceReader: func() (*collab.Store, error) { t.Error("ambiguous recipient queried"); return nil, nil },
			Policy:          tools.CollabPolicy{Mailbox: true},
		}, true
	}
	registry.add("one", connHandle{mailboxInbox: resolve})
	registry.add("two", connHandle{mailboxInbox: resolve})
	if _, err := registry.mailboxProbe(context.Background(), hookMailboxRequest{SessionID: "conversation"}); err == nil {
		t.Fatal("ambiguous identity accepted")
	}
}

func TestMailboxNotice_ConcurrentAndBounded(t *testing.T) {
	registry := newConnRegistry()
	var group sync.WaitGroup
	notices := make(chan bool, 20)
	for range 20 {
		group.Go(func() { notices <- registry.shouldNotifyMailbox("same recipient", "same state") })
	}
	group.Wait()
	close(notices)
	count := 0
	for notify := range notices {
		if notify {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("concurrent unchanged notices=%d, want 1", count)
	}
	for i := range maxStopMailStates {
		registry.shouldNotifyMailbox(strings.Repeat("x", i+1), "state")
	}
	if registry.shouldNotifyMailbox("overflow", "state") || len(registry.stopMailSeen) != maxStopMailStates {
		t.Fatal("notification observation state exceeded its bound")
	}
}

func TestCodexMailboxAdapter_NoDaemonAllowsStop(t *testing.T) {
	probeTestEnv(t)
	if out := codexStopOutput(t, "conversation", false); out != "" {
		t.Fatalf("missing daemon blocked: %s", out)
	}
}

func TestCodexMailboxAdapter_TimeoutAllowsStop(t *testing.T) {
	probeTestEnv(t)
	released := make(chan struct{})
	fakeCtrlDaemon(t, func(conn net.Conn, line string) {
		defer close(released)
		handleMailboxCommand(conn, line, ctrlHandlers{
			mailbox: func(ctx context.Context, _ hookMailboxRequest) (hookMailboxReply, error) {
				<-ctx.Done()
				return hookMailboxReply{}, ctx.Err()
			},
		})
	})
	start := time.Now()
	if out := codexStopOutput(t, "conversation", false); out != "" {
		t.Fatalf("timed-out daemon blocked: %s", out)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("probe exceeded its bound: %s", elapsed)
	}
	<-released
}

func TestMailboxProbe_OverlappingStopsObserveInOrder(t *testing.T) {
	registry := newConnRegistry()
	store, err := collab.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	entered, release := make(chan struct{}), make(chan struct{})
	var reads atomic.Int32
	registry.add("recipient", connHandle{mailboxInbox: func(string) (tools.Inbox, bool) {
		return tools.Inbox{
			Self: "alice", SelfID: "alice-id", Root: "/ws", Policy: tools.CollabPolicy{Mailbox: true},
			WorkspaceReader: func() (*collab.Store, error) {
				if reads.Add(1) == 1 {
					close(entered)
					<-release
				}
				return store, nil
			},
		}, true
	}})
	request := hookMailboxRequest{SessionID: "conversation", Stop: true}
	first := make(chan hookMailboxReply, 1)
	go func() {
		reply, probeErr := registry.mailboxProbe(context.Background(), request)
		if probeErr != nil {
			t.Error(probeErr)
		}
		first <- reply
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := registry.mailboxProbe(ctx, request); !errors.Is(err, context.DeadlineExceeded) || reads.Load() != 1 {
		t.Fatalf("overlapping Stop crossed the observation gate: reads=%d err=%v", reads.Load(), err)
	}
	_, err = store.PutNote(context.Background(), collab.NoteInput{
		AuthorSession: "peer", AuthorID: "peer-id", Addressee: "alice", AddresseeID: "alice-id",
		Body: "new arrival while probe is delayed", TTL: time.Hour,
	}, time.Now())
	close(release)
	if err != nil {
		t.Fatal(err)
	}
	if reply := <-first; !reply.Notify || reply.Report.Count != 1 {
		t.Fatalf("delayed probe lost new arrival: %+v", reply)
	}
	reply, err := registry.mailboxProbe(context.Background(), request)
	if err != nil || reply.Notify || reply.Report.Count != 1 {
		t.Fatalf("unchanged new state notified twice: %+v err=%v", reply, err)
	}
}

func TestCodexMailboxAdapter_OlderDaemonAllowsStop(t *testing.T) {
	probeTestEnv(t)
	fakeCtrlDaemon(t, func(conn net.Conn, _ string) {
		fmt.Fprintln(conn, "error: unknown command")
	})
	if out := codexStopOutput(t, "conversation", false); out != "" {
		t.Fatalf("older daemon blocked: %s", out)
	}
}
