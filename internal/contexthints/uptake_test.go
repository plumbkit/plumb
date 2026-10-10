package contexthints

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// The report folds observations per host, and the consumption proxy is
// three-valued: a later call by the same agent that mentions a named selector is
// consumed; attributable calls that do not are unconsumed; no attributable call
// at all is unattributed, never counted against the hint.
func TestComputeUptake(t *testing.T) {
	t0 := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	obs := []Observation{
		{At: t0, Host: "claude-code", SessionID: "c1", Outcome: OutcomeEmitted, Emitted: []string{"Cart.Total"}, EmittedBytes: 100},
		{At: t0, Host: "claude-code", SessionID: "c1", AgentID: "a", Outcome: OutcomeEmitted, Emitted: []string{"pricing/discount.go"}, EmittedBytes: 50},
		{At: t0, Host: "claude-code", SessionID: "c2", Outcome: OutcomeEmitted, Emitted: []string{"Ledger.Record"}, EmittedBytes: 70},
		{At: t0, Host: "claude-code", SessionID: "c1", Outcome: OutcomeNoop, Detail: "no-seeds"},
		{At: t0, Host: "claude-code", SessionID: "c1", Outcome: OutcomeThrottled, Detail: "budget"},
		{At: t0, Host: "codex", SessionID: "x1", Outcome: OutcomeEmitted, Emitted: []string{"Cart.Add"}, EmittedBytes: 30},
	}
	var asked []string
	calls := func(agent string, from, to time.Time, limit int) ([]string, error) {
		asked = append(asked, agent)
		if !from.Equal(t0) || to.Sub(from) != 10*time.Minute || limit != 20 {
			t.Errorf("window = (%v, %v] limit %d", from, to, limit)
		}
		switch agent {
		case "c1":
			return []string{`{"name_path":"Cart.Total","uri":"cart/cart.go"}`}, nil
		case "c1/a":
			return []string{`{"file_path":"/ws/pricing/discount.go"}`}, nil
		case "c2":
			return []string{`{"pattern":"something else"}`}, nil
		}
		return nil, nil // codex: unstamped, nothing attributable
	}
	u, err := ComputeUptake(obs, t0, calls, 10*time.Minute, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(u.Hosts) != 2 || u.Hosts[0].Host != "claude-code" || u.Hosts[1].Host != "codex" {
		t.Fatalf("hosts = %+v", u.Hosts)
	}
	cc := u.Hosts[0]
	if cc.Invoked != 5 || cc.ByOutcome[OutcomeEmitted] != 3 || cc.ByDetail["noop:no-seeds"] != 1 || cc.ByDetail["throttled:budget"] != 1 {
		t.Errorf("claude counts = %+v", cc)
	}
	if cc.Consumed != 2 || cc.Unconsumed != 1 || cc.Unattributed != 0 || cc.Agents != 3 || cc.EmittedBytes != 220 {
		t.Errorf("claude consumption = consumed %d unconsumed %d unattributed %d agents %d bytes %d",
			cc.Consumed, cc.Unconsumed, cc.Unattributed, cc.Agents, cc.EmittedBytes)
	}
	cx := u.Hosts[1]
	if cx.Consumed != 0 || cx.Unconsumed != 0 || cx.Unattributed != 1 {
		t.Errorf("codex consumption = %+v, want one unattributed", cx)
	}
	if strings.Join(asked, ",") != "c1,c1/a,c2,x1" {
		t.Errorf("asked about %v, want only the emitted hints' agents", asked)
	}
}

func TestComputeUptake_NoCallSourceIsUnattributed(t *testing.T) {
	u, err := ComputeUptake([]Observation{{Host: "h", SessionID: "s", Outcome: OutcomeEmitted, Emitted: []string{"X.Y"}}}, time.Time{}, nil, time.Minute, 5)
	if err != nil || u.Hosts[0].Unattributed != 1 || u.Hosts[0].Consumed != 0 {
		t.Fatalf("uptake = %+v, %v", u, err)
	}
}

func TestComputeUptake_CallSourceErrorSurfaces(t *testing.T) {
	boom := errors.New("boom")
	_, err := ComputeUptake([]Observation{{Host: "h", SessionID: "s", Outcome: OutcomeEmitted, Emitted: []string{"X.Y"}}}, time.Time{},
		func(string, time.Time, time.Time, int) ([]string, error) { return nil, boom }, time.Minute, 5)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the call source's error", err)
	}
}

// A read-only ledger can read what the daemon wrote and can write nothing.
func TestOpenReadOnly(t *testing.T) {
	s, path := openTest(t)
	o := obs("/ws", "s1", "", time.Now())
	o.Emitted = []string{"Cart.Total"}
	if err := s.Record(o); err != nil {
		t.Fatal(err)
	}
	ro, err := OpenReadOnly(path)
	if err != nil || ro == nil {
		t.Fatalf("OpenReadOnly = %v, %v", ro, err)
	}
	defer ro.Close()
	rows, err := ro.Since(time.Time{})
	if err != nil || len(rows) != 1 || rows[0].Emitted[0] != "Cart.Total" {
		t.Fatalf("rows = %+v, %v", rows, err)
	}
	if err := ro.Record(o); err == nil {
		t.Error("a read-only ledger accepted a write")
	}
	missing, err := OpenReadOnly(path + ".absent")
	if missing != nil || err != nil {
		t.Errorf("an absent ledger = %v, %v; want nil, nil", missing, err)
	}
}
