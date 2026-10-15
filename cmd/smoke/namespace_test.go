//go:build integration

package smoke_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// namespace_test.go is PLAN-520's acceptance against real binaries: a namespace
// isolates an end-to-end run inside the ONE daemon (the owner's decision E2).
// It runs only in CI or `make verify-full-container` (TestMain), because it
// starts and restarts a daemon.
//
// Run with
//
//	go test -tags=integration ./cmd/smoke/ -run TestSmoke_Namespace -v

type namespaceCreated struct {
	Token     string `json:"token"`
	Namespace struct {
		ID    string `json:"id"`
		State string `json:"state"`
	} `json:"namespace"`
}

// rosterPart is workspace_sessions' output up to the caller's own outbox: the
// outbox legitimately names a session the caller wrote to by name, which is not
// that session appearing in the caller's roster.
func rosterPart(out string) string {
	if i := strings.Index(out, "your recent notes"); i >= 0 {
		return out[:i]
	}
	return out
}

func createNamespace(t *testing.T, plumbBin, tmpHome string) namespaceCreated {
	t.Helper()
	out := runPlumb(t, plumbBin, tmpHome, "namespace", "create", "--ttl", "30m", "--json")
	var c namespaceCreated
	if err := json.Unmarshal([]byte(out[strings.Index(out, "{"):]), &c); err != nil || c.Token == "" {
		t.Fatalf("plumb namespace create did not return a token: %v\n%s", err, out)
	}
	return c
}

// TestSmoke_Namespace_IsolatesAnEndToEndRun: two sessions on one workspace and
// one daemon — one default, one namespaced — cannot see or mail each other; the
// namespace survives a daemon restart; release refuses its token at once and
// purges what it held.
func TestSmoke_Namespace_IsolatesAnEndToEndRun(t *testing.T) {
	plumbBin := buildPlumb(t)
	fixture := makeFixture(t)
	tmpHome := mkTmpHome(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	def := newMCPClient(t, ctx, plumbBin, tmpHome, fixture)
	def.initialize(t, fixture)
	defName := parseSelfIdentity(def.call(t, "session_start", map[string]any{"workspace": fixture, "session_id": "ns-smoke-default"}, sessionStartTimeout)).name

	ns := createNamespace(t, plumbBin, tmpHome)
	nsc := newMCPClient(t, ctx, plumbBin, tmpHome, fixture, "PLUMB_NAMESPACE="+ns.Token)
	nsc.initialize(t, fixture)
	nsName := parseSelfIdentity(nsc.call(t, "session_start", map[string]any{"workspace": fixture, "session_id": "ns-smoke-ns"}, sessionStartTimeout)).name
	if defName == "" || nsName == "" {
		t.Fatalf("sessions unnamed: default %q, namespaced %q", defName, nsName)
	}

	// Rosters: each sees itself (the control), never the other.
	if out := def.call(t, "workspace_sessions", map[string]any{}, toolTimeout); !strings.Contains(out, defName) || strings.Contains(rosterPart(out), nsName) {
		t.Fatalf("the default roster must show %s and not %s:\n%s", defName, nsName, out)
	}
	if out := nsc.call(t, "workspace_sessions", map[string]any{}, toolTimeout); !strings.Contains(out, nsName) || strings.Contains(rosterPart(out), defName) {
		t.Fatalf("the namespaced roster must show %s and not %s:\n%s", nsName, defName, out)
	}

	// The recent-writes feed: a default-namespace write shows in the default
	// roster (the control) and not in the namespace's.
	// The stats writer batches its inserts, so the row lands within a flush
	// interval, not synchronously: poll for it (bounded) before the negative
	// check, which is only meaningful once the row is known to be stored.
	def.call(t, "write_file", map[string]any{"file_path": fixture + "/ns_smoke_default.txt", "content": "default\n"}, toolTimeout)
	var feed string
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(250 * time.Millisecond) {
		if feed = def.call(t, "workspace_sessions", map[string]any{}, toolTimeout); strings.Contains(feed, "ns_smoke_default.txt") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("control: the default feed does not show its own write:\n%s", feed)
		}
	}
	if out := nsc.call(t, "workspace_sessions", map[string]any{}, toolTimeout); strings.Contains(out, "ns_smoke_default.txt") {
		t.Fatalf("the namespaced feed shows a default-namespace write:\n%s", out)
	}

	// Mail by name does not cross, in either direction; inside the namespace it
	// is delivered (the control: a second session in the same namespace).
	const crossBody = "namespace-crossing note"
	assertContains(t, "ns->default leave_note", nsc.call(t, "leave_note", map[string]any{"to": defName, "body": crossBody + " ns->default"}, toolTimeout), "Message sent")
	assertContains(t, "default->ns leave_note", def.call(t, "leave_note", map[string]any{"to": nsName, "body": crossBody + " default->ns"}, toolTimeout), "Message sent")
	if got := def.call(t, "check_messages", map[string]any{}, toolTimeout); strings.Contains(got, crossBody) {
		t.Fatalf("the default session received a namespaced session's note:\n%s", got)
	}
	if got := nsc.call(t, "check_messages", map[string]any{}, toolTimeout); strings.Contains(got, crossBody) {
		t.Fatalf("the namespaced session received a default session's note:\n%s", got)
	}
	peer := newMCPClient(t, ctx, plumbBin, tmpHome, fixture, "PLUMB_NAMESPACE="+ns.Token)
	peer.initialize(t, fixture)
	peer.call(t, "session_start", map[string]any{"workspace": fixture, "session_id": "ns-smoke-peer"}, sessionStartTimeout)
	const sameBody = "same-namespace note"
	assertContains(t, "peer leave_note", peer.call(t, "leave_note", map[string]any{"to": nsName, "body": sameBody}, toolTimeout), "Message sent")
	if got := nsc.call(t, "check_messages", map[string]any{}, toolTimeout); !strings.Contains(got, sameBody) {
		t.Fatalf("control: a note inside the namespace was not delivered:\n%s", got)
	}

	// `plumb sessions` shows the default namespace unless asked.
	if out := runPlumb(t, plumbBin, tmpHome, "sessions", "--all"); strings.Contains(out, nsName) {
		t.Fatalf("`plumb sessions` lists the namespaced session:\n%s", out)
	}
	if out := runPlumb(t, plumbBin, tmpHome, "sessions", "--all", "--namespace", ns.Namespace.ID); !strings.Contains(out, nsName) {
		t.Fatalf("`plumb sessions --namespace %s` does not list its session:\n%s", ns.Namespace.ID, out)
	}

	// A daemon restart: the namespace and its connection come back.
	pid := waitForPID(t, tmpHome, 15*time.Second)
	stopDaemon(t, tmpHome)
	recoverWithSessionStart(t, nsc, 60*time.Second)
	if newPID := waitForNewPID(t, tmpHome, pid, 20*time.Second); newPID == pid {
		t.Fatal("the daemon pid did not change; nothing was restarted")
	}
	if out := runPlumb(t, plumbBin, tmpHome, "namespace", "list"); !strings.Contains(out, ns.Namespace.ID) || !strings.Contains(out, "active") {
		t.Fatalf("the namespace did not survive the daemon restart:\n%s", out)
	}
	if out := nsc.call(t, "workspace_sessions", map[string]any{}, toolTimeout); strings.Contains(rosterPart(out), defName) {
		t.Fatalf("after the restart, the namespaced roster lists the default session:\n%s", out)
	}

	// Release: the token is refused at once for a new connection, with no fallback.
	if out := runPlumb(t, plumbBin, tmpHome, "namespace", "release", ns.Token); !strings.Contains(out, ns.Namespace.ID) || !strings.Contains(out, "revoked") {
		t.Fatalf("plumb namespace release did not report the revoked namespace:\n%s", out)
	}
	late := newMCPClient(t, ctx, plumbBin, tmpHome, fixture, "PLUMB_NAMESPACE="+ns.Token)
	id, err := late.send("initialize", map[string]any{
		"protocolVersion": "2024-11-05", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "smoke-test", "version": "0.0.1"},
	})
	if err != nil {
		t.Fatalf("send initialize: %v", err)
	}
	msg, err := late.recv(id, 15*time.Second)
	if err != nil {
		t.Fatalf("recv initialize: %v", err)
	}
	if msg.Error == nil || !strings.Contains(msg.Error.Message, "revoked") {
		t.Fatalf("a released namespace's token was not refused at initialize: %+v", msg)
	}

	// The drain (30 s), the cancel and the purge (a later janitor tick, 30 s).
	deadline := time.Now().Add(150 * time.Second)
	for {
		out := runPlumb(t, plumbBin, tmpHome, "namespace", "list")
		if !strings.Contains(out, ns.Namespace.ID) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the released namespace was never purged:\n%s", out)
		}
		time.Sleep(5 * time.Second)
	}
	if out := runPlumb(t, plumbBin, tmpHome, "sessions", "--all", "--namespace", ns.Namespace.ID); strings.Contains(out, nsName) {
		t.Fatalf("the purged namespace's session row survived:\n%s", out)
	}
	// The default session is untouched throughout.
	if out := def.call(t, "workspace_sessions", map[string]any{}, toolTimeout); !strings.Contains(out, defName) {
		t.Fatalf("the default session lost its own roster entry:\n%s", out)
	}
}
