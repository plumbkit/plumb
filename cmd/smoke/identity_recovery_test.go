//go:build integration

package smoke_test

// identity_recovery_test.go — PLAN-426's acceptance test, and the only one in
// the tree that can actually prove the claim.
//
// READ THIS BEFORE TRUSTING A RESULT HERE. Every component test in
// internal/cli constructs a connSession directly and calls onProxySession by
// hand. That exercises the daemon's half of recovery and nothing else: it
// cannot show that the proxy captures an identity from a real initialize
// response, replays it across a real socket, or that a serve process which
// never restarts comes back as itself. The old restart test's own comment
// admits as much — it "manually closes a connSession and invokes onSessionID
// with a supplied predecessor ID", which is the seam, not the system.
//
// So this test keeps ONE `plumb serve` process and ONE stdio connection alive
// for its whole duration, and restarts only the daemon underneath it. Nothing
// re-sends initialize; nothing calls a daemon function directly. The client
// speaks the wire protocol, and the assertions are on what comes back over it.
//
// Isolation: every child runs under an isolated HOME and XDG tree (isolatedEnv),
// with XDG_RUNTIME_DIR deliberately CLEARED so the daemon cannot land on the
// machine's shared socket and answer from the developer's real state. The
// harness stops only the daemon it spawned, by the pid file inside that tree.

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// sessionIdentity is what session_start reports about the caller, parsed back
// out of the orientation packet. Reading it from the packet rather than from a
// database is deliberate: the packet is what an agent actually sees, and the
// incident this card came from was an agent misreading exactly this text.
type sessionIdentity struct {
	name string
	id   string
}

// parseSelfIdentity pulls the "Session:  <name> (you, id <id>…)" line out of an
// orientation packet. Empty fields mean the packet did not name its reader,
// which is itself the PLAN-425 item 1 failure and is asserted against.
func parseSelfIdentity(packet string) sessionIdentity {
	for _, line := range strings.Split(packet, "\n") {
		if !strings.HasPrefix(line, "Session:") {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(line, "Session:"))
		name, tail, found := strings.Cut(rest, " (you")
		if !found {
			continue
		}
		var id string
		if _, after, ok := strings.Cut(tail, "id "); ok {
			id = strings.TrimRight(strings.TrimSuffix(strings.SplitN(after, ")", 2)[0], "…"), " ")
		}
		return sessionIdentity{name: strings.TrimSpace(name), id: id}
	}
	return sessionIdentity{}
}

// TestSmoke_SessionIdentitySurvivesDaemonRestarts is the acceptance case.
//
// One serve process, one stdio connection, one initialize, one external-ID link
// — then three daemon restarts, with the caller's own name and internal ID
// asserted identical across every one of them, and no extra session_start
// needed to make it so.
//
// Three restarts rather than one, because two cannot tell continuity from a
// single carry-forward: an implementation that resumed the predecessor but
// recorded the wrong thing passes at two and forks at three. That is the
// gradual-fork failure the card names, and one restart cannot see it.
func TestSmoke_SessionIdentitySurvivesDaemonRestarts(t *testing.T) {
	plumbBin := buildPlumb(t)
	fixture := makeFixture(t)
	tmpHome := mkTmpHome(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// The ONE serve process and the ONE connection. Neither is replaced below.
	c := newMCPClient(t, ctx, plumbBin, tmpHome, fixture)
	c.initialize(t, fixture)

	const externalID = "smoke-conversation-426"
	first := c.call(t, "session_start", map[string]any{
		"workspace":  fixture,
		"session_id": externalID,
	}, sessionStartTimeout)

	want := parseSelfIdentity(first)
	if want.name == "" {
		t.Fatalf("session_start never named its own caller — PLAN-425 item 1; packet:\n%s", first)
	}
	if want.id == "" {
		t.Fatalf("session_start named the caller without its session ID; packet:\n%s", first)
	}
	t.Logf("established identity: %s (id %s)", want.name, want.id)

	pid := waitForPID(t, tmpHome, 15*time.Second)

	for round := 1; round <= 3; round++ {
		stopDaemon(t, plumbBin, tmpHome)

		// A bare orientation call: no workspace argument, nothing that re-links
		// the conversation. If identity recovery depended on either — as it did
		// before this card — this is where it would fail.
		packet := recoverWithSessionStart(t, c, 60*time.Second)
		got := parseSelfIdentity(packet)

		newPID := waitForNewPID(t, tmpHome, pid, 20*time.Second)
		if newPID == pid {
			t.Fatalf("round %d: the daemon pid did not change (%s); nothing was actually "+
				"restarted, so this round proves nothing", round, pid)
		}
		pid = newPID

		if got.name != want.name {
			t.Fatalf("round %d: the session came back as %q, want %q — a surviving serve must "+
				"keep the name it is addressed by, or every note written to it is orphaned",
				round, got.name, want.name)
		}
		if got.id != want.id {
			t.Fatalf("round %d: the session came back under ID %q, want %q — mail is BOUND to "+
				"the ID, and a fork here strands it silently", round, got.id, want.id)
		}
		t.Logf("round %d: recovered as %s (id %s) behind a new daemon pid %s", round, got.name, got.id, pid)
	}

	// The external linkage survived too, and is resolvable by the real CLI
	// rather than only by an in-process helper. This is the fact that used to
	// live solely in an ended session file the janitor collects after 24 h.
	out := runPlumb(t, plumbBin, tmpHome, "mail", "--external-id", externalID, "--json")
	if !strings.Contains(out, want.name) {
		t.Errorf("`plumb mail --external-id %s` does not resolve to %q after three restarts; "+
			"the authorised linkage did not survive:\n%s", externalID, want.name, out)
	}

	// daemon_info must agree with session_start about who this is. Two tools
	// reporting different identities is the ambiguity that started this card.
	info := c.call(t, "daemon_info", map[string]any{}, toolTimeout)
	if !strings.Contains(info, want.name) {
		t.Errorf("daemon_info does not report the session as %q; the self-reporting tools "+
			"disagree:\n%s", want.name, info)
	}
}

// TestSmoke_ReconnectNoteDoesNotAssertARestartItCannotSee is the reporting half.
//
// The note an agent reads after a reconnect must state what the proxy actually
// observed. The specific false claim that produced this card was a note that
// read as a daemon restart and as a wholesale loss of session state, in a case
// where neither had happened.
func TestSmoke_ReconnectNoteDoesNotAssertARestartItCannotSee(t *testing.T) {
	plumbBin := buildPlumb(t)
	fixture := makeFixture(t)
	tmpHome := mkTmpHome(t)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	c := newMCPClient(t, ctx, plumbBin, tmpHome, fixture)
	c.initialize(t, fixture)
	c.call(t, "session_start", map[string]any{"workspace": fixture}, sessionStartTimeout)
	waitForPID(t, tmpHome, 15*time.Second)

	stopDaemon(t, plumbBin, tmpHome)
	packet := recoverWithSessionStart(t, c, 60*time.Second)

	// The note is ONE-SHOT and rides the first content-bearing tool result after
	// the reconnect. recoverWithSessionStart retries until one succeeds, and only
	// a SUCCESSFUL result carries content to inject into — the synthesised
	// retryable errors it discards are error responses, which injectReconnectNote
	// refuses by design. So the first successful call is necessarily the one
	// carrying the note, and its absence is a failure rather than a timing
	// accident.
	//
	// This deliberately does not skip. It is the only end-to-end assertion on the
	// note's honesty, and a skip here would let the whole claim quietly stop being
	// checked while the suite still reported green.
	if !strings.Contains(packet, "plumb-note:") {
		t.Fatalf("no reconnect note on the first successful call after the daemon was stopped; "+
			"the note is what tells an agent what just happened to its session:\n%s", packet)
	}
	if strings.Contains(packet, "your session state (read-tracking, caches, and the pinned workspace) was rebuilt") {
		t.Errorf("the note still claims session state was wholesale rebuilt, with no distinction "+
			"between the identity (durable) and the caches (genuinely rebuilt):\n%s", packet)
	}
	// The daemon really was killed here, so a restart IS what happened — the
	// note must say that rather than the neutral wording, and must not claim
	// the opposite.
	if strings.Contains(packet, "transport reconnect, not a restart") {
		t.Errorf("the note denies a restart that demonstrably occurred:\n%s", packet)
	}
	if !strings.Contains(packet, "identity") {
		t.Errorf("the note says nothing about what became of the session's identity, which is "+
			"the question an agent reading it actually has:\n%s", packet)
	}
}

// recoverWithSessionStart drives a bare session_start — no workspace, no
// session_id — until the proxy has reconnected and the daemon answers.
//
// The bareness is the point. Recovery must not depend on the caller naming a
// workspace or re-linking a conversation, because an agent that has done
// neither is exactly the one a restart catches unprepared.
func recoverWithSessionStart(t *testing.T, c *mcpClient, budget time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(budget)
	for {
		if txt, ok := c.callAllowError("session_start", map[string]any{}, toolTimeout); ok {
			return txt
		}
		if time.Now().After(deadline) {
			t.Fatal("the proxy did not recover: session_start still failing after the daemon was stopped")
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// stopDaemon stops the isolated daemon this test spawned, by the pid file inside
// its own XDG tree. It never touches the developer's daemon: `plumb stop` reads
// the pid path derived from the environment, and isolatedEnv redirects every
// directory that path is built from.
func stopDaemon(t *testing.T, plumbBin, tmpHome string) {
	t.Helper()
	stop := exec.Command(plumbBin, "stop", "--force")
	stop.Env = isolatedEnv(tmpHome)
	if out, err := stop.CombinedOutput(); err != nil {
		// Best-effort: the proxy's heartbeat may already have reaped it. The
		// pid-change assertion at the call site is what actually establishes
		// that a restart happened.
		t.Logf("plumb stop: %v\n%s", err, out)
	}
}

// runPlumb runs a plumb subcommand against the isolated tree and returns its
// combined output, failing the test only on a start error — a non-zero exit is
// returned to the caller, which usually wants to assert on the message.
func runPlumb(t *testing.T, plumbBin, tmpHome string, args ...string) string {
	t.Helper()
	cmd := exec.Command(plumbBin, args...)
	cmd.Env = isolatedEnv(tmpHome)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("plumb %s: %v", strings.Join(args, " "), err)
	}
	return string(out)
}
