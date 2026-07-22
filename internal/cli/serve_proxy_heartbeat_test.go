package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// TestHeartbeatID_NonceDiffersAcrossProxies proves the hardening exists: two
// independently constructed proxies produce different ids for the same
// sequence number, while the id keeps its recognisable pingIDPrefix shape.
func TestHeartbeatID_NonceDiffersAcrossProxies(t *testing.T) {
	t.Parallel()
	p1 := newReconnectingProxy(proxyDeps{})
	p2 := newReconnectingProxy(proxyDeps{})

	id1, id2 := p1.heartbeatID(1), p2.heartbeatID(1)
	if id1 == id2 {
		t.Fatalf("two proxy instances produced identical heartbeat ids for the same sequence (%q); nonce is not effective", id1)
	}
	if !strings.HasPrefix(id1, pingIDPrefix) || !strings.HasPrefix(id2, pingIDPrefix) {
		t.Fatalf("heartbeat ids lost their recognisable prefix: %q, %q", id1, id2)
	}
}

// TestDeliverPong_OutstandingIDConsumed is requirement (a): a pong for a
// genuinely outstanding heartbeat id is still consumed exactly as before the
// nonce hardening — swallowed, and never forwarded to the client.
func TestDeliverPong_OutstandingIDConsumed(t *testing.T) {
	t.Parallel()
	p := newReconnectingProxy(proxyDeps{})
	var out bytes.Buffer
	p.deps.out = &out

	id := p.heartbeatID(42)
	key := idKey(json.RawMessage(fmt.Sprintf("%q", id)))
	ch := make(chan struct{}, 1)
	p.pongMu.Lock()
	p.pongCh[key] = ch
	p.pongMu.Unlock()

	frame := fmt.Appendf(nil, `{"jsonrpc":"2.0","id":%q,"result":{}}`, id)
	p.handleDaemonFrame(frame)

	select {
	case <-ch:
	default:
		t.Fatal("expected the outstanding heartbeat's pong channel to receive")
	}
	if out.Len() != 0 {
		t.Fatalf("a genuine pong must not be forwarded to the client, got %q", out.String())
	}
}

// TestDeliverPong_WrongNonceIDNotSwallowed is requirement (b), the core
// regression for this hardening: while a genuine heartbeat is outstanding, a
// message whose id merely has the right shape (pingIDPrefix + the same
// sequence number) but lacks this proxy's nonce — i.e. the pre-hardening
// bare convention id a client could plausibly reuse — must flow through to
// the client, not be swallowed as the pong.
func TestDeliverPong_WrongNonceIDNotSwallowed(t *testing.T) {
	t.Parallel()
	p := newReconnectingProxy(proxyDeps{})
	var out bytes.Buffer
	p.deps.out = &out

	genuineID := p.heartbeatID(7)
	genuineKey := idKey(json.RawMessage(fmt.Sprintf("%q", genuineID)))
	ch := make(chan struct{}, 1)
	p.pongMu.Lock()
	p.pongCh[genuineKey] = ch
	p.pongMu.Unlock()

	spoofedID := fmt.Sprintf("%s%d", pingIDPrefix, 7) // old bare "__plumb_hb_7", no nonce
	if spoofedID == genuineID {
		t.Fatalf("test fixture collided with the real (nonced) heartbeat id: %q", genuineID)
	}

	frame := fmt.Appendf(nil, `{"jsonrpc":"2.0","id":%q,"result":{"content":[{"type":"text","text":"client answer"}]}}`, spoofedID)
	p.handleDaemonFrame(frame)

	if !strings.Contains(out.String(), spoofedID) {
		t.Fatalf("client response with a heartbeat-shaped-but-wrong-nonce id was swallowed instead of forwarded; got %q", out.String())
	}
	select {
	case <-ch:
		t.Fatal("the wrong-nonce id must not deliver the pong meant for the genuine outstanding heartbeat")
	default:
	}
}

// TestDeliverPong_NoHeartbeatOutstandingNotSwallowed is requirement (b)'s
// second case: a pingIDPrefix-shaped id with no matching outstanding
// heartbeat at all is forwarded normally — baseline coverage for
// deliverPong's outstanding-map gate.
func TestDeliverPong_NoHeartbeatOutstandingNotSwallowed(t *testing.T) {
	t.Parallel()
	p := newReconnectingProxy(proxyDeps{})
	var out bytes.Buffer
	p.deps.out = &out

	id := "__plumb_hb_999"
	frame := fmt.Appendf(nil, `{"jsonrpc":"2.0","id":%q,"result":{}}`, id)
	p.handleDaemonFrame(frame)

	if !strings.Contains(out.String(), id) {
		t.Fatalf("response was swallowed though no heartbeat was outstanding; got %q", out.String())
	}
}
