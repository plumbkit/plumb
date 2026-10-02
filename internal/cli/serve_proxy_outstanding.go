package cli

import (
	"encoding/json"
	"fmt"
)

// serve_proxy_outstanding.go — the resilient proxy's ledger of in-flight requests:
// what has been confirmed sent to the daemon and not yet answered, and the
// synthesised retryable error a reconnect owes the client for each one the dead
// daemon will never answer. Split out of serve_proxy.go, unchanged, to keep that
// file under the size rule.

// outstandingReq is one confirmed-sent, unanswered request: its wire id plus
// the connection generation it was written under. The generation is what lets
// a reconnect sweep distinguish requests sent to a dead daemon (gen < current)
// from requests already re-issued on the fresh connection.
type outstandingReq struct {
	id  json.RawMessage
	gen uint64
}

// trackOutstanding records a request id as in-flight — but only AFTER the frame
// was successfully written to the daemon. Tracking before the write would let a
// reconnect's sweep synthesise a -32000 for a request the pump then re-sends to
// the fresh daemon: a double response, and an auto-replay of a write the "never
// auto-replay" contract forbids. By tracking only confirmed-sent requests, a
// request whose write failed is simply re-sent once (it never reached a
// daemon), while a confirmed-sent request that the daemon dies before answering
// gets exactly one synthesised retryable error. The initialize request is
// excluded — it is resolved by replayHandshake, not the sweep.
//
// Track-after-write leaves one race: the daemon can die — and the reconnect
// sweep run — in the gap between the successful write and the store below,
// which would orphan the request forever (the client hangs until its own
// timeout; reproduced as the proxy-test family's long-standing load flake).
// The post-store generation check closes it: if the connection generation
// advanced past writeGen while we were storing, the entry was written to a
// dead daemon and a sweep may already have missed it — sweep again now.
// Whichever of the two sweeps deletes the entry synthesises the error, so the
// client gets exactly one response either way.
func (p *reconnectingProxy) trackOutstanding(frame []byte, writeGen uint64) {
	e := parseEnvelope(frame)
	if !e.isRequest() {
		return
	}
	key := idKey(e.ID)
	p.hsMu.Lock()
	isInit := key == p.initializeID
	p.hsMu.Unlock()
	if isInit {
		return
	}
	p.reqMu.Lock()
	p.outstanding[key] = outstandingReq{id: cloneBytes(e.ID), gen: writeGen}
	p.reqMu.Unlock()
	if gen := p.generation(); gen != writeGen {
		p.failOutstandingBelow(gen)
	}
}

// failOutstandingBelow synthesises a retryable JSON-RPC error for every
// in-flight request written under a connection generation older than gen, so
// the client is never left waiting for a response a dead daemon will never
// send. Requests written on the current connection (gen == current) are left
// alone. The initialize request is excluded — it is resolved by
// replayHandshake.
func (p *reconnectingProxy) failOutstandingBelow(gen uint64) {
	p.reqMu.Lock()
	ids := make([]json.RawMessage, 0, len(p.outstanding))
	for k, req := range p.outstanding {
		if req.gen >= gen {
			continue
		}
		ids = append(ids, req.id)
		delete(p.outstanding, k)
	}
	p.reqMu.Unlock()
	p.dropPendingStarts(ids)

	for _, raw := range ids {
		resp := fmt.Sprintf(
			`{"jsonrpc":"2.0","id":%s,"error":{"code":-32000,"message":"plumb daemon restarted mid-request; this request's outcome is unconfirmed — for a write, re-read the file to check whether it landed before retrying"}}`,
			raw)
		p.writeClient([]byte(resp))
	}
}

// dropPendingStarts forgets the in-flight session_start bookkeeping for requests
// that have just been failed, so a call whose response will never arrive does
// not leave an entry behind forever.
//
// It matters more now than it used to: `pending` records EVERY session_start (a
// no-workspace call still carries the connection's identity), so the leak it
// closes is per-orientation-call rather than per-re-pin. The entry is small, but
// a long-lived proxy across many reconnects is exactly the shape that
// accumulates them.
func (p *reconnectingProxy) dropPendingStarts(ids []json.RawMessage) {
	if len(ids) == 0 {
		return
	}
	p.rc.dropInflight(ids)
	p.pinMu.Lock()
	defer p.pinMu.Unlock()
	for _, raw := range ids {
		delete(p.pending, idKey(raw))
	}
}

// failAllOutstanding synthesises the retryable error for EVERY in-flight
// request, whatever its connection generation. Used when the fast reconnect
// phase is exhausted and the proxy drops to slow background retry: the client
// must not stay blocked on an in-flight call for the whole outage.
func (p *reconnectingProxy) failAllOutstanding() {
	p.failOutstandingBelow(p.generation() + 1)
}
