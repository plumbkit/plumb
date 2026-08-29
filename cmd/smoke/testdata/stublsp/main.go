// Command stublsp is a language server that completes its handshake and then
// never answers a textDocument/documentSymbol request — the limit case of a
// cold server still loading the package graph, which is what a real gopls looks
// like on a CI runner with a cold module cache.
//
// It exists so the PLAN-390 regression can be driven deterministically end to
// end over the wire, instead of waiting for a slow runner to reproduce it by
// luck. Nothing here is a general-purpose fake: internal/lsp/lsptest is the
// scenario-driven fake for protocol behaviour. This one has exactly one
// property, and it is a refusal to answer.
//
// It lives under testdata/ so the go tool never builds it as part of the
// module; the test that needs it compiles it on demand.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	// No omitempty: a JSON-RPC response must carry a result even when it is null.
	Result any `json:"result"`
}

func main() {
	r := bufio.NewReader(os.Stdin)
	w := bufio.NewWriter(os.Stdout)
	for {
		body, err := readFrame(r)
		if err != nil {
			return // stdin closed: the daemon is done with us.
		}
		var m message
		if json.Unmarshal(body, &m) != nil {
			continue
		}
		if len(m.ID) == 0 {
			if m.Method == "exit" {
				return
			}
			continue // a notification needs no reply
		}
		switch m.Method {
		case "textDocument/documentSymbol":
			// The whole point: accept the request and never answer it.
			continue
		case "initialize":
			reply(w, m.ID, map[string]any{
				"capabilities": map[string]any{
					"textDocumentSync":       1,
					"documentSymbolProvider": true,
				},
				"serverInfo": map[string]any{"name": "stublsp", "version": "0"},
			})
		default:
			// Everything else (shutdown, and any capability plumb probes for)
			// answers null, so only the one method under test is slow.
			reply(w, m.ID, nil)
		}
	}
}

func readFrame(r *bufio.Reader) ([]byte, error) {
	length := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if name, value, ok := strings.Cut(line, ":"); ok &&
			strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			length, err = strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return nil, err
			}
		}
	}
	if length < 0 {
		return nil, io.ErrUnexpectedEOF
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return body, nil
}

func reply(w *bufio.Writer, id json.RawMessage, result any) {
	body, err := json.Marshal(message{JSONRPC: "2.0", ID: id, Result: result})
	if err != nil {
		return
	}
	fmt.Fprintf(w, "Content-Length: %d\r\n\r\n", len(body))
	_, _ = w.Write(body)
	_ = w.Flush()
}
