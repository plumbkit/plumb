package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// makeRequest sends a server-initiated JSON-RPC request and waits for the
// response, satisfying the RequestFn signature.
func (ss *serveState) makeRequest(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := fmt.Sprintf("srv-%d", ss.s.reqCounter.Add(1))
	ch := make(chan json.RawMessage, 1)

	ss.s.pendingMu.Lock()
	ss.s.pending[id] = ch
	ss.s.pendingMu.Unlock()

	msg := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	ss.wrMu.Lock()
	var encErr error
	if ss.broken {
		encErr = errors.New("connection closed")
	} else if encErr = ss.encode(msg); encErr != nil {
		ss.fail(encErr)
	}
	ss.wrMu.Unlock()
	if encErr != nil {
		ss.s.pendingMu.Lock()
		delete(ss.s.pending, id)
		ss.s.pendingMu.Unlock()
		return nil, fmt.Errorf("sending %s: %w", method, encErr)
	}

	select {
	case raw := <-ch:
		return ss.parseResponse(method, raw)
	case <-ctx.Done():
		ss.s.pendingMu.Lock()
		delete(ss.s.pending, id)
		ss.s.pendingMu.Unlock()
		return nil, ctx.Err()
	}
}

// notify sends a server-initiated JSON-RPC notification (no id), satisfying the
// NotifyFn signature. Guarded by wrMu like every other socket write.
func (ss *serveState) notify(method string, params any) error {
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		msg["params"] = params
	}
	ss.wrMu.Lock()
	defer ss.wrMu.Unlock()
	if ss.broken {
		return errors.New("connection closed")
	}
	if err := ss.encode(msg); err != nil {
		ss.fail(err)
		return err
	}
	return nil
}

func (ss *serveState) parseResponse(method string, raw json.RawMessage) (json.RawMessage, error) {
	var r struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("parsing %s response: %w", method, err)
	}
	if r.Error != nil {
		return nil, fmt.Errorf("%s: %s", method, r.Error.Message)
	}
	return r.Result, nil
}
