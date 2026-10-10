package cli

// hooks_codex_clear_test.go — Codex's SessionStart hook announcing a /clear (PLAN-465),
// the Codex half of what hooks_claude_clear_test.go pins for Claude Code.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net"
	"testing"
	"time"
)

// codexSessionStartJSON is the hook's stdout for one SessionStart input, exactly as
// Codex reads it.
func codexSessionStartJSON(t *testing.T, input codexHookInput, notify func(string)) string {
	t.Helper()
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runCodexHookIOWith(bytes.NewReader(body), &out, nil, notify); err != nil {
		t.Fatalf("hook returned %v, want nil (hooks fail open)", err)
	}
	return out.String()
}

// The linkage document as it was before Source existed: what every start reason,
// clear included, must still print byte for byte.
func codexLinkageJSON(t *testing.T, sessionID string) string {
	t.Helper()
	var out bytes.Buffer
	if err := json.NewEncoder(&out).Encode(map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName":     "SessionStart",
		"additionalContext": sessionLinkageSentence(sessionID, "Codex conversation"),
	}}); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// Only a conversation begun by /clear is announced. Codex 0.161.0's SessionStart
// schema lists startup, resume, clear, compact and fork; fork starts a new
// conversation too, but from a copy of one that still exists, so it must not take
// the old conversation's identity.
func TestCodexSessionStartHook_OnlyClearAnnouncesTheConversation(t *testing.T) {
	for _, tc := range []struct {
		source string
		want   bool
	}{
		{"clear", true},
		{"startup", false},
		{"resume", false},
		{"compact", false},
		{"fork", false},
		{"Clear", false},
		{"", false},
	} {
		t.Run("source="+tc.source, func(t *testing.T) {
			var sent []string
			got := codexSessionStartJSON(t,
				codexHookInput{Event: "SessionStart", SessionID: " thr-NEW ", Source: tc.source},
				func(id string) { sent = append(sent, id) })

			if tc.want && len(sent) != 1 || !tc.want && len(sent) != 0 {
				t.Fatalf("source %q announced %v, want announced=%v", tc.source, sent, tc.want)
			}
			if tc.want && sent[0] != "thr-NEW" {
				t.Errorf("announced %q, want the trimmed conversation id", sent[0])
			}
			if want := codexLinkageJSON(t, " thr-NEW "); got != want {
				t.Errorf("stdout = %q, want the unchanged linkage document %q", got, want)
			}
		})
	}
}

// A SessionStart with no conversation id announces nothing and prints nothing.
func TestCodexSessionStartHook_NoConversationIDIsSilent(t *testing.T) {
	got := codexSessionStartJSON(t, codexHookInput{Event: "SessionStart", SessionID: "  ", Source: "clear"},
		func(string) { t.Error("announced an empty id") })
	if got != "" {
		t.Errorf("stdout = %q, want nothing", got)
	}
}

// Stop never announces, whatever source a client might send with it.
func TestCodexStopHook_NeverAnnounces(t *testing.T) {
	out := codexHookResult(codexHookInput{Event: "Stop", SessionID: "thr-1", Source: "clear"},
		func(string, string) (mailReport, bool) { return mailReport{}, false },
		func(string) { t.Error("Stop announced a conversation") })
	if out != nil {
		t.Errorf("Stop with no mail = %v, want nil", out)
	}
}

// The `source` field is decoded from the real wire shape: Codex sends snake_case
// keys, and a payload carrying fields plumb does not read still decodes.
func TestCodexHookInput_DecodesSourceFromTheWire(t *testing.T) {
	var sent []string
	payload := `{"session_id":"thr-NEW","cwd":"/repo","hook_event_name":"SessionStart","model":"gpt-x",` +
		`"permission_mode":"default","source":"clear","transcript_path":null}`
	var out bytes.Buffer
	if err := runCodexHookIOWith(bytes.NewReader([]byte(payload)), &out, nil,
		func(id string) { sent = append(sent, id) }); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 1 || sent[0] != "thr-NEW" {
		t.Fatalf("announced %v, want [thr-NEW]", sent)
	}
	if want := codexLinkageJSON(t, "thr-NEW"); out.String() != want {
		t.Errorf("stdout = %q, want %q", out.String(), want)
	}
}

// clearMarkingDaemon stands up a fake control socket that answers through the
// daemon's real handler, and returns the marker table it fills.
func clearMarkingDaemon(t *testing.T) *clearMarkers {
	t.Helper()
	probeTestEnv(t)
	markers := newClearMarkers()
	fakeCtrlDaemon(t, func(c net.Conn, line string) {
		p1, p2 := net.Pipe()
		go handleCtrlConn(p2, "info", "text", ctrlHandlers{conversationCleared: markers.mark})
		_, _ = p1.Write([]byte(line + "\n"))
		r, _ := bufio.NewReader(p1).ReadString('\n')
		_ = p1.Close()
		_, _ = c.Write([]byte(r))
	})
	return markers
}

// The production entry point, runCodexHookIO, is wired to the real notifier: a
// clear payload on stdin leaves a marker in the daemon.
func TestRunCodexHookIO_ClearReachesTheDaemon(t *testing.T) {
	markers := clearMarkingDaemon(t)
	payload := `{"session_id":"thr-NEW","cwd":"/repo","hook_event_name":"SessionStart","source":"clear"}`
	var out bytes.Buffer
	if err := runCodexHookIO(bytes.NewReader([]byte(payload)), &out); err != nil {
		t.Fatal(err)
	}
	if !markers.take("thr-NEW") {
		t.Fatal("runCodexHookIO announced nothing to the daemon for source=clear")
	}
	if want := codexLinkageJSON(t, "thr-NEW"); out.String() != want {
		t.Errorf("stdout = %q, want %q", out.String(), want)
	}
}

// The announcement reaches the daemon's real control handler and becomes a marker
// that its conversation's first call consumes exactly once.
func TestCodexSessionStartHook_ClearReachesTheRealControlHandler(t *testing.T) {
	markers := clearMarkingDaemon(t)

	got := codexSessionStartJSON(t,
		codexHookInput{Event: "SessionStart", SessionID: "thr-NEW", Source: "clear"}, notifyConversationCleared)

	if !markers.take("thr-NEW") {
		t.Fatal("the daemon holds no marker for the conversation the hook announced")
	}
	if markers.take("thr-NEW") {
		t.Error("the marker was usable twice")
	}
	if want := codexLinkageJSON(t, "thr-NEW"); got != want {
		t.Errorf("stdout = %q, want the unchanged linkage document", got)
	}
}

// Codex's calls carry no identity stamp (it has no identity hook), so its new
// conversation's session_start is unstamped and relinks the connection by itself,
// keeping the name, as an unstamped session_start always has. The /clear marker
// its hook sends is not what does that today: handOverOnClear never sees an
// unstamped call, so the marker stays unconsumed and expires. It is announced
// anyway because the relink is not the whole handover — the new id still counts as
// a second conversation on the connection — and the marker is the only evidence
// that the old one ended (PLAN-465's follow-up card).
func TestConversationIdentity_CodexUnstampedClearRelinksWithoutTheMarker(t *testing.T) {
	w := newIdentityWorld(t)
	ws := identityRepo(t)
	c := w.conn("")
	c.start("", ws, convOld, nil)
	name := c.s.sessionName()

	w.registry.clears.mark(convNew) // what the Codex SessionStart hook announces on /clear
	c.start("", ws, convNew, nil)

	if got := c.s.externalID(); got != convNew {
		t.Fatalf("linkage = %q, want the unstamped session_start to relink it to %q", got, convNew)
	}
	if got := c.s.sessionName(); got != name {
		t.Errorf("the connection was renamed %q, want it to keep %q", got, name)
	}
	if !w.registry.clears.take(convNew) {
		t.Error("an unstamped call consumed the /clear marker; the handover path should never see it")
	}
}

// A daemon too old to know the command, no daemon, or one that accepts and never
// answers: the hook finishes inside its budget and prints only the linkage document.
func TestCodexSessionStartHook_UnhelpfulDaemonIsSilentAndBounded(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T){
		"older daemon": func(t *testing.T) { t.Helper(); fakeCtrlDaemon(t, preKeysDaemon("0.21.0")) },
		"no daemon":    func(t *testing.T) { t.Helper() },
		"wedged daemon": func(t *testing.T) {
			t.Helper()
			release := make(chan struct{})
			fakeCtrlDaemon(t, func(net.Conn, string) { <-release })
			t.Cleanup(func() { close(release) }) // runs before the listener's own cleanup
		},
	} {
		t.Run(name, func(t *testing.T) {
			probeTestEnv(t)
			setup(t)
			start := time.Now()
			got := codexSessionStartJSON(t,
				codexHookInput{Event: "SessionStart", SessionID: "thr-NEW", Source: "clear"}, notifyConversationCleared)

			if took := time.Since(start); took > clearNotifyBudget+500*time.Millisecond {
				t.Errorf("the hook took %v, past its %v budget", took, clearNotifyBudget)
			}
			if want := codexLinkageJSON(t, "thr-NEW"); got != want {
				t.Errorf("stdout = %q, want the unchanged linkage document", got)
			}
		})
	}
}
