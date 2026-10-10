package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/plumbkit/plumb/internal/tools"
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
	// tool is the tools/call name this request carried, or "" for anything else. It
	// exists so the synthesised error can say something specific when the killed
	// request was a mutation_test (PLAN-459): the journal knows which files may still
	// hold a mutant, and "re-read the file" is useless advice without a path.
	tool string
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
	p.outstanding[key] = outstandingReq{id: cloneBytes(e.ID), gen: writeGen, tool: calledTool(e)}
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
	failed := make([]outstandingReq, 0, len(p.outstanding))
	for k, req := range p.outstanding {
		if req.gen >= gen {
			continue
		}
		failed = append(failed, req)
		delete(p.outstanding, k)
	}
	p.reqMu.Unlock()
	ids := make([]json.RawMessage, 0, len(failed))
	for _, req := range failed {
		ids = append(ids, req.id)
	}
	p.dropPendingStarts(ids)

	for _, req := range failed {
		message, err := json.Marshal(proxyRestartMessage(req.tool, p.pinned))
		if err != nil {
			// A message that cannot be encoded must still leave the client with an
			// answer: the point of this whole sweep is that nothing hangs.
			message = []byte(`"plumb daemon restarted mid-request; this request's outcome is unconfirmed"`)
		}
		resp := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"error":{"code":-32000,"message":%s}}`, req.id, message)
		p.writeClient([]byte(resp))
	}
}

// calledTool names the tool a tools/call request invokes, or "" when the frame is
// some other method. Only the name is read: the proxy never inspects a tool's
// arguments, and adding this field is the whole of its knowledge about which tool
// a request was for.
func calledTool(e rpcEnvelope) string {
	if e.Method != "tools/call" || len(e.Params) == 0 {
		return ""
	}
	var params struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(e.Params, &params) != nil {
		return ""
	}
	return params.Name
}

// proxyRestartMessage is the retryable error a reconnect owes an interrupted
// request. For a mutation_test it names what the mutant journal still holds, because
// that tool can leave a source file holding a mutant when the daemon is killed, and
// "re-read the file" without a path is exactly the advice PLAN-459 was filed about.
//
// Each entry's own state is worded for what is true of it: a file still holding the
// mutant needs a re-read, one already back does not, and only a file matching neither
// side is one plumb deliberately left alone. Saying "matches neither side" for a file
// that simply still holds the mutant is the lie this classifier exists to prevent.
func proxyRestartMessage(tool, workspace string) string {
	const base = "plumb daemon restarted mid-request; this request's outcome is unconfirmed — for a write, re-read the file to check whether it landed before retrying"
	if tool != "mutation_test" {
		return base
	}
	states, err := tools.MutantJournalStates()
	if err != nil {
		return base + ". This was a mutation_test; plumb could not read its mutant journal to say which file was involved"
	}
	if len(states) == 0 {
		return base + ". This was a mutation_test: nothing is left journalled, so any mutant it had applied has been put back"
	}
	// Keep only THIS workspace's mutants: a note about this request that names another project's
	// files would be worse than saying less (PLAN-459 SHOULD-FIX 3). Containment, not provenance —
	// an entry records an absolute target path, and a path under this workspace is this
	// workspace's business. An EMPTY workspace filters nothing, the contract for a caller that
	// cannot resolve one: better to describe every entry than to describe none.
	if workspace != "" {
		// The separator is taken from the workspace's own spelling rather than from filepath,
		// which this file does not import: a workspace is written the way its platform writes
		// paths, so its last separator is the right one for the comparison.
		sep := "/"
		if i := strings.LastIndexAny(workspace, `\/`); i >= 0 {
			sep = string(workspace[i])
		}
		root := strings.TrimSuffix(workspace, sep) + sep
		kept := states[:0]
		for _, st := range states {
			if strings.HasPrefix(st.Path, root) {
				kept = append(kept, st)
			}
		}
		states = kept
		if len(states) == 0 {
			// Narrower than the case above: nothing is journalled FOR THIS WORKSPACE, so this
			// must not claim a mutant was put back — only that none is on record here.
			return base + ". This was a mutation_test: nothing is left journalled for this workspace"
		}
	}
	// One sentence per state, so a group is never described as something it is not.
	var msg strings.Builder
	msg.WriteString(base)
	msg.WriteByte('.')
	for _, group := range groupMutantJournalStates(states) {
		msg.WriteString(mutantJournalClause(group.state, group.paths))
	}
	return msg.String()
}

// mutantJournalGroup is one state's share of the journal: every path in it is in the
// same state, so one sentence can be true of all of them.
type mutantJournalGroup struct {
	state tools.MutantJournalState
	paths []string
}

// groupMutantJournalStates gathers paths by state, keeping the order they arrived in
// (the journal is read in sorted file-name order), so the message is deterministic
// rather than map-ordered. A state with no paths contributes no sentence.
func groupMutantJournalStates(states []tools.MutantJournalStatus) []mutantJournalGroup {
	var groups []mutantJournalGroup
	at := make(map[tools.MutantJournalState]int, 4)
	for _, s := range states {
		i, ok := at[s.State]
		if !ok {
			at[s.State] = len(groups)
			groups = append(groups, mutantJournalGroup{state: s.State, paths: []string{s.Path}})
			continue
		}
		groups[i].paths = append(groups[i].paths, s.Path)
	}
	return groups
}

// mutantJournalClause is one state's sentence, with the plural wording kept for a
// group holding more than one path.
func mutantJournalClause(state tools.MutantJournalState, paths []string) string {
	joined := strings.Join(paths, ", ")
	one := len(paths) == 1
	switch state {
	case tools.MutantStillApplied:
		if one {
			return " This was a mutation_test, and a mutant is still applied at " + joined + " — re-read it before retrying"
		}
		return " This was a mutation_test, and mutants are still applied at " + joined + " — re-read them before retrying"
	case tools.MutantAlreadyBack:
		if one {
			return " This was a mutation_test; the file at " + joined + " is already back to its pre-run content"
		}
		return " This was a mutation_test; the files at " + joined + " are already back to their pre-run content"
	case tools.MutantFileChanged:
		if one {
			return " This was a mutation_test; the file at " + joined +
				" matches neither side, so plumb left it exactly as it was — re-read it before retrying"
		}
		return " This was a mutation_test; the files at " + joined +
			" match neither side, so plumb left them exactly as they were — re-read them before retrying"
	case tools.MutantFileGone:
		if one {
			return " This was a mutation_test; the file at " + joined + " no longer exists"
		}
		return " This was a mutation_test; the files at " + joined + " no longer exist"
	default:
		// A state this switch does not know must not inherit another state's claim: the
		// old `default: // MutantFileGone` gave a future fifth state the sentence "no
		// longer exists", which is a false statement about a file that is sitting
		// there. Say only what is certainly true — a mutant is journalled for the path.
		if one {
			return " This was a mutation_test; a mutant is journalled for " + joined
		}
		return " This was a mutation_test; mutants are journalled for " + joined
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
