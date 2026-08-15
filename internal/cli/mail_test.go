package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/collab"
	"github.com/plumbkit/plumb/internal/session"
)

// putTestNote writes one unbound note addressed to `to` in workspace's
// collab.db, creating the store on first use, and returns the conversation it
// landed in.
func putTestNote(t *testing.T, workspace, from, to, body string) string {
	t.Helper()
	return putBoundTestNote(t, workspace, from, to, "", body)
}

// putBoundTestNote is putTestNote with the note bound to one session ID, which
// is how every note to a live peer is stored.
func putBoundTestNote(t *testing.T, workspace, from, to, toID, body string) string {
	t.Helper()
	store, err := collab.Open(workspace)
	if err != nil {
		t.Fatalf("opening collab store: %v", err)
	}
	defer store.Close()
	conv, err := store.PutNote(context.Background(), collab.NoteInput{
		AuthorSession: from,
		AuthorID:      "id-" + from,
		Body:          body,
		Addressee:     to,
		AddresseeID:   toID,
		TTL:           time.Hour,
	}, time.Now())
	if err != nil {
		t.Fatalf("putting note: %v", err)
	}
	return conv
}

// TestMailWaiting_NeverClaims is the load-bearing test of `plumb mail`.
//
// The command exists so a hook can ask, from outside a session, whether an
// agent has mail. That caller is not the recipient. If the probe set the
// delivery watermark, the row would be marked delivered to an agent that never
// saw a word of it — plumb's exactly-once guarantee inverted into
// exactly-never, and silently, because a consumed message looks exactly like a
// message nobody sent.
//
// It is pinned from BOTH ends, because either alone can pass while the property
// is broken. Repeated probing must keep reporting the message (a claim would
// make the second probe report zero), AND a subsequent real claim must still
// deliver it (the row must still be handed over, not merely still counted).
func TestMailWaiting_NeverClaims(t *testing.T) {
	ws := t.TempDir()
	putTestNote(t, ws, "peer-two", "peer-one", "the rate limiter is yours")

	for i := 1; i <= 3; i++ {
		ages, err := mailWaiting(collab.Claimant{Name: "peer-one", Workspace: ws})
		if err != nil {
			t.Fatalf("probe %d: %v", i, err)
		}
		if len(ages) != 1 {
			t.Fatalf("probe %d reported %d waiting messages, want 1 — a probe that consumes its "+
				"answer has claimed the message on behalf of an agent that never read it", i, len(ages))
		}
	}

	// The other end: the message must still be DELIVERABLE, not merely still
	// counted. A probe that marked the row delivered would leave nothing here.
	store, err := collab.Open(ws)
	if err != nil {
		t.Fatalf("reopening collab store: %v", err)
	}
	defer store.Close()
	rows, err := store.ClaimNotes(context.Background(), collab.Claimant{Name: "peer-one", Workspace: ws}, time.Now(), 0)
	if err != nil {
		t.Fatalf("claiming: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("the real claim delivered %d messages, want 1 — probing consumed the message, "+
			"so the recipient can never receive it", len(rows))
	}
	if rows[0].Body != "the rate limiter is yours" {
		t.Errorf("delivered body = %q, want the original", rows[0].Body)
	}

	// And once genuinely delivered, the probe agrees it is gone.
	ages, err := mailWaiting(collab.Claimant{Name: "peer-one", Workspace: ws})
	if err != nil {
		t.Fatalf("probe after claim: %v", err)
	}
	if len(ages) != 0 {
		t.Errorf("probe after a real claim reported %d waiting, want 0 — the probe is not reading "+
			"the same watermark delivery writes", len(ages))
	}
}

// TestMailWaiting_ReadOnlyHandleCannotWrite pins the mechanism rather than the
// behaviour above: the store `plumb mail` holds must REFUSE a write, so the
// no-claim property survives someone later wiring a different query through it.
func TestMailWaiting_ReadOnlyHandleCannotWrite(t *testing.T) {
	ws := t.TempDir()
	putTestNote(t, ws, "peer-two", "peer-one", "hello")

	store, err := collab.OpenReadOnly(ws)
	if err != nil {
		t.Fatalf("opening read-only: %v", err)
	}
	defer store.Close()

	claimant := collab.Claimant{Name: "peer-one", Workspace: ws}
	if _, err := store.ClaimNotes(context.Background(), claimant, time.Now(), 0); err == nil {
		t.Fatal("ClaimNotes succeeded through the read-only handle — mode=ro is not in force, so " +
			"nothing but caller discipline stops `plumb mail` consuming a message")
	}
}

// TestMailWaiting_NoMailboxIsNotAnError: collab.db is created lazily, so a
// workspace whose sessions never exchanged a message has none. That is "no
// mail", not a failure — a hook must not be broken by the common case.
func TestMailWaiting_NoMailboxIsNotAnError(t *testing.T) {
	ages, err := mailWaiting(collab.Claimant{Name: "peer-one", Workspace: t.TempDir()})
	if err != nil {
		t.Fatalf("workspace with no collab.db: %v", err)
	}
	if len(ages) != 0 {
		t.Errorf("got %d waiting, want 0", len(ages))
	}
}

// TestMailWaiting_ExcludesOtherAddressees checks the disclosure boundary at the
// query: a probe for one session must not count another's mail. Names are the
// address, so an over-broad filter would let anyone learn that a peer has mail.
func TestMailWaiting_ExcludesOtherAddressees(t *testing.T) {
	ws := t.TempDir()
	putTestNote(t, ws, "peer-two", "someone-else", "not for peer-one")
	putTestNote(t, ws, "peer-two", collab.AddresseeNext, "for whoever arrives")

	ages, err := mailWaiting(collab.Claimant{Name: "peer-one", Workspace: ws})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if len(ages) != 0 {
		t.Errorf("got %d waiting for peer-one, want 0 — a note to another session, or to %q, is "+
			"not this session's mail", len(ages), collab.AddresseeNext)
	}
}

// TestMailWaiting_CountsMailBoundToTheSession is the regression test for the
// claimant this probe passes. A note bound to a live recipient carries that
// session's ID in addressee_id, and the delivery predicate hands over a row
// only when it is unbound OR bound to one of the claimant's own identities. A
// probe built with an empty ID therefore matches unbound rows alone — it would
// report zero for precisely the mail the binding exists to protect, and it
// would do so silently, since "no mail" is the ordinary answer.
func TestMailWaiting_CountsMailBoundToTheSession(t *testing.T) {
	ws := t.TempDir()
	store, err := collab.Open(ws)
	if err != nil {
		t.Fatalf("opening collab store: %v", err)
	}
	if _, err := store.PutNote(context.Background(), collab.NoteInput{
		AuthorSession: "peer-two", AuthorID: "id-peer-two",
		Body:      "bound to this session",
		Addressee: "peer-one", AddresseeID: "sess-peer-one",
		TTL: time.Hour,
	}, time.Now()); err != nil {
		t.Fatalf("putting bound note: %v", err)
	}
	store.Close()

	ages, err := mailWaiting(collab.Claimant{Name: "peer-one", ID: "sess-peer-one", Workspace: ws})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if len(ages) != 1 {
		t.Fatalf("got %d waiting, want 1 — the probe must carry the session ID, or every bound "+
			"message reads as no mail at all", len(ages))
	}

	// The other half of the boundary: the binding still excludes a different
	// session answering to the same name.
	ages, err = mailWaiting(collab.Claimant{Name: "peer-one", ID: "sess-someone-else", Workspace: ws})
	if err != nil {
		t.Fatalf("probe as another session: %v", err)
	}
	if len(ages) != 0 {
		t.Errorf("got %d waiting for a session that merely holds the name, want 0", len(ages))
	}
}

// TestMailWaiting_AgesOldestFirst pins the ordering the JSON contract promises,
// since a hook applying a staleness rule reads ages_seconds[0] as the oldest.
func TestMailWaiting_AgesOldestFirst(t *testing.T) {
	ws := t.TempDir()
	store, err := collab.Open(ws)
	if err != nil {
		t.Fatalf("opening collab store: %v", err)
	}
	defer store.Close()

	now := time.Now()
	for _, age := range []time.Duration{30 * time.Second, 20 * time.Minute, 5 * time.Minute} {
		if _, err := store.PutNote(context.Background(), collab.NoteInput{
			AuthorSession: "peer-two", AuthorID: "id-peer-two",
			Body: "x", Addressee: "peer-one", TTL: time.Hour,
		}, now.Add(-age)); err != nil {
			t.Fatalf("putting note: %v", err)
		}
	}

	ages, err := mailWaiting(collab.Claimant{Name: "peer-one", Workspace: ws})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if len(ages) != 3 {
		t.Fatalf("got %d ages, want 3", len(ages))
	}
	for i := 1; i < len(ages); i++ {
		if ages[i] > ages[i-1] {
			t.Fatalf("ages %v are not oldest-first — a hook reading ages_seconds[0] as the oldest "+
				"would apply its staleness rule to the wrong message", ages)
		}
	}
}

// TestMailReport_EmptyAgesMarshalAsArray pins the JSON contract on the common
// case. A nil slice marshals as null, and `jq '.ages_seconds | length'` errors
// on null — so a consumer would break precisely when there is no mail, which is
// almost every invocation.
func TestMailReport_EmptyAgesMarshalAsArray(t *testing.T) {
	for _, ws := range []string{"", t.TempDir()} {
		ages, err := mailWaiting(collab.Claimant{Name: "peer-one", Workspace: ws})
		if err != nil {
			t.Fatalf("mailWaiting(%q): %v", ws, err)
		}
		blob, err := json.Marshal(mailReport{Session: "peer-one", AgesSeconds: ages})
		if err != nil {
			t.Fatalf("marshalling: %v", err)
		}
		if !strings.Contains(string(blob), `"ages_seconds":[]`) {
			t.Errorf("workspace %q rendered %s — ages_seconds must be an empty array, never null", ws, blob)
		}
	}
}

// TestMatchSessions_ResolvesSymlinkedWorkspace: a session's Folder is recorded
// symlink-resolved, while --workspace is whatever was typed. On macOS that
// difference is routine (/tmp -> /private/tmp), and a lexical compare would
// match nothing while looking like "no session here".
func TestMatchSessions_ResolvesSymlinkedWorkspace(t *testing.T) {
	target, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolving temp dir: %v", err)
	}
	link := filepath.Join(t.TempDir(), "link-to-repo")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	all := []session.Info{{Name: "quiet-mesa", Folder: target}}
	if got := matchSessions(all, "workspace", link); len(got) != 1 {
		t.Errorf("--workspace via a symlink matched %d sessions, want 1 — the argument is not being "+
			"canonicalised the way a session root is", len(got))
	}
}

// TestMatchByWorkspace_WalksUpToTheNearestRoot is the regression test for the
// defect that made --workspace useless in practice.
//
// plumb acquires a workspace by walking UP to a root marker, so a session pinned
// to /repo serves every directory under it — and the hook that passes its `cwd`
// is routinely somewhere like /repo/internal/cli. Exact equality answered "no
// live session matches" for a session that was live and pinned exactly there,
// and because every failure path in the recipe's hook allows the stop, the wake
// hook silently never fired.
func TestMatchByWorkspace_WalksUpToTheNearestRoot(t *testing.T) {
	root := t.TempDir()
	deep := filepath.Join(root, "internal", "cli")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatalf("creating %s: %v", deep, err)
	}
	all := []session.Info{{Name: "quiet-mesa", Folder: root}}

	for _, arg := range []string{root, deep, filepath.Join(root, "internal")} {
		got := matchSessions(all, "workspace", arg)
		if len(got) != 1 || got[0].Name != "quiet-mesa" {
			t.Errorf("--workspace %q matched %v, want quiet-mesa — a directory inside a session's "+
				"root belongs to that session", arg, mailNames(got))
		}
	}

	if got := matchSessions(all, "workspace", t.TempDir()); len(got) != 0 {
		t.Errorf("an unrelated directory matched %v, want nothing", mailNames(got))
	}
}

// TestMatchByWorkspace_NearestRootWinsWhenNested: a superproject and its
// submodule both contain the argument. Reporting both would be "ambiguous" and
// refuse, so a hook in the inner project would break the moment someone attached
// a session to the outer one. The deepest root is what plumb's own upward walk
// would have found.
func TestMatchByWorkspace_NearestRootWinsWhenNested(t *testing.T) {
	outer := t.TempDir()
	inner := filepath.Join(outer, "plumb")
	deep := filepath.Join(inner, "internal", "cli")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatalf("creating %s: %v", deep, err)
	}
	all := []session.Info{
		{Name: "outer-session", Folder: outer},
		{Name: "inner-session", Folder: inner},
	}

	got := matchSessions(all, "workspace", deep)
	if len(got) != 1 || got[0].Name != "inner-session" {
		t.Fatalf("nested roots matched %v, want only inner-session — the nearest root is the one "+
			"plumb would resolve, and reporting both refuses instead of answering", mailNames(got))
	}
	if got := matchSessions(all, "workspace", outer); len(got) != 1 || got[0].Name != "outer-session" {
		t.Errorf("the outer root matched %v, want outer-session", mailNames(got))
	}
}

// TestMatchByWorkspace_SameRootIsStillAmbiguous: nearest-wins must not paper
// over the case the refusal exists for — two agents on the SAME project, where
// picking one would wake the wrong agent.
func TestMatchByWorkspace_SameRootIsStillAmbiguous(t *testing.T) {
	root := t.TempDir()
	all := []session.Info{
		{Name: "quiet-mesa", Folder: root},
		{Name: "swift-falcon", Folder: root},
	}
	if got := matchSessions(all, "workspace", root); len(got) != 2 {
		t.Errorf("two sessions on one root matched %d, want 2 so the caller is told it is ambiguous", len(got))
	}
}

// TestMailSelector_RefusesZeroOrSeveral: the selectors answer one question three
// ways, so a call naming none is unanswerable and one naming two would need a
// precedence rule no caller could predict.
func TestMailSelector_RefusesZeroOrSeveral(t *testing.T) {
	t.Cleanup(resetMailFlags)

	resetMailFlags()
	if _, _, err := mailSelector(); err == nil {
		t.Error("no selector was accepted — the command cannot know which session to report on")
	}

	resetMailFlags()
	mailFlagSession, mailFlagExternalID = "quiet-mesa", "abc123"
	if _, _, err := mailSelector(); err == nil {
		t.Error("two selectors were accepted — resolution would depend on an unstated precedence")
	}

	resetMailFlags()
	mailFlagSession = "  quiet-mesa  "
	name, value, err := mailSelector()
	if err != nil {
		t.Fatalf("one selector: %v", err)
	}
	if name != "session" || value != "quiet-mesa" {
		t.Errorf("got (%q, %q), want (\"session\", \"quiet-mesa\")", name, value)
	}
}

func resetMailFlags() {
	mailFlagSession, mailFlagExternalID, mailFlagWorkspace, mailFlagJSON = "", "", "", false
}

// TestMatchSessions_SelectorsAndAmbiguity covers the resolution rules a hook
// depends on: --external-id is exact (the reason it exists), --workspace is a
// best-effort fallback that must report ambiguity rather than pick, and a
// session with no resolved folder has no mailbox to check.
func TestMatchSessions_SelectorsAndAmbiguity(t *testing.T) {
	ws := t.TempDir()
	all := []session.Info{
		{Name: "quiet-mesa", Folder: ws, ExternalID: "cc-session-1"},
		{Name: "swift-falcon", Folder: ws},
		{Name: "lone-heron", Folder: t.TempDir()},
		{Name: "pending", Folder: ""},
	}

	if got := matchSessions(all, "external-id", "cc-session-1"); len(got) != 1 || got[0].Name != "quiet-mesa" {
		t.Errorf("--external-id matched %v, want exactly quiet-mesa — the selector a hook relies on "+
			"must be exact even when sessions share a folder", mailNames(got))
	}
	if got := matchSessions(all, "workspace", ws); len(got) != 2 {
		t.Errorf("--workspace matched %d sessions, want 2 — the caller must be told it is ambiguous, "+
			"not handed a guess", len(got))
	}
	if got := matchSessions(all, "session", "pending"); len(got) != 0 {
		t.Errorf("a session with no resolved folder matched — there is no workspace to hold its mailbox")
	}
	if got := matchSessions(all, "workspace", filepath.Join(ws, ".")+string(filepath.Separator)); len(got) != 2 {
		t.Errorf("an untidy but equivalent --workspace path matched %d, want 2", len(got))
	}
}

// TestRunMail_ResolvesByExternalID is the end-to-end path the Stop hook takes:
// a client knows its own conversation id and nothing else, session_start
// persists it as external_id, and the probe resolves from that alone.
func TestRunMail_ResolvesByExternalID(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Cleanup(resetMailFlags)
	resetMailFlags()

	ws := t.TempDir()
	info, err := session.Register(session.Info{Name: "quiet-mesa", Folder: ws, Language: "go"})
	if err != nil {
		t.Fatalf("registering session: %v", err)
	}
	session.SetExternalID(info.ID, "cc-session-1")
	// Bound to the session that is about to ask — the case a claimant carrying
	// only the name would report as no mail at all.
	putBoundTestNote(t, ws, "swift-falcon", "quiet-mesa", info.ID, "ratelimit is yours")

	mailFlagExternalID = "cc-session-1"
	got, err := resolveMailSession()
	if err != nil {
		t.Fatalf("resolving by external id: %v", err)
	}
	if got.Name != "quiet-mesa" {
		t.Fatalf("resolved %q, want quiet-mesa", got.Name)
	}
	// Through runMail itself, so the claimant it builds is under test and not
	// just the function it hands one to.
	var report mailReport
	if err := json.Unmarshal(captureMailJSON(t), &report); err != nil {
		t.Fatalf("decoding the report: %v", err)
	}
	if report.Count != 1 {
		t.Fatalf("count = %d, want 1 — runMail must address the mailbox with the session's ID as "+
			"well as its name, or a message bound to this very session reads as none", report.Count)
	}
	if report.Session != "quiet-mesa" || report.Workspace != ws {
		t.Errorf("report identified %q in %q, want quiet-mesa in %q", report.Session, report.Workspace, ws)
	}
}

// captureMailJSON runs `plumb mail --json` and returns what it printed.
func captureMailJSON(t *testing.T) []byte {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	stdout := os.Stdout
	os.Stdout = w
	mailFlagJSON = true
	defer func() { os.Stdout = stdout }()

	runErr := runMail(nil, nil)
	w.Close()
	out, readErr := io.ReadAll(r)
	r.Close()
	if runErr != nil {
		t.Fatalf("runMail: %v", runErr)
	}
	if readErr != nil {
		t.Fatalf("reading captured stdout: %v", readErr)
	}
	return out
}

// TestResolveMailSession_UnknownIsAnError: a selector matching nothing is a
// usage error, not "no mail". Reporting zero would tell a hook that a session it
// failed to find is quietly up to date.
func TestResolveMailSession_UnknownIsAnError(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Cleanup(resetMailFlags)
	resetMailFlags()

	mailFlagSession = "no-such-session"
	_, err := resolveMailSession()
	if err == nil {
		t.Fatal("an unknown session resolved successfully")
	}
	if !strings.Contains(err.Error(), "no live session") {
		t.Errorf("error %q does not say the session was not found", err)
	}
}

// TestMailSentence_SaysWhichSession: resolving by --external-id or --workspace
// means the caller may not know the session name, so the human form has to name
// what it actually answered about.
func TestMailSentence_SaysWhichSession(t *testing.T) {
	empty := mailSentence(mailReport{Session: "quiet-mesa"})
	if !strings.Contains(empty, "quiet-mesa") || !strings.Contains(empty, "No messages") {
		t.Errorf("empty report renders as %q", empty)
	}
	full := mailSentence(mailReport{Session: "quiet-mesa", Count: 2, AgesSeconds: []int{300, 30}})
	for _, want := range []string{"2 messages", "quiet-mesa", "oldest 5m", "newest 30s", "check_messages"} {
		if !strings.Contains(full, want) {
			t.Errorf("report %q does not mention %q", full, want)
		}
	}
	if strings.Contains(full, "swift-falcon") {
		t.Error("the sentence leaked a sender name")
	}
}
