package cli

import "testing"

func TestLogicalAgentStateRecord(t *testing.T) {
	var l logicalAgentState
	if l.record("") {
		t.Fatal("an empty ID must never record, let alone flag a shared connection")
	}
	if l.record("a") {
		t.Fatal("a lone ID must not flag a shared connection")
	}
	if l.record("a") {
		t.Fatal("re-observing the same ID must not flag a shared connection")
	}
	if !l.record("b") {
		t.Fatal("a second distinct ID must flag the connection as shared")
	}
}

func TestRecordLogicalAgent(t *testing.T) {
	var s connSession // zero value: nil logger falls back to slog.Default()
	s.recordLogicalAgent("agent-1")
	s.recordLogicalAgent("agent-2") // distinct => shared; must not panic on the nil logger
}
