package cli

// hooks_claude_clear_test.go — the SessionStart hook's /clear announcement (#564).

import (
	"bufio"
	"bytes"
	"net"
	"strings"
	"testing"
	"time"
)

// Only a conversation begun by /clear is announced; every other start reason is
// not, and the hook's one line of stdout is the linkage sentence either way.
func TestSessionStartHook_OnlyClearAnnouncesTheConversation(t *testing.T) {
	for _, tc := range []struct {
		source string
		want   bool
	}{
		{"clear", true},
		{"startup", false},
		{"resume", false},
		{"compact", false},
		{"", false},
	} {
		t.Run("source="+tc.source, func(t *testing.T) {
			var sent []string
			var out bytes.Buffer
			runClaudeSessionStart(claudeHookInput{SessionID: " conv-NEW ", Source: tc.source},
				func(id string) { sent = append(sent, id) }, &out)

			if tc.want && len(sent) != 1 || !tc.want && len(sent) != 0 {
				t.Fatalf("source %q announced %v, want announced=%v", tc.source, sent, tc.want)
			}
			if tc.want && sent[0] != "conv-NEW" {
				t.Errorf("announced %q, want the trimmed conversation id", sent[0])
			}
			if want := sessionLinkageSentence("conv-NEW", "conversation") + "\n"; out.String() != want {
				t.Errorf("stdout = %q, want exactly the linkage sentence %q", out.String(), want)
			}
		})
	}
}

// A hook input with no conversation id announces nothing and prints nothing.
func TestSessionStartHook_NoConversationIDIsSilent(t *testing.T) {
	var out bytes.Buffer
	runClaudeSessionStart(claudeHookInput{Source: "clear"}, func(string) { t.Error("announced an empty id") }, &out)
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want nothing", out.String())
	}
}

// The announcement reaches the daemon's real control handler and becomes a marker
// that its conversation's first call consumes exactly once.
func TestSessionStartHook_ClearReachesTheRealControlHandler(t *testing.T) {
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

	var out bytes.Buffer
	runClaudeSessionStart(claudeHookInput{SessionID: "conv-NEW", Source: "clear"}, notifyConversationCleared, &out)

	if !markers.take("conv-NEW") {
		t.Fatal("the daemon holds no marker for the conversation the hook announced")
	}
	if markers.take("conv-NEW") {
		t.Error("the marker was usable twice")
	}
	if want := sessionLinkageSentence("conv-NEW", "conversation") + "\n"; out.String() != want {
		t.Errorf("stdout = %q, want exactly the linkage sentence", out.String())
	}
}

// A daemon that does not know the command (any release before this one) is not an
// error the hook shows.
func TestSessionStartHook_OlderDaemonIsSilent(t *testing.T) {
	probeTestEnv(t)
	fakeCtrlDaemon(t, preKeysDaemon("0.21.0"))
	var out bytes.Buffer
	runClaudeSessionStart(claudeHookInput{SessionID: "conv-NEW", Source: "clear"}, notifyConversationCleared, &out)
	if want := sessionLinkageSentence("conv-NEW", "conversation") + "\n"; out.String() != want {
		t.Errorf("stdout = %q, want exactly the linkage sentence", out.String())
	}
}

// No daemon, or one that accepts and never answers: the hook finishes inside its
// budget and prints only the linkage sentence.
func TestSessionStartHook_UnreachableOrWedgedDaemonIsSilentAndBounded(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T){
		"no daemon": func(t *testing.T) { t.Helper() },
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
			var out bytes.Buffer
			start := time.Now()
			runClaudeSessionStart(claudeHookInput{SessionID: "conv-NEW", Source: "clear"}, notifyConversationCleared, &out)

			if took := time.Since(start); took > clearNotifyBudget+500*time.Millisecond {
				t.Errorf("the hook took %v, past its %v budget", took, clearNotifyBudget)
			}
			if want := sessionLinkageSentence("conv-NEW", "conversation") + "\n"; out.String() != want {
				t.Errorf("stdout = %q, want exactly the linkage sentence", out.String())
			}
		})
	}
}

// An id that spans lines is dropped before it is sent: the protocol is one command
// per line.
func TestNotifyConversationCleared_DropsAMultiLineID(t *testing.T) {
	probeTestEnv(t)
	got := make(chan string, 2)
	fakeCtrlDaemon(t, func(c net.Conn, line string) { got <- line; _, _ = c.Write([]byte("ok\n")) })
	notifyConversationCleared("conv-1\nreload-config")
	select {
	case line := <-got:
		t.Errorf("a multi-line id reached the daemon as %q", line)
	case <-time.After(100 * time.Millisecond):
	}
}

// The command through the real handler with no marker table is an error line, not
// a hang or a panic.
func TestCtrlConversationCleared_WithoutATableIsAnError(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()
	go handleCtrlConn(server, "info", "text", ctrlHandlers{})
	if _, err := client.Write([]byte(ctrlConversationClearedCommand + " conv-1\n")); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(client).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "error:") {
		t.Fatalf("reply %q, err %v; want an error line", line, err)
	}
}
