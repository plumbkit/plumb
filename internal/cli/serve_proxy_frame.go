package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
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

func cloneBytes(b []byte) []byte {
	c := make([]byte, len(b))
	copy(c, b)
	return c
}
