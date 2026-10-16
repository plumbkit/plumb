package stats

import (
	"slices"
	"testing"
	"time"
)

// agentInputsFixture records one hint window's worth of calls across every
// connection shape the consumption proxy must tell apart. Each call's input
// names its connection, so a result list says exactly which rows were credited.
func agentInputsFixture(t *testing.T) (*DB, time.Time) {
	t.Helper()
	db := openFailuresDB(t)
	hint := time.Date(2026, 10, 12, 10, 44, 0, 0, time.UTC)
	rec := func(sid, agent, tool, in string, at time.Duration) {
		t.Helper()
		c := Call{Workspace: "/w", SessionID: sid, LogicalAgent: agent, Tool: tool, InputJSON: in, Success: true, CalledAt: hint.Add(at)}
		if err := db.Record(c); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	declare := func(sid, conv string, at time.Duration) {
		rec(sid, "", "session_start", `{"detail":"brief","session_id":"`+conv+`"}`, at)
	}
	// solo: the Codex shape. The first prompt's hint comes BEFORE the agent's
	// first plumb call, so its declaration lands inside the window, after the hint.
	declare("solo", "conv-A", 5*time.Second)
	rec("solo", "", "read_symbol", `{"conn":"solo"}`, 10*time.Second)
	// early: a Claude Code session without subagents that declared long before.
	declare("early", "conv-A", -time.Hour)
	rec("early", "", "read_file", `{"conn":"early"}`, 20*time.Second)
	// stamped: a shared connection's call carrying the agent itself.
	rec("stamped", "conv-A", "read_file", `{"conn":"stamped"}`, 25*time.Second)
	// two: one connection declared by two conversations, whose unstamped calls
	// could be either's.
	declare("two", "conv-A", -time.Minute)
	declare("two", "conv-B", -30*time.Second)
	rec("two", "", "read_file", `{"conn":"two"}`, 30*time.Second)
	// undeclared: a session_start that named no one, so somebody unannounced
	// may hold the connection too.
	declare("undeclared", "conv-A", -time.Minute)
	rec("undeclared", "", "session_start", `{"detail":"brief"}`, -30*time.Second)
	rec("undeclared", "", "read_file", `{"conn":"undeclared"}`, 35*time.Second)
	// shared: declared by conv-A alone, but it carried a stamped call (outside
	// the window), so its unstamped calls belong to no one in particular.
	declare("shared", "conv-A", -time.Minute)
	rec("shared", "conv-B", "read_file", `{"conn":"shared-stamped-B"}`, -2*time.Hour)
	rec("shared", "", "read_file", `{"conn":"shared"}`, 40*time.Second)
	// other: a connection another conversation holds.
	declare("other", "conv-B", -time.Minute)
	rec("other", "", "read_file", `{"conn":"other"}`, 45*time.Second)
	// garbage: an unparseable session_start input cannot declare anyone, and it
	// must not fail the whole query either.
	declare("garbage", "conv-A", -time.Minute)
	rec("garbage", "", "session_start", `not json`, -30*time.Second)
	rec("garbage", "", "read_file", `{"conn":"garbage"}`, 50*time.Second)
	// Bounds: a call AT the hint is not after it; one at the window's end is in.
	rec("solo", "", "read_file", `{"conn":"solo-at-hint"}`, 0)
	rec("solo", "", "read_file", `{"conn":"solo-at-end"}`, 10*time.Minute)
	rec("solo", "", "read_file", `{"conn":"solo-after-end"}`, 10*time.Minute+time.Millisecond)
	return db, hint
}

func TestInputsForAgent_CreditsTheConnectionTheAgentAloneDeclared(t *testing.T) {
	db, hint := agentInputsFixture(t)
	got, err := db.InputsForAgent("conv-A", hint, hint.Add(10*time.Minute), 50)
	if err != nil {
		t.Fatalf("InputsForAgent: %v", err)
	}
	want := []string{
		`{"detail":"brief","session_id":"conv-A"}`, // solo's own declaration, inside the window
		`{"conn":"solo"}`,
		`{"conn":"early"}`,
		`{"conn":"stamped"}`,
		`{"conn":"solo-at-end"}`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("inputs credited to conv-A:\n got %q\nwant %q", got, want)
	}
}

func TestInputsForAgent_AnotherAgentGetsOnlyItsOwnRows(t *testing.T) {
	db, hint := agentInputsFixture(t)
	got, err := db.InputsForAgent("conv-B", hint, hint.Add(10*time.Minute), 50)
	if err != nil {
		t.Fatalf("InputsForAgent: %v", err)
	}
	// other is conv-B's alone. two is shared by both declarations and shared is
	// stamped, so neither is credited to conv-B either.
	if want := []string{`{"conn":"other"}`}; !slices.Equal(got, want) {
		t.Errorf("inputs credited to conv-B:\n got %q\nwant %q", got, want)
	}
}

func TestInputsForAgent_LimitKeepsTheOldest(t *testing.T) {
	db, hint := agentInputsFixture(t)
	got, err := db.InputsForAgent("conv-A", hint, hint.Add(10*time.Minute), 2)
	if err != nil {
		t.Fatalf("InputsForAgent: %v", err)
	}
	if want := []string{`{"detail":"brief","session_id":"conv-A"}`, `{"conn":"solo"}`}; !slices.Equal(got, want) {
		t.Errorf("limit 2:\n got %q\nwant %q", got, want)
	}
}

func TestInputsForAgent_NoAgentNoLimitNoDB(t *testing.T) {
	db, hint := agentInputsFixture(t)
	for _, tc := range []struct {
		name  string
		db    *DB
		agent string
		limit int
	}{
		{"empty agent", db, "", 50},
		{"zero limit", db, "conv-A", 0},
		{"nil db", nil, "conv-A", 50},
	} {
		got, err := tc.db.InputsForAgent(tc.agent, hint, hint.Add(10*time.Minute), tc.limit)
		if err != nil || got != nil {
			t.Errorf("%s: got %q, %v; want nothing", tc.name, got, err)
		}
	}
}
