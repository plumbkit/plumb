package cli

// conn_resume_harness_test.go — the rig for the resume-credential tests: frames
// that carry request `_meta` and return result `_meta`, the initialize-time hooks
// run in the order handleInitialize runs them, and a fixture that is the whole
// serve-replacement story up to the moment the replacement presents.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/plumbkit/plumb/internal/mcp"
)

// frameResult is one tools/call result: the text a model sees, and the `_meta` only
// the proxy reads.
type frameResult struct {
	Text    string
	IsError bool
	Meta    map[string]json.RawMessage
}

// credential is the resume credential the result's `_meta` discloses, or "".
func (r frameResult) credential() string {
	raw, ok := r.Meta[mcp.MetaResumeCredentialKey]
	if !ok {
		return ""
	}
	var v string
	_ = json.Unmarshal(raw, &v)
	return v
}

// callMeta is identityConn.call with request `_meta` and the full result back.
func (c *identityConn) callMeta(agent, tool string, args, meta map[string]any) frameResult {
	c.w.t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	if agent != "" {
		args[mcp.ArgLogicalAgentKey] = agent
	}
	params := map[string]any{"name": tool, "arguments": args}
	if meta != nil {
		params["_meta"] = meta
	}
	frame, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": c.w.ids.Add(1), "method": "tools/call", "params": params})
	if err != nil {
		c.w.t.Fatalf("marshal frame: %v", err)
	}
	var out bytes.Buffer
	if err := c.srv.Serve(context.Background(), strings.NewReader(string(frame)+"\n"), &out); err != nil {
		c.w.t.Fatalf("Serve: %v", err)
	}
	var resp struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool                       `json:"isError"`
			Meta    map[string]json.RawMessage `json:"_meta"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		c.w.t.Fatalf("decode %q: %v", out.String(), err)
	}
	if resp.Error != nil {
		return frameResult{Text: resp.Error.Message, IsError: true}
	}
	r := frameResult{IsError: resp.Result.IsError, Meta: resp.Result.Meta}
	if len(resp.Result.Content) > 0 {
		r.Text = resp.Result.Content[0].Text
	}
	return r
}

// startPresenting is the stamped session_start a replacement serve sends: it names
// the conversation and carries the stored credential in the request `_meta`.
func (c *identityConn) startPresenting(agent, workspace, conversation, secret string) frameResult {
	c.w.t.Helper()
	args := map[string]any{"session_id": conversation}
	if workspace != "" {
		args["workspace"] = workspace
	}
	var meta map[string]any
	if secret != "" {
		meta = map[string]any{mcp.MetaResumeCredentialKey: secret}
	}
	r := c.callMeta(agent, "session_start", args, meta)
	if r.IsError {
		c.w.t.Fatalf("session_start as %q: %s", agent, r.Text)
	}
	return r
}

// initialize runs what handleInitialize runs, in its order: the param hook that
// hands the connection its proxy credential, then the hook that builds the result
// `_meta`. It returns that `_meta`.
func (c *identityConn) initialize(proxy string) map[string]any {
	c.w.t.Helper()
	ctx := context.Background()
	if proxy != "" {
		c.srv.OnProxySession(ctx, proxy)
	}
	return c.srv.InitializeMeta(ctx)
}

// disclosed is the resume credential an initialize `_meta` carries, or "".
func disclosed(meta map[string]any) string {
	v, _ := meta[mcp.MetaResumeCredentialKey].(string)
	return v
}

var resumeSecretShape = regexp.MustCompile(`^rsk1-[A-Za-z0-9_-]{22}$`)

// leakShape finds a resume credential anywhere in a body of text.
var leakShape = regexp.MustCompile(`rsk1-[A-Za-z0-9_-]{22}`)

// credFixture is a conversation that ran on a serve, was given a credential, built up
// mail and a thread, and then lost its serve and connection (the reboot shape). The
// durable state survives; a replacement has not yet arrived.
type credFixture struct {
	w        *identityWorld
	ws       string
	peer     *identityConn
	cred     string // the credential the first serve was given
	id, name string // the identity that must come back
	threadID string
	proxy    string
}

const (
	credConv      = "conv-cred"
	credBoundNote = "second note, unread"
)

func newCredFixture(t *testing.T) *credFixture {
	t.Helper()
	w := newIdentityWorld(t).withState()
	f := &credFixture{w: w, ws: identityRepo(t), peer: w.conn(""), proxy: "P1"}
	f.peer.call("", "session_start", map[string]any{"workspace": f.ws})

	first := w.conn("")
	f.cred = disclosed(first.initialize(f.proxy))
	if !resumeSecretShape.MatchString(f.cred) {
		t.Fatalf("precondition: first contact under a proxy credential disclosed %q, want an rsk1- credential", f.cred)
	}
	first.start(credConv, f.ws, credConv, nil)
	f.id, f.name = first.s.sessionID(), first.s.sessionName()

	f.peer.call("", "leave_note", map[string]any{"to": f.name, "body": "hello"})
	out, _ := first.call(credConv, "check_messages", nil)
	f.threadID = conversationIDOf(t, out)
	if out, isErr := first.call(credConv, "leave_note", map[string]any{"conversation_id": f.threadID, "body": "reply from the original"}); isErr {
		t.Fatalf("precondition: the original cannot reply in its own thread: %s", out)
	}
	f.peer.call("", "leave_note", map[string]any{"to": f.name, "body": credBoundNote})
	first.s.close() // the serve, the daemon and the connection are gone
	return f
}

// replacement is a fresh serve's connection: new proxy credential, first contact.
func (f *credFixture) replacement(proxy string) *identityConn {
	f.w.t.Helper()
	c := f.w.conn("")
	if got := disclosed(c.initialize(proxy)); !resumeSecretShape.MatchString(got) {
		f.w.t.Fatalf("precondition: the replacement's first contact disclosed %q", got)
	}
	return c
}

// logCapture routes the process logger into a buffer for the test, BEFORE any
// connection is built (a connection binds its logger at construction), and returns
// the buffer's contents on demand.
type logCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logCapture) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logCapture) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func captureLogs(t *testing.T) *logCapture {
	t.Helper()
	lc := &logCapture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(lc, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return lc
}

// assertNoSecret fails when a credential-shaped token, or any of the given secrets,
// appears in body. It is the positive-controlled leak scan: leakScanWorks proves it
// fires, so an absence it reports means something.
func assertNoSecret(t *testing.T, label, body string, secrets ...string) {
	t.Helper()
	if m := leakShape.FindString(body); m != "" {
		t.Errorf("%s contains a resume-credential-shaped token (%s…)", label, m[:9])
	}
	for _, s := range secrets {
		if s != "" && strings.Contains(body, s) {
			t.Errorf("%s contains a disclosed secret", label)
		}
	}
}

func leakScanWorks() bool {
	return leakShape.MatchString(fmt.Sprintf("noise %s noise", "rsk1-AAAAAAAAAAAAAAAAAAAAAA"))
}
