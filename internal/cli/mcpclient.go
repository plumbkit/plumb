package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
)

// mcpCliClient is a minimal MCP client for CLI commands that need to talk to
// the running daemon (e.g. plumb diagnostics). It speaks newline-delimited
// JSON-RPC 2.0 over a Unix socket.
//
// Concurrency: not safe — intended for sequential request/response use from a
// single CLI command.
type mcpCliClient struct {
	conn net.Conn
	enc  *json.Encoder
	dec  *json.Decoder
	id   int
}

func newMcpCliClient(conn net.Conn) *mcpCliClient {
	enc := json.NewEncoder(conn)
	enc.SetEscapeHTML(false)
	return &mcpCliClient{
		conn: conn,
		enc:  enc,
		dec:  json.NewDecoder(conn),
	}
}

func (c *mcpCliClient) nextID() int { c.id++; return c.id }

// Initialize performs the MCP initialize/initialized handshake.
func (c *mcpCliClient) Initialize(clientName, clientVersion string) error {
	id := c.nextID()
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"clientInfo": map[string]any{
				"name":    clientName,
				"version": clientVersion,
			},
		},
	}
	if err := c.enc.Encode(req); err != nil {
		return fmt.Errorf("send initialize: %w", err)
	}
	if _, err := c.readResponse(id); err != nil {
		return err
	}
	// Send the initialized notification (no ID, no response expected).
	return c.enc.Encode(map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	})
}

// CallTool invokes the named tool with arguments and returns its text output.
// If the tool returns an MCP error result, it is returned as a Go error.
func (c *mcpCliClient) CallTool(name string, args map[string]any) (string, error) {
	id := c.nextID()
	if args == nil {
		args = map[string]any{}
	}
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "tools/call",
		"params":  map[string]any{"name": name, "arguments": args},
	}
	if err := c.enc.Encode(req); err != nil {
		return "", fmt.Errorf("send tools/call: %w", err)
	}
	raw, err := c.readResponse(id)
	if err != nil {
		return "", err
	}
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", fmt.Errorf("parse tool result: %w", err)
	}
	var sb []byte
	for _, c := range res.Content {
		sb = append(sb, c.Text...)
	}
	if res.IsError {
		return "", fmt.Errorf("%s", string(sb))
	}
	return string(sb), nil
}

// Close closes the underlying socket.
func (c *mcpCliClient) Close() error { return c.conn.Close() }

// readResponse reads JSON-RPC messages until one matches the given request ID.
// Notifications (no ID) and other unrelated messages are silently skipped.
func (c *mcpCliClient) readResponse(wantID int) (json.RawMessage, error) {
	for {
		var msg struct {
			ID     any             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
			Method string `json:"method"`
		}
		if err := c.dec.Decode(&msg); err != nil {
			if err == io.EOF {
				return nil, fmt.Errorf("daemon closed connection")
			}
			return nil, fmt.Errorf("read response: %w", err)
		}
		// Skip notifications and server-initiated requests.
		if msg.Method != "" {
			continue
		}
		// Match by numeric ID.
		gotID, ok := numID(msg.ID)
		if !ok || gotID != wantID {
			continue
		}
		if msg.Error != nil {
			return nil, fmt.Errorf("%s", msg.Error.Message)
		}
		return msg.Result, nil
	}
}

func numID(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	}
	return 0, false
}
