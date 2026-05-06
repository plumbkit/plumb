package jsonrpc

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

// MockCaller is a test double for Caller. It records all calls and lets tests
// register per-method handlers that return canned responses.
// Concurrency: all methods are safe for concurrent use.
type MockCaller struct {
	mu       sync.Mutex
	handlers map[string]func(params json.RawMessage) (any, error)
	calls    []RecordedCall
	onNotify func(method string, params json.RawMessage)
}

// RecordedCall is a single invocation captured by MockCaller.
type RecordedCall struct {
	Method string
	Params json.RawMessage
}

// NewMockCaller returns an empty MockCaller with no handlers registered.
func NewMockCaller() *MockCaller {
	return &MockCaller{
		handlers: make(map[string]func(json.RawMessage) (any, error)),
	}
}

// Handle registers a handler for method. Panics if called concurrently with
// Call — register all handlers before the test begins.
func (m *MockCaller) Handle(method string, fn func(params json.RawMessage) (any, error)) {
	m.mu.Lock()
	m.handlers[method] = fn
	m.mu.Unlock()
}

// HandleOK registers a handler that always succeeds with result.
func (m *MockCaller) HandleOK(method string, result any) {
	m.Handle(method, func(_ json.RawMessage) (any, error) { return result, nil })
}

// HandleErr registers a handler that always returns err.
func (m *MockCaller) HandleErr(method string, err error) {
	m.Handle(method, func(_ json.RawMessage) (any, error) { return nil, err })
}

// Calls returns a snapshot of all recorded calls in invocation order.
func (m *MockCaller) Calls() []RecordedCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]RecordedCall, len(m.calls))
	copy(out, m.calls)
	return out
}

// Call records the invocation, finds the handler, and returns its result.
func (m *MockCaller) Call(_ context.Context, method string, params, result any) error {
	p, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("mock: marshaling params for %s: %w", method, err)
	}

	m.mu.Lock()
	m.calls = append(m.calls, RecordedCall{Method: method, Params: p})
	fn, ok := m.handlers[method]
	m.mu.Unlock()

	if !ok {
		return fmt.Errorf("mock: no handler registered for %q", method)
	}
	res, err := fn(p)
	if err != nil {
		return err
	}
	if result != nil && res != nil {
		b, merr := json.Marshal(res)
		if merr != nil {
			return fmt.Errorf("mock: marshaling result for %s: %w", method, merr)
		}
		return json.Unmarshal(b, result)
	}
	return nil
}

// Notify records the notification. No response is produced.
func (m *MockCaller) Notify(_ context.Context, method string, params any) error {
	p, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("mock: marshaling params for %s: %w", method, err)
	}
	m.mu.Lock()
	m.calls = append(m.calls, RecordedCall{Method: method, Params: p})
	m.mu.Unlock()
	return nil
}

// SetNotificationHandler registers fn for simulated server-push notifications.
func (m *MockCaller) SetNotificationHandler(fn func(method string, params json.RawMessage)) {
	m.mu.Lock()
	m.onNotify = fn
	m.mu.Unlock()
}

// Push simulates a server-initiated notification.
func (m *MockCaller) Push(method string, params any) error {
	m.mu.Lock()
	fn := m.onNotify
	m.mu.Unlock()
	if fn == nil {
		return nil
	}
	p, err := json.Marshal(params)
	if err != nil {
		return err
	}
	fn(method, p)
	return nil
}

// Close is a no-op for the mock.
func (m *MockCaller) Close() error { return nil }
