package cli

// serve_resume.go — the proxy's half of the resume credential
// (docs/identity-resume-credential-design.md §3 and §5): announce that it strips the
// key, learn the credential the daemon discloses, keep it per conversation, and present
// it when a replacement serve names the conversation.
//
// The pieces, and where each one runs:
//
//   - withResumeConsumer folds the announcement into the initialize `_meta`. The daemon
//     discloses to nothing else, so a proxy that does not announce is simply never
//     issued a credential (and a daemon that predates the feature ignores the key).
//   - stripResumeCredential (serve_resume_strip.go) removes the key from every frame on
//     its way to the client. Not here: it is the last step of writeClient.
//   - observeResumeFrame reads every daemon frame BEFORE it is stripped, takes the
//     credential out of it, and, when the frame answers a session_start, learns which
//     conversation this process's identity is linked to.
//   - presentResumeCredential rewrites a session_start request that names a conversation
//     with a stored entry, attaching the credential to its request `_meta`.
//
// What is presented, and when. Only a REPLACEMENT serve presents: a serve whose daemon
// restarted keeps its proxy credential and is restored by it, and presenting a resume
// credential there would only rotate it pointlessly. The daemon decides the same way (it
// accepts a presentation only on a connection that is a fresh `established` identity), so
// the proxy presents only while the daemon's last word on this connection is
// `established`, only on the first session_start that names a conversation with a stored
// entry, only once per conversation per process, and never on a call stamped as another
// agent. The proxy is the sole presenter: a credential the client put in the request
// `_meta` itself is removed from every session_start.
//
// What is stored, and when. The first successful session_start that names a conversation
// links this process's identity to it, and the credential the daemon has disclosed to this
// process is then recorded under it, and re-recorded at every later disclosure (the
// successor after an accepted resume, a re-mint after a daemon restart, a late C3
// disclosure). The exception is a process that PRESENTED a stored credential and was not
// given a successor: it was refused, or could not complete (a live session still holds the
// predecessor's ID), and the daemon does not say which. The stored entry is then left
// alone for the life of the process, because overwriting it with this process's fresh
// credential would trade a retry that can still restore the predecessor's mail and threads
// for one that restores a name-only fork.

import (
	"encoding/json"
	"log/slog"
	"sync"

	"github.com/plumbkit/plumb/internal/mcp"
)

// resumeCreds is a proxy's resume-credential state. The zero value is a proxy with no
// store: it strips and holds in memory but persists and presents nothing.
type resumeCreds struct {
	store *resumeStore

	mu sync.Mutex
	// secret is the credential the daemon last disclosed to THIS process.
	secret string
	// conversation is the conversation the first successful session_start linked.
	conversation string
	// owns reports that this process may write conversation's stored entry.
	owns bool
	// presented is the conversations this process has already presented for.
	presented map[string]bool
	// inflight is the session_start requests awaiting a response, by id.
	inflight map[string]resumeStart
}

// resumeStart is what an in-flight session_start asked for.
type resumeStart struct {
	conversation string
	presented    bool
}

// withResumeConsumer adds the consumer announcement to the initialize `_meta`. It
// travels inside the captured initialize frame, so every handshake replay re-announces.
func withResumeConsumer(meta map[string]json.RawMessage) map[string]json.RawMessage {
	if meta == nil {
		meta = map[string]json.RawMessage{}
	}
	meta[mcp.MetaResumeCredentialConsumerKey] = json.RawMessage("1")
	return meta
}

// sessionStartCall is the part of a session_start request the proxy reads.
type sessionStartCall struct {
	id           string
	conversation string
	stamp        string
	clientMeta   bool // the client put a credential in the request _meta itself
}

// parseSessionStartCall reads a tools/call frame. ok is false for anything that is not a
// session_start request.
func parseSessionStartCall(frame []byte) (sessionStartCall, bool) {
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Name      string                     `json:"name"`
			Arguments map[string]json.RawMessage `json:"arguments"`
			Meta      map[string]json.RawMessage `json:"_meta"`
		} `json:"params"`
	}
	if err := json.Unmarshal(frame, &req); err != nil || req.Method != "tools/call" || req.Params.Name != sessionStartTool {
		return sessionStartCall{}, false
	}
	call := sessionStartCall{
		id:           idKey(req.ID),
		conversation: linkageIDOf(rawString(req.Params.Arguments, "session_id")),
		stamp:        rawString(req.Params.Meta, mcp.MetaLogicalAgentKey),
	}
	if call.stamp == "" {
		call.stamp = rawString(req.Params.Arguments, mcp.ArgLogicalAgentKey)
	}
	if call.stamp == "" {
		call.stamp = rawString(req.Params.Arguments, mcp.ArgLogicalAgentDeclaredKey)
	}
	_, call.clientMeta = req.Params.Meta[mcp.MetaResumeCredentialKey]
	return call, true
}

func rawString(m map[string]json.RawMessage, key string) string {
	var s string
	if raw, ok := m[key]; ok {
		_ = json.Unmarshal(raw, &s)
	}
	return s
}

// presentResumeCredential returns the frame to forward for a client request: a
// session_start has its request `_meta` set to the credential stored for the conversation
// it names, when this process is entitled to present one, and cleared of any credential
// the client supplied otherwise. Every other frame is returned unchanged.
func (p *reconnectingProxy) presentResumeCredential(frame []byte) []byte {
	call, ok := parseSessionStartCall(frame)
	if !ok {
		return frame
	}
	secret := p.rc.claim(call, p.heldIdentity().recovery)
	switch {
	case secret != "":
		raw, err := json.Marshal(secret)
		if err != nil {
			return frame
		}
		return setRequestMeta(frame, mcp.MetaResumeCredentialKey, raw)
	case call.clientMeta:
		return setRequestMeta(frame, mcp.MetaResumeCredentialKey, nil)
	}
	return frame
}

// claim records an in-flight session_start and decides whether it presents a credential,
// returning it ("" for none). The entitlement rules are in the file comment.
func (rc *resumeCreds) claim(call sessionStartCall, recovery string) string {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if call.id == "" || call.conversation == "" {
		return ""
	}
	start := resumeStart{conversation: call.conversation}
	defer func() {
		if rc.inflight == nil {
			rc.inflight = map[string]resumeStart{}
		}
		rc.inflight[call.id] = start
	}()
	if rc.store == nil || recovery != string(recoveryEstablished) || rc.conversation != "" ||
		rc.presented[call.conversation] || (call.stamp != "" && call.stamp != call.conversation) {
		return ""
	}
	entry, ok := rc.store.load(call.conversation)
	if !ok {
		return ""
	}
	if rc.presented == nil {
		rc.presented = map[string]bool{}
	}
	rc.presented[call.conversation] = true
	start.presented = true
	return entry.Secret
}

// observeResumeFrame reads a frame the daemon sent, before it is stripped. It keeps the
// credential the frame discloses, and learns the linked conversation from the response to
// a session_start. e is the frame's envelope.
func (p *reconnectingProxy) observeResumeFrame(e rpcEnvelope, frame []byte) {
	_, secrets := scrubResumeCredential(frame)
	var (
		start   resumeStart
		tracked bool
	)
	if e.isResponse() {
		start, tracked = p.rc.take(idKey(e.ID))
	}
	if len(secrets) == 0 && !tracked {
		return
	}
	p.rc.observe(secrets, start, tracked && toolCallSucceeded(frame))
}

// take removes and returns the in-flight session_start with this request id.
func (rc *resumeCreds) take(id string) (resumeStart, bool) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	start, ok := rc.inflight[id]
	delete(rc.inflight, id)
	return start, ok
}

// dropInflight forgets session_start requests that were failed locally and will never be
// answered, so the table does not grow across reconnects.
func (rc *resumeCreds) dropInflight(ids []json.RawMessage) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	for _, raw := range ids {
		delete(rc.inflight, idKey(raw))
	}
}

// observe folds a daemon frame's disclosure and a session_start's outcome into the state,
// and persists whatever this process is entitled to persist. linked reports a successful
// session_start response.
func (rc *resumeCreds) observe(secrets []string, start resumeStart, linked bool) {
	rc.mu.Lock()
	if n := len(secrets); n > 0 {
		rc.secret = secrets[n-1]
	}
	justLinked := linked && rc.conversation == "" && start.conversation != ""
	if justLinked {
		rc.conversation = start.conversation
		// A process that presented and was handed no successor does not own the entry.
		rc.owns = !start.presented || len(secrets) > 0
	}
	conv, secret := rc.conversation, rc.secret
	write := rc.store != nil && conv != "" && rc.owns && secret != "" && (justLinked || len(secrets) > 0)
	rc.mu.Unlock()
	if !write {
		return
	}
	e, err := rc.store.put(conv, secret)
	if err != nil {
		slog.Warn("serve: could not store the resume credential; a serve replacement will resume by name only", "err", err)
		return
	}
	slog.Debug("serve: resume credential recorded", "conversation", shortIDPrefix(conv), "generation", e.Generation)
}

// setRequestMeta sets (or, with a nil value, removes) one key of a request frame's
// params._meta, returning the rewritten frame. Like injectInitMeta it is fail-safe and
// envelope-preserving: any frame that does not round-trip, or whose routing envelope
// would move, is returned unchanged.
func setRequestMeta(frame []byte, key string, value json.RawMessage) []byte {
	var full map[string]json.RawMessage
	if err := json.Unmarshal(frame, &full); err != nil {
		return frame
	}
	params := map[string]json.RawMessage{}
	if raw, ok := full["params"]; ok {
		if err := json.Unmarshal(raw, &params); err != nil {
			return frame
		}
	}
	meta := map[string]json.RawMessage{}
	if raw, ok := params["_meta"]; ok {
		if err := json.Unmarshal(raw, &meta); err != nil {
			return frame
		}
	}
	if value == nil {
		delete(meta, key)
	} else {
		meta[key] = value
	}
	if !encodeInto(meta, params, "_meta") || !encodeInto(params, full, "params") {
		return frame
	}
	out, err := json.Marshal(full)
	if err != nil || !sameEnvelope(frame, out) {
		return frame
	}
	return out
}
