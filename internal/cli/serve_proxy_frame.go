package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"

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
	for k, v := range kv {
		meta[k] = v
	}
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
	return out
}

// buildInitMeta assembles the _meta key/values the proxy injects into the
// initialize frame: the client-granted allow-dirs (when any), the stable
// proxy session ID (when set), and the proxy's working directory as an
// advisory workspace attach hint (when known). Returns nil when there is
// nothing to inject, so the frame is left byte-identical.
func buildInitMeta(dirs []string, proxySessionID, cwd string) map[string]json.RawMessage {
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
	if cwd != "" {
		if raw, err := json.Marshal(cwd); err == nil {
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
// also says so — a long-lived `plumb serve` keeps running the old binary
// after a daemon upgrade, and the agent/user otherwise has no in-band signal
// of that lag. The mismatch is harmless (the proxy reconnected transparently
// and tools stay registered), so the note reports it without prescribing an
// action an autonomous agent cannot take. An unknown daemon version falls back
// to the proxy's.
func reconnectNoteText(daemonVersion, proxyVersion string) string {
	const tail = " — your session state (read-tracking, caches) was rebuilt, so " +
		"re-read a file before editing it (or pass dirty_ok:true for a file you " +
		"wrote earlier this session) if a write is unexpectedly refused."
	if daemonVersion == "" || daemonVersion == proxyVersion {
		v := daemonVersion
		if v == "" {
			v = proxyVersion
		}
		return fmt.Sprintf("# plumb-note: plumb daemon reconnected (now %s)%s", v, tail)
	}
	return fmt.Sprintf("# plumb-note: plumb daemon reconnected (daemon now %s; this serve proxy is still %s — the mismatch is harmless; restart `plumb serve` when convenient to match versions)%s",
		daemonVersion, proxyVersion, tail)
}

// injectReconnectNote appends a one-shot informational note as an extra text
// content item to a tools/call result frame, so the agent learns its plumb
// daemon was transparently reconnected (and may have changed behaviour) on the
// first response after a reconnect. The note reports the daemon's own version
// (see reconnectNoteText).
//
// It is deliberately additive — it only *appends* a content item, never edits
// existing text — and fully fail-safe: any frame that is not a well-formed MCP
// tools/call result (an error response, a result with no content array,
// anything that does not round-trip) is returned unchanged with ok=false, so a
// malformed injection can never corrupt a real tool result.
func injectReconnectNote(frame []byte, daemonVersion, proxyVersion string) (out []byte, ok bool) {
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
		"text": reconnectNoteText(daemonVersion, proxyVersion),
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
