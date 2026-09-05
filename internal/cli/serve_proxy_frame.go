package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"

	"github.com/plumbkit/plumb/internal/mcp"
)

// Newline-delimited JSON-RPC framing for the resilient serve proxy.
//
// Both the MCP client (over stdio) and the daemon (over the Unix socket) speak
// newline-terminated JSON-RPC 2.0. The proxy reads whole frames so it can peek
// at `method`/`id` — enough to replay the handshake and track in-flight
// requests — without interpreting tool semantics.

// frameReader reads newline-delimited frames from an underlying reader.
//
// Concurrency: a frameReader is not safe for concurrent use; each direction of
// the proxy owns its own reader. A reader is bound to one connection and is
// replaced wholesale when the daemon connection is swapped on reconnect.
type frameReader struct {
	r *bufio.Reader
}

func newFrameReader(rd io.Reader) *frameReader {
	return &frameReader{r: bufio.NewReaderSize(rd, 64*1024)}
}

// read returns the next complete frame with its trailing newline stripped.
//
// A frame is only returned when a delimiter is seen, so a partial line left in
// the buffer when the peer crashes mid-write is reported as an error rather
// than forwarded as corrupt JSON. The error is io.EOF on a clean close.
func (fr *frameReader) read() ([]byte, error) {
	line, err := fr.r.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	return bytes.TrimRight(line, "\r\n"), nil
}

// writeFrame writes a single frame followed by a newline in one Write so a
// concurrent writer (guarded by the caller's mutex) never interleaves bytes
// of two frames on the same stream.
func writeFrame(w io.Writer, frame []byte) error {
	buf := make([]byte, 0, len(frame)+1)
	buf = append(buf, frame...)
	buf = append(buf, '\n')
	_, err := w.Write(buf)
	return err
}

// rpcEnvelope is the minimal slice of a JSON-RPC message the proxy inspects.
type rpcEnvelope struct {
	Method string          `json:"method"`
	ID     json.RawMessage `json:"id"`
}

func parseEnvelope(frame []byte) rpcEnvelope {
	var e rpcEnvelope
	_ = json.Unmarshal(frame, &e)
	return e
}

func (e rpcEnvelope) hasID() bool {
	return len(e.ID) > 0 && !bytes.Equal(bytes.TrimSpace(e.ID), []byte("null"))
}

// isRequest reports whether the frame is a request (method + id) — including
// the initialize request and every tool call.
func (e rpcEnvelope) isRequest() bool { return e.Method != "" && e.hasID() }

// isResponse reports whether the frame is a response (id, no method).
func (e rpcEnvelope) isResponse() bool { return e.Method == "" && e.hasID() }

// idKey normalises a JSON-RPC id to a canonical string so a daemon response can
// be matched to the request that produced it regardless of equivalent encodings
// (numbers vs strings, whitespace). Falls back to the raw text on any error.
func idKey(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return ""
	}
	var v any
	if err := json.Unmarshal(trimmed, &v); err != nil {
		return string(trimmed)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return string(trimmed)
	}
	return string(b)
}

// injectInitMeta folds the given key/value pairs into an initialize request
// frame's params._meta object in a single pass, returning the augmented frame.
// Because the resilient proxy captures and replays this exact frame, the
// metadata travels with every handshake replay automatically — no separate
// post-initialize control message is needed.
//
// Fully fail-safe and zero-cost when there is nothing to add: an empty kv map,
// or any frame that does not round-trip as a JSON object with an object params,
// is returned unchanged — so a session that injects nothing behaves exactly as
// before. An existing _meta is preserved; only the given keys are set.
//
// The rewrite is also ENVELOPE-PRESERVING by construction: before returning, the
// re-encoded frame's method and id are compared against the original's, and a
// frame whose envelope moved is returned unchanged. That guard is not
// theoretical. This function decodes into map[string]json.RawMessage, where a
// DUPLICATE key is last-wins, while the routing envelope (parseEnvelope) and the
// daemon's own mcp.Request both decode into a STRUCT, where a second copy that
// fails to fit the field type is a soft error that leaves the first value in
// place. So `{"method":"initialize","method":{}}` routes as initialize and used
// to be re-emitted as `{"method":{}}` — the proxy acting on one message and the
// daemon receiving another, which is the shape of every request-smuggling bug.
// Found by FuzzProxyFrameRewrite.
func injectInitMeta(frame []byte, kv map[string]json.RawMessage) []byte {
	if len(kv) == 0 {
		return frame
	}
	var full map[string]json.RawMessage
	if err := json.Unmarshal(frame, &full); err != nil {
		return frame
	}
	paramsRaw, ok := full["params"]
	params := map[string]json.RawMessage{}
	if ok {
		if err := json.Unmarshal(paramsRaw, &params); err != nil {
			return frame
		}
	}
	meta := map[string]json.RawMessage{}
	if metaRaw, ok := params["_meta"]; ok {
		if err := json.Unmarshal(metaRaw, &meta); err != nil {
			return frame
		}
	}
	maps.Copy(meta, kv)
	if !encodeInto(meta, params, "_meta") {
		return frame
	}
	if !encodeInto(params, full, "params") {
		return frame
	}
	out, err := json.Marshal(full)
	if err != nil {
		return frame
	}
	if !sameEnvelope(frame, out) {
		return frame
	}
	return out
}

// sameEnvelope reports whether two frames present the same routing envelope to
// the proxy — same method, same id. It asks via parseEnvelope, the function the
// router itself uses, so the guard cannot drift from what it is guarding.
func sameEnvelope(before, after []byte) bool {
	a, b := parseEnvelope(before), parseEnvelope(after)
	return a.Method == b.Method && idKey(a.ID) == idKey(b.ID)
}

// buildInitMeta assembles the _meta key/values the proxy injects into the
// initialize frame: the client-granted allow-dirs (when any), the stable
// proxy session ID (when set), and the explicit workspace pre-pin
// (--workspace/PLUMB_WORKSPACE, when given). There is no serve-cwd fallback: a
// serve started without one sends no workspace key at all, so the frame stays
// byte-identical (nil return) and the daemon has nothing to auto-attach from —
// session_start is then the sole workspace-pin authority.
func buildInitMeta(dirs []string, proxySessionID, workspace string) map[string]json.RawMessage {
	meta := map[string]json.RawMessage{}
	if len(dirs) > 0 {
		if raw, err := json.Marshal(dirs); err == nil {
			meta[mcp.MetaAllowDirsKey] = raw
		}
	}
	if proxySessionID != "" {
		if raw, err := json.Marshal(proxySessionID); err == nil {
			meta[mcp.MetaProxySessionKey] = raw
		}
	}
	if workspace != "" {
		if raw, err := json.Marshal(workspace); err == nil {
			meta[mcp.MetaWorkspaceKey] = raw
		}
	}
	if len(meta) == 0 {
		return nil
	}
	return meta
}

// injectAllowDirs folds the client-granted extra read-write roots into an
// initialize request frame's params._meta[mcp.MetaAllowDirsKey] array. Thin
// wrapper over injectInitMeta retained for the direct allow-dir tests; an empty
// dirs slice or a non-object frame is returned unchanged.
func injectAllowDirs(frame []byte, dirs []string) []byte {
	return injectInitMeta(frame, buildInitMeta(dirs, "", ""))
}

// encodeInto marshals child and stores it under key in parent, reporting
// success. A helper purely to keep injectInitMeta flat (gocyclo).
func encodeInto(child any, parent map[string]json.RawMessage, key string) bool {
	raw, err := json.Marshal(child)
	if err != nil {
		return false
	}
	parent[key] = raw
	return true
}

func cloneBytes(b []byte) []byte {
	c := make([]byte, len(b))
	copy(c, b)
	return c
}

// serverInfoVersion extracts result.serverInfo.version from an initialize
// response frame. Fail-safe like the injector below: any shape mismatch —
// an error response, missing serverInfo, malformed JSON — returns "".
func serverInfoVersion(frame []byte) string {
	var resp struct {
		Result struct {
			ServerInfo struct {
				Version string `json:"version"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal(frame, &resp); err != nil {
		return ""
	}
	return resp.Result.ServerInfo.Version
}

// reconnectNoteText builds the reconnect note. The daemon's own reported
// version leads; when it differs from this proxy's compiled version the note
// may also say so — a long-lived `plumb serve` keeps running the old binary
// after a daemon upgrade, and the agent/user otherwise has no in-band signal
// of that lag. warnMismatch selects that wording: the mismatch is harmless
// (the proxy reconnected transparently and tools stay registered), so the
// caller (annotateReconnect) passes true only once per daemon version per
// proxy — repeating the clause on every reconnect is alarm fatigue — and the
// suppressed form falls back to the plain note naming the daemon's version.
// The clause itself reports the lag without prescribing an action an
// autonomous agent cannot take. An unknown daemon version falls back to the
// proxy's.
func reconnectNoteText(daemonVersion, proxyVersion string, warnMismatch bool, outcome reconnectOutcome) string {
	if daemonVersion == "" || daemonVersion == proxyVersion || !warnMismatch {
		v := daemonVersion
		if v == "" {
			v = proxyVersion
		}
		return fmt.Sprintf("# plumb-note: %s (daemon %s)%s", outcome.cause(), v, outcome.tail())
	}
	return fmt.Sprintf("# plumb-note: %s (daemon now %s; this serve proxy is still %s — the mismatch is harmless; restart `plumb serve` when convenient to match versions)%s",
		outcome.cause(), daemonVersion, proxyVersion, outcome.tail())
}

// reconnectOutcome is what the proxy actually OBSERVED across a reconnect: did
// the daemon process change, and what did it say about this connection's
// identity.
//
// It exists because the note it feeds used to assert two things the proxy had
// not established. It said the daemon "reconnected" in wording that reads as a
// restart, when the commonest cause is an idle eviction that restarted nothing;
// and it said session state "was rebuilt" unconditionally, when the identity may
// have been recovered intact — or may have failed to recover, which the old
// wording could not distinguish from success either. An agent reading that note
// could reasonably conclude it had been renamed, which is precisely the false
// conclusion that started this work.
type reconnectOutcome struct {
	// restarted is whether the daemon PROCESS changed. Meaningful only when
	// instanceKnown; false otherwise means "not established", not "no".
	restarted bool
	// instanceKnown is whether both sides of the comparison had a process
	// marker. False against a daemon that predates the marker.
	instanceKnown bool
	// recovery is the daemon's acknowledged identity outcome (see
	// recoveryOutcome), or "" when it acknowledged none — a legacy daemon, an
	// unparseable snapshot, or a connection that is not a serve proxy.
	recovery string
	// name and sessionID are the identity the daemon acknowledged, empty when
	// none was.
	name      string
	sessionID string
}

// cause states what happened to the connection, in the strongest form the
// evidence supports and no stronger.
func (o reconnectOutcome) cause() string {
	switch {
	case !o.instanceKnown:
		return "plumb daemon connection re-established"
	case o.restarted:
		return "plumb daemon process restarted and the connection was re-established"
	default:
		return "plumb daemon connection re-established (same daemon process — a transport reconnect, not a restart)"
	}
}

// tail states what became of the session's identity and its expendable state.
//
// Identity and cached state are reported separately because they now behave
// differently: the identity is durable and usually survives, while read
// tracking, caches and — absent an explicit session_start — the pin genuinely
// are rebuilt. Collapsing both into one "state was rebuilt" sentence is what
// made a successful recovery read as a loss.
func (o reconnectOutcome) tail() string {
	const state = " Read-tracking and caches were rebuilt: re-read a file before " +
		"editing it (or pass dirty_ok:true for a file you wrote earlier this " +
		"session). The daemon restores an explicit session_start workspace, but " +
		"if you have not set one, a relative path may now resolve against a " +
		"different project — confirm the pin before a relative-path write."
	return " — " + o.identitySentence() + state
}

// identitySentence reports the identity outcome, and says "unknown" rather than
// inventing a result when the daemon acknowledged nothing.
func (o reconnectOutcome) identitySentence() string {
	switch recoveryOutcome(o.recovery) {
	case recoveryRestored, recoveryEstablished:
		if o.name != "" {
			return fmt.Sprintf("Your session identity was restored: you are still %s (%s).", o.name, o.sessionID)
		}
		return "Your session identity was restored."
	case recoveryDegraded:
		return "Your session identity could NOT be restored this time and you are running under a temporary one; " +
			"the durable record is intact, so a later reconnect will try again. Mail addressed to your previous " +
			"name may not reach you until it succeeds."
	case recoveryUnavailable:
		return "This connection has no durable session identity (persistence is off, or the client is not `plumb serve`), " +
			"so its name and ID are new."
	default:
		return "This daemon did not report an identity outcome, so whether your session name and ID carried over is unknown — " +
			"call session_start to see them."
	}
}

// injectReconnectNote appends a one-shot informational note as an extra text
// content item to a tools/call result frame, so the agent learns its plumb
// daemon was transparently reconnected (and may have changed behaviour) on the
// first response after a reconnect. The note reports the daemon's own version
// (see reconnectNoteText); warnMismatch carries annotateReconnect's
// once-per-daemon-version decision for the version-mismatch clause.
//
// It is deliberately additive — it only *appends* a content item, never edits
// existing text — and fully fail-safe: any frame that is not a well-formed MCP
// tools/call result (an error response, a result with no content array,
// anything that does not round-trip) is returned unchanged with ok=false, so a
// malformed injection can never corrupt a real tool result.
func injectReconnectNote(frame []byte, daemonVersion, proxyVersion string, warnMismatch bool, outcome reconnectOutcome) (out []byte, ok bool) {
	var full map[string]json.RawMessage
	if err := json.Unmarshal(frame, &full); err != nil {
		return frame, false
	}
	resultRaw, hasResult := full["result"]
	if !hasResult {
		return frame, false // an error response has no result — leave it untouched
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(resultRaw, &result); err != nil {
		return frame, false
	}
	contentRaw, hasContent := result["content"]
	if !hasContent {
		return frame, false // not the MCP tools/call result shape
	}
	// content is populated by Unmarshal, so a prealloc would be discarded.
	var content []json.RawMessage //nolint:prealloc // filled by json.Unmarshal below
	if err := json.Unmarshal(contentRaw, &content); err != nil {
		return frame, false
	}
	note, err := json.Marshal(map[string]string{
		"type": "text",
		"text": reconnectNoteText(daemonVersion, proxyVersion, warnMismatch, outcome),
	})
	if err != nil {
		return frame, false
	}
	content = append(content, note)
	newContent, err := json.Marshal(content)
	if err != nil {
		return frame, false
	}
	result["content"] = newContent
	newResult, err := json.Marshal(result)
	if err != nil {
		return frame, false
	}
	full["result"] = newResult
	newFrame, err := json.Marshal(full)
	if err != nil {
		return frame, false
	}
	return newFrame, true
}

// annotateReconnect appends a one-shot "daemon reconnected" note to the first
// content-bearing tool result after a transparent reconnect, so a
// silently-changed tool contract (e.g. a rebuilt daemon's new output format) is
// attributable rather than spooky. It is called for every daemon response while
// the flag is set and consumes the flag ONLY when injection actually succeeds —
// so the note lands on a real tool result, not on a ping/initialize/error
// response that happens to be the first frame back. The shape check inside
// injectReconnectNote (a `result.content` array) is the filter, which is why no
// request-id correlation is needed: the response can race ahead of its own
// request being tracked (track-after-write), so id-matching would be unreliable.
//
// pumpDaemonToClient is the sole caller and runs single-threaded, so the
// Load/Store pair needs no CAS.
func (p *reconnectingProxy) annotateReconnect(frame []byte) []byte {
	if !p.reconnected.Load() {
		return frame
	}
	p.hsMu.Lock()
	daemonV := p.daemonVersion
	// The version-mismatch clause fires ONCE per daemon version per proxy —
	// the mismatch is harmless, so warning on every reconnect is alarm
	// fatigue. A daemon version change re-arms it. The flag is recorded only
	// after a successful injection, so a frame the note could not attach to
	// keeps the warning armed rather than silently consuming it.
	warnMismatch := daemonV != "" && daemonV != Version && p.notifiedMismatch != daemonV
	p.hsMu.Unlock()
	annotated, ok := injectReconnectNote(frame, daemonV, Version, warnMismatch, p.reconnectOutcome())
	if !ok {
		return frame // not a tool result — keep the note armed for the next response
	}
	if warnMismatch {
		p.hsMu.Lock()
		p.notifiedMismatch = daemonV
		p.hsMu.Unlock()
	}
	p.reconnected.Store(false)
	return annotated
}

// reconnectOutcome assembles what this proxy actually observed across the
// reconnect — whether the daemon process changed, and what it acknowledged about
// this connection's identity — so the note reports rather than assumes.
func (p *reconnectingProxy) reconnectOutcome() reconnectOutcome {
	restarted, instanceKnown := p.daemonRestarted()
	id := p.heldIdentity()
	return reconnectOutcome{
		restarted:     restarted,
		instanceKnown: instanceKnown,
		recovery:      id.recovery,
		name:          id.name,
		sessionID:     id.sessionID,
	}
}
