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
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
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
	first, firstMeta := c.callWithMeta(t, "session_start", map[string]any{
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
	// The packet abbreviates the ID; mail binds to the FULL one, so the rounds
	// below assert identity on the _meta form and only sanity-check the prefix
	// against the text an agent actually reads.
	fullID := fullSessionID(t, firstMeta)
	if !strings.HasPrefix(fullID, want.id) {
		t.Fatalf("the packet's abbreviated ID %q is not a prefix of the full _meta ID %q — the two "+
			"channels report different identities", want.id, fullID)
	}
	t.Logf("established identity: %s (id %s)", want.name, fullID)
	visible := []string{first}

	pid := waitForPID(t, tmpHome, 15*time.Second)

	for round := 1; round <= 3; round++ {
		stopDaemon(t, tmpHome)

		// A bare orientation call: no workspace argument, nothing that re-links
		// the conversation. If identity recovery depended on either — as it did
		// before this card — this is where it would fail.
		packet, meta := recoverWithSessionStart(t, c, 60*time.Second)
		got := parseSelfIdentity(packet)
		gotFull := fullSessionID(t, meta)
		visible = append(visible, packet)

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
		if gotFull != fullID {
			t.Fatalf("round %d: the full session ID is %q, want %q — the abbreviated packet check "+
				"above can agree while the identity itself forked, and mail binds to the full ID",
				round, gotFull, fullID)
		}
		t.Logf("round %d: recovered as %s (id %s) behind a new daemon pid %s", round, got.name, gotFull, pid)
	}

	// The external linkage survived too, and is resolvable by the real CLI
	// rather than only by an in-process helper. This is the fact that used to
	// live solely in an ended session file the janitor collects after 24 h.
	out := runPlumb(t, plumbBin, tmpHome, "mail", "--external-id", externalID, "--json")
	visible = append(visible, out)
	if !strings.Contains(out, want.name) {
		t.Errorf("`plumb mail --external-id %s` does not resolve to %q after three restarts; "+
			"the authorised linkage did not survive:\n%s", externalID, want.name, out)
	}

	// daemon_info must agree with session_start about who this is. Two tools
	// reporting different identities is the ambiguity that started this card.
	info := c.call(t, "daemon_info", map[string]any{}, toolTimeout)
	visible = append(visible, info)
	if !strings.Contains(info, want.name) {
		t.Errorf("daemon_info does not report the session as %q; the self-reporting tools "+
			"disagree:\n%s", want.name, info)
	}

	// And nothing the client saw may carry the proxy session credential. It is
	// the one secret in this scenario — the 122-bit value a surviving serve
	// replays to prove itself — and it is UUID-shaped, which nothing else in
	// this test's output legitimately is.
	for i, out := range visible {
		assertNoCredentialLeak(t, fmt.Sprintf("client-visible output #%d", i), out)
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

	stopDaemon(t, tmpHome)
	packet, _ := recoverWithSessionStart(t, c, 60*time.Second)

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
func recoverWithSessionStart(t *testing.T, c *mcpClient, budget time.Duration) (string, map[string]any) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for {
		if txt, meta, ok := c.callAllowErrorMeta("session_start", map[string]any{}, toolTimeout); ok {
			return txt, meta
		}
		if time.Now().After(deadline) {
			t.Fatal("the proxy did not recover: session_start still failing after the daemon was stopped")
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// stopDaemon stops the isolated daemon this test spawned, by signalling the PID
// in its own tree's pid file — and nothing else.
//
// It deliberately does NOT shell out to `plumb stop`. That command's third
// discovery strategy is an unscoped `pgrep -f "plumb daemon"`, which finds every
// daemon on the machine no matter which XDG tree it belongs to, so running it
// under isolatedEnv also SIGTERMs the developer's live daemon. That is not a
// theoretical hazard: a review run of this file restarted the operator's real
// daemon three times and stretched one test from 15s to 347s, because the
// daemon it kept killing was serving eleven other sessions.
//
// Card §7 makes harness isolation a hard precondition — "clean up only
// harness-owned PIDs/paths" — so the pid file is the only correct source here.
// The scoping bug in `plumb stop` itself is fixed separately; this does not
// depend on that fix, and should not, because a test must own its blast radius
// rather than borrow it from a command's current behaviour.
func stopDaemon(t *testing.T, tmpHome string) {
	t.Helper()
	proc, n, ok := signalIsolatedDaemon(t, tmpHome)
	if !ok {
		return
	}
	// WAIT for it to actually go. `plumb stop` blocked until the process exited,
	// and dropping that wait quietly broke the callers: SIGTERM is asynchronous,
	// so the very next tool call could still be answered by the daemon we just
	// signalled — no reconnect, no reconnect note, and an assertion failing for a
	// reason that has nothing to do with what it is testing.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if err := proc.Signal(syscall.Signal(0)); err != nil {
			return // gone
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("isolated daemon %d did not exit within 20s of SIGTERM; the reconnect this test "+
		"depends on never happened", n)
}

// stopDaemonBestEffort is stopDaemon for TEARDOWN: it signals and returns
// without waiting and without ever failing the test. A cleanup that fails the
// test it is cleaning up after turns a passing run into a confusing red one.
func stopDaemonBestEffort(t *testing.T, tmpHome string) {
	t.Helper()
	signalIsolatedDaemon(t, tmpHome)
}

// signalIsolatedDaemon SIGTERMs the daemon named by the isolated tree's own pid
// file — and nothing else. ok is false when there was nothing to signal.
//
// The pid file is the only correct source here. `plumb stop`'s discovery has a
// pgrep strategy that (before it was scoped) matched every daemon on the
// machine, and a harness must own its blast radius rather than borrow it from a
// command's current behaviour — which is also why this does not simply call the
// now-scoped `plumb stop`.
func signalIsolatedDaemon(t *testing.T, tmpHome string) (*os.Process, int, bool) {
	t.Helper()
	pid := readDaemonPID(tmpHome)
	if pid == "" {
		t.Log("no daemon pid file in the isolated tree; nothing to stop")
		return nil, 0, false
	}
	n, err := strconv.Atoi(pid)
	if err != nil || n <= 0 {
		t.Errorf("isolated daemon pid file holds %q, which is not a pid — refusing to signal anything", pid)
		return nil, 0, false
	}
	proc, err := os.FindProcess(n)
	if err != nil {
		t.Logf("isolated daemon pid %d not found: %v", n, err)
		return nil, 0, false
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		// Best-effort: the proxy's heartbeat may already have reaped it. The
		// pid-change assertion at the call site is what actually establishes
		// that a restart happened.
		t.Logf("signalling isolated daemon %d: %v", n, err)
		return nil, 0, false
	}
	return proc, n, true
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

// callAllowErrorMeta is callAllowError, also returning the result's _meta on
// success. Used across restarts, where transient failures are expected and
// retried, and the identity assertions need the full ID the packet only
// abbreviates.
func (c *mcpClient) callAllowErrorMeta(toolName string, args map[string]any, timeout time.Duration) (string, map[string]any, bool) {
	id, err := c.send("tools/call", map[string]any{"name": toolName, "arguments": args})
	if err != nil {
		return "", nil, false
	}
	msg, err := c.recv(id, timeout)
	if err != nil || msg.Error != nil {
		return "", nil, false
	}
	text, isErr, meta, derr := decodeToolResultFull(msg.Result)
	if derr != nil || isErr {
		return "", nil, false
	}
	return text, meta, true
}

// fullSessionID extracts the full internal session ID from a session_start
// result's _meta. The wire key is mcp.MetaSessionIDKey; it is spelled literally
// here because the tests assert the wire contract, not the Go constant. Mail
// binds to the full ID, so identity assertions run on it rather than on the
// abbreviation the packet prints.
func fullSessionID(t *testing.T, meta map[string]any) string {
	t.Helper()
	if len(meta) == 0 {
		t.Fatal("session_start returned no _meta; the full session ID travels there (mcp.MetaSessionIDKey)")
	}
	id, _ := meta["dev.plumbkit/session-id"].(string)
	if id == "" {
		t.Fatalf("session_start _meta carries no full session ID; meta: %v", meta)
	}
	return id
}

// uuidShape matches RFC-4122 tokens. In these scenarios the only UUID in play
// is the proxy session credential — the secret a surviving serve replays to
// prove itself — which must never reach client-visible output.
var uuidShape = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

// assertNoCredentialLeak fails when a UUID-shaped token appears in
// client-visible output. Every identity scenario below funnels its tool
// results, packets and CLI output through this.
func assertNoCredentialLeak(t *testing.T, label, out string) {
	t.Helper()
	if leaked := uuidShape.FindString(out); leaked != "" {
		t.Errorf("%s: client-visible output contains a UUID-shaped token (%q…). The only UUID in "+
			"play is the proxy session credential, and it must never appear in any tool result, "+
			"packet, or CLI output:\n%s", label, leaked, out)
	}
}

// retryCall drives a tool call across a daemon restart: the first attempt may
// land in the reconnect window and surface the proxy's retryable error.
func retryCall(t *testing.T, c *mcpClient, tool string, args map[string]any, budget time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(budget)
	for {
		if txt, ok := c.callAllowError(tool, args, toolTimeout); ok {
			return txt
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s still failing after %s", tool, budget)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// TestSmoke_ServeReplacementResumesByName is the machine-reboot case, and the
// reason the two restart tests above are not the whole story: a reboot kills
// the serve proxy TOO, so no proxy credential survives and the daemon-restart
// restore cannot fire. Continuity then rests entirely on the external linkage:
// the new serve process presents the same conversation ID and takes back the
// NAME its predecessor answered to. The internal session ID does NOT come
// back — that would mean the credential boundary leaked — and the packet must
// say the caller resumed rather than silently handing back the name.
func TestSmoke_ServeReplacementResumesByName(t *testing.T) {
	plumbBin := buildPlumb(t)
	fixture := makeMarkerFixture(t)
	tmpHome := mkTmpHome(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	const externalID = "smoke-reboot-426"
	first := newMCPClient(t, ctx, plumbBin, tmpHome, fixture)
	first.initialize(t, fixture)
	packet1, meta1 := first.callWithMeta(t, "session_start", map[string]any{
		"workspace":  fixture,
		"session_id": externalID,
	}, sessionStartTimeout)
	want := parseSelfIdentity(packet1)
	full1 := fullSessionID(t, meta1)
	if want.name == "" || full1 == "" {
		t.Fatalf("the first serve never established an identity; packet:\n%s", packet1)
	}
	waitForPID(t, tmpHome, 15*time.Second)

	// Pre-restart sanity: the linkage must already resolve through the real CLI,
	// so a resume failure below indicts the resume path, not the send.
	if pre := runPlumb(t, plumbBin, tmpHome, "mail", "--external-id", externalID, "--json"); !strings.Contains(pre, want.name) {
		t.Fatalf("the linkage did not resolve even before the replacement (mail --external-id → %q, want %q); "+
			"the resume below never had a chance:\n%s", strings.TrimSpace(pre), want.name, pre)
	}

	// Kill the first serve process AND the daemon the way a reboot does: no
	// graceful handover, no credential replay — both gone together. Killing the
	// serve alone would leave the daemon holding the session for its reconnect
	// grace (the idle-eviction window), and the replacement would then be a
	// live-name collision rather than the reboot's resume-from-ended state.
	first.cancel()
	stopDaemon(t, tmpHome)

	second := newMCPClient(t, ctx, plumbBin, tmpHome, fixture)
	second.initialize(t, fixture)
	packet2, meta2 := second.callWithMeta(t, "session_start", map[string]any{
		"workspace":  fixture,
		"session_id": externalID,
	}, sessionStartTimeout)
	t.Logf("successor packet:\n%s", packet2)
	got := parseSelfIdentity(packet2)
	full2 := fullSessionID(t, meta2)

	if got.name != want.name {
		t.Fatalf("the same conversation came back as %q, want its predecessor's name %q — a serve "+
			"replacement must not cost the session the address its mail was sent to", got.name, want.name)
	}
	if !strings.Contains(packet2, "— resumed") {
		t.Errorf("the packet does not say the caller resumed; an agent handed its old name back "+
			"without being told it is a continuation cannot tell that from coincidence:\n%s", packet2)
	}
	if full2 == full1 {
		t.Fatalf("the replacement serve recovered the internal session ID %q — only the proxy "+
			"credential may restore an ID, and no credential survived the replacement", full2)
	}

	// And the linkage is resolvable by the real CLI, not only in-process.
	out := runPlumb(t, plumbBin, tmpHome, "mail", "--external-id", externalID, "--json")
	if !strings.Contains(out, want.name) {
		t.Errorf("`plumb mail --external-id %s` does not resolve to %q after the serve replacement; "+
			"the linkage did not carry across:\n%s", externalID, want.name, out)
	}
	assertNoCredentialLeak(t, "serve-replacement outputs", packet1+packet2+out)
}

// TestSmoke_ServeReplacementWithoutLinkStartsAFreshIdentity pins the honest
// half of the reboot case: a replacement serve whose conversation never linked
// gets a fresh identity — different name, different ID — and the packet warns
// that the session has no external id. Asserting the FORK is the point: the
// alternative, inheriting a predecessor's name without its linkage, is the
// name-reuse hole the reservation system exists to keep shut.
func TestSmoke_ServeReplacementWithoutLinkStartsAFreshIdentity(t *testing.T) {
	plumbBin := buildPlumb(t)
	fixture := makeMarkerFixture(t)
	tmpHome := mkTmpHome(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	first := newMCPClient(t, ctx, plumbBin, tmpHome, fixture)
	first.initialize(t, fixture)
	packet1, meta1 := first.callWithMeta(t, "session_start", map[string]any{
		"workspace": fixture,
	}, sessionStartTimeout)
	want := parseSelfIdentity(packet1)
	full1 := fullSessionID(t, meta1)
	waitForPID(t, tmpHome, 15*time.Second)

	// Same reboot shape as the linked case: the serve and the daemon die
	// together, so the predecessor's session ends deterministically instead of
	// waiting out the daemon's reconnect grace for a proxy that is never coming
	// back.
	first.cancel()
	stopDaemon(t, tmpHome)

	second := newMCPClient(t, ctx, plumbBin, tmpHome, fixture)
	second.initialize(t, fixture)
	packet2, meta2 := second.callWithMeta(t, "session_start", map[string]any{
		"workspace": fixture,
	}, sessionStartTimeout)
	got := parseSelfIdentity(packet2)
	full2 := fullSessionID(t, meta2)

	if got.name == want.name {
		t.Fatalf("an unlinked replacement serve inherited the predecessor's name %q; name "+
			"inheritance requires the conversation linkage, and any weaker basis is the "+
			"name-reuse hole", want.name)
	}
	if full2 == full1 {
		t.Fatalf("an unlinked replacement serve recovered the internal session ID %q; without a "+
			"linkage there is nothing to resume and the identity must be fresh", full2)
	}
	if !strings.Contains(packet2, "no external id") {
		t.Errorf("the packet does not warn that the session has no external id; an agent left "+
			"unlinked is one client restart away from losing its identity and must be told:\n%s", packet2)
	}
	assertNoCredentialLeak(t, "unlinked-replacement outputs", packet1+packet2)
}

// TestSmoke_MailBoundIdentitySurvivesDaemonRestarts is the continuity promise
// that makes identity restoration worth having: mail BOUND to a session ID —
// which is what stops a name-reuser reading it — still delivers to the
// restored session, replies flow back, and nothing is delivered twice or to
// the wrong side. Three restarts, because a single carry-forward can pass
// where a repeated one forks (the sibling identity test's reasoning, applied
// to mail).
func TestSmoke_MailBoundIdentitySurvivesDaemonRestarts(t *testing.T) {
	plumbBin := buildPlumb(t)
	fixture := makeMarkerFixture(t)
	tmpHome := mkTmpHome(t)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	receiver := newMCPClient(t, ctx, plumbBin, tmpHome, fixture)
	receiver.initialize(t, fixture)
	sender := newMCPClient(t, ctx, plumbBin, tmpHome, fixture)
	sender.initialize(t, fixture)

	const externalID = "smoke-mail-426"
	start, _ := receiver.callWithMeta(t, "session_start", map[string]any{
		"workspace":  fixture,
		"session_id": externalID,
	}, sessionStartTimeout)
	a := parseSelfIdentity(start)
	if a.name == "" {
		t.Fatalf("the receiving session was never named; packet:\n%s", start)
	}
	sStart := sender.call(t, "session_start", map[string]any{"workspace": fixture}, sessionStartTimeout)
	b := parseSelfIdentity(sStart)
	if b.name == "" {
		t.Fatalf("the sending session was never named; packet:\n%s", sStart)
	}
	waitForPID(t, tmpHome, 15*time.Second)

	var inboxes []string
	var packets []string
	for round := 1; round <= 3; round++ {
		ping := fmt.Sprintf("round-%d ping", round)
		noteOut := retryCall(t, sender, "leave_note", map[string]any{"to": a.name, "body": ping}, 30*time.Second)
		t.Logf("round %d leave_note: %s", round, noteOut)

		stopDaemon(t, tmpHome)
		packet, _ := recoverWithSessionStart(t, receiver, 60*time.Second)
		if got := parseSelfIdentity(packet); got.name != a.name {
			t.Fatalf("round %d: the session came back as %q, want %q — a fork across the restart "+
				"strands the note that was bound to the old ID, which is precisely the failure "+
				"this test exists to catch", round, got.name, a.name)
		}
		// And a LINKED session must never be told it has no external id — the
		// warning keys on persisted state, not on this call's arguments.
		if strings.Contains(packet, "no external id") {
			t.Fatalf("round %d: a linked session was warned it has no external id — the warning "+
				"is keying on the call's arguments instead of the persisted linkage:\n%s", round, packet)
		}
		packets = append(packets, packet)

		// The note may arrive through either delivery channel: appended to the
		// first post-restart tool result (the message hint claims it there, once,
		// under its watermark) or handed over by an explicit check_messages. Both
		// are the note reaching its reader; requiring the second specifically
		// would test the watermark, not the mail.
		inbox := retryCall(t, receiver, "check_messages", map[string]any{}, 30*time.Second)
		inboxes = append(inboxes, inbox)
		if !strings.Contains(packet, ping) && !strings.Contains(inbox, ping) {
			t.Fatalf("round %d: the note sent before the restart reached neither delivery "+
				"channel — recovery packet and inbox both lack %q. Mail bound to a restored "+
				"session must survive the restart:\nrecovery packet:\n%s\ninbox:\n%s",
				round, ping, packet, inbox)
		}

		reply := fmt.Sprintf("round-%d reply", round)
		receiver.call(t, "leave_note", map[string]any{"to": b.name, "body": reply}, toolTimeout)
		bInbox := retryCall(t, sender, "check_messages", map[string]any{}, 30*time.Second)
		assertContains(t, fmt.Sprintf("round %d sender inbox", round), bInbox, reply)
	}

	// No self-replay, matched on the round-specific body rather than a generic
	// word: the inbox render quotes a claimed note's body inside its own
	// template, so a generic substring matches honest output. Scan the recovery
	// packets too — a self-replay delivered at session_start would surface
	// there, never in an inbox.
	for i, inbox := range inboxes {
		if strings.Contains(inbox, fmt.Sprintf("round-%d reply", i+1)) {
			t.Fatalf("inbox %d contains this session's own outbound note — mail is being "+
				"replayed to its author:\n%s", i+1, inbox)
		}
	}
	for i, packet := range packets {
		if strings.Contains(packet, fmt.Sprintf("round-%d reply", i+1)) {
			t.Fatalf("recovery packet %d contains this session's own outbound note — mail is being "+
				"replayed to its author at session_start:\n%s", i+1, packet)
		}
	}
	assertNoCredentialLeak(t, "mail round-trip outputs", strings.Join(inboxes, "\n"))
}
