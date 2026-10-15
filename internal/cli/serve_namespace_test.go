package cli

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/mcp"
)

const nsRefusalFrame = `{"jsonrpc":"2.0","id":1,"error":{"code":-32010,"message":"namespace ns-1 is revoked"}}`

func TestNamespaceRefusal_RecognisesOnlyItsCode(t *testing.T) {
	if msg, ok := namespaceRefusal([]byte(nsRefusalFrame)); !ok || msg != "namespace ns-1 is revoked" {
		t.Fatalf("refusal = %q, %v", msg, ok)
	}
	for _, f := range []string{
		`{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"other"}}`,
		`{"jsonrpc":"2.0","id":1,"result":{}}`,
		`not json`,
	} {
		if _, ok := namespaceRefusal([]byte(f)); ok {
			t.Fatalf("%s read as a namespace refusal", f)
		}
	}
}

// TestCaptureHandshake_CarriesTheNamespaceToken: the token rides the initialize
// the proxy forwards (and so every replay, which resends the captured frame).
func TestCaptureHandshake_CarriesTheNamespaceToken(t *testing.T) {
	p := newReconnectingProxy(proxyDeps{namespaceToken: "pns1-abc"})
	frame := p.captureHandshake([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	if !strings.Contains(string(frame), `"`+mcp.MetaNamespaceTokenKey+`":"pns1-abc"`) {
		t.Fatalf("captured initialize lacks the token: %s", frame)
	}
	plain := newReconnectingProxy(proxyDeps{})
	if f := plain.captureHandshake([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)); strings.Contains(string(f), mcp.MetaNamespaceTokenKey) {
		t.Fatalf("a proxy without a namespace sent a token key: %s", f)
	}
}

// refusingDaemon answers every initialize on conn with the namespace refusal.
func refusingDaemon(conn net.Conn) {
	fr := newFrameReader(conn)
	if _, err := fr.read(); err != nil {
		return
	}
	_ = writeFrame(conn, []byte(nsRefusalFrame))
}

// TestReconnect_NamespaceRefusalEndsTheProxy: a replay the daemon refuses for
// the namespace ends the proxy with that error after ONE attempt — a revoked
// namespace does not come back, and retrying in the background would keep a
// client attached to nothing (or, worse, invite a fallback).
func TestReconnect_NamespaceRefusalEndsTheProxy(t *testing.T) {
	outR, outW := io.Pipe()
	go func() { _, _ = io.Copy(io.Discard, outR) }()
	t.Cleanup(func() { _ = outW.Close() })
	var dials atomic.Int32
	p := newReconnectingProxy(proxyDeps{
		out:           outW,
		handshakeWait: 5 * time.Second,
		baseBackoff:   time.Millisecond,
		dial: func(context.Context) (net.Conn, error) {
			dials.Add(1)
			daemon, proxy := net.Pipe()
			go refusingDaemon(daemon)
			return proxy, nil
		},
	})
	p.initializeFrame = []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	p.initializeID = idKey([]byte(`1`))
	p.initializeAnswered = true

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := p.reconnect(ctx, p.generation(), false, "test")
	if !errors.Is(err, errNamespaceRefused) {
		t.Fatalf("reconnect = %v, want errNamespaceRefused", err)
	}
	if n := dials.Load(); n != 1 {
		t.Fatalf("dialled %d times, want exactly 1 (no retry after a refusal)", n)
	}
}

func TestServeNamespaceToken_FlagThenEnv(t *testing.T) {
	env := func(string) string { return "pns1-env" }
	if got := serveNamespaceToken("pns1-flag", env); got != "pns1-flag" {
		t.Fatalf("flag+env = %q, want the flag", got)
	}
	if got := serveNamespaceToken("", env); got != "pns1-env" {
		t.Fatalf("env only = %q, want PLUMB_NAMESPACE", got)
	}
	if got := serveNamespaceToken("", func(string) string { return "" }); got != "" {
		t.Fatalf("neither = %q", got)
	}
}

func TestLegacyPumpNamespaceGuard(t *testing.T) {
	if err := legacyPumpNamespaceGuard("pns1-x"); err == nil {
		t.Fatal("the legacy byte pump accepted a namespace it cannot carry")
	}
	if err := legacyPumpNamespaceGuard(""); err != nil {
		t.Fatalf("a plain legacy serve was refused: %v", err)
	}
}
