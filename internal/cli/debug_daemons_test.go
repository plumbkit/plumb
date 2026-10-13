package cli

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/daemons"
)

func TestRenderDaemonCensus(t *testing.T) {
	report := daemons.Report{
		Authority: "/data/plumb/daemon.authority.lock",
		Daemons: []daemons.Daemon{{
			PID:            4242,
			PPID:           1,
			Start:          time.Date(2026, 10, 12, 13, 32, 10, 0, time.UTC),
			Executable:     "/opt/homebrew/bin/plumb",
			Argv:           "plumb daemon",
			Identity:       daemons.IdentityAuthority,
			HoldsAuthority: true,
			OwnsSocket:     []string{"/run/plumb/plumb.sock"},
		}},
		Unknown: []string{"cannot read the owner of /run/plumb/plumb.sock"},
	}
	got := renderDaemonCensus(report, report.Verdict(), nil)

	for _, want := range []string{
		"4242", // the pid
		"2026-10-12T13:32:10Z",
		"/opt/homebrew/bin/plumb",
		"authority", // how it was found
		"UNKNOWN",   // an unreadable source is never a clean verdict
		"unknown: cannot read the owner",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("renderDaemonCensus output is missing %q:\n%s", want, got)
		}
	}

	// A violation must name the remedy; a report with nothing in it must not
	// print an empty table.
	empty := daemons.Report{Authority: "/data/plumb/daemon.authority.lock"}
	if out := renderDaemonCensus(empty, empty.Verdict(), nil); !strings.Contains(out, "no plumb daemon process was found") {
		t.Errorf("an empty census should say so:\n%s", out)
	}
}

func TestRenderDaemonCensus_UnknownTable(t *testing.T) {
	// A daemon whose start time and executable could not be read renders dashes,
	// never a zero time (which would read as "just started").
	report := daemons.Report{
		Daemons: []daemons.Daemon{{PID: 7, Identity: daemons.IdentityCandidate}},
	}
	out := renderDaemonCensus(report, report.Verdict(), nil)
	if strings.Contains(out, "0001-01-01") {
		t.Errorf("a start time that could not be read must render as a dash:\n%s", out)
	}
	if !strings.Contains(out, "candidate") {
		t.Errorf("an unconfirmed look-alike should be named as such:\n%s", out)
	}
}

func TestDaemonCensusResult(t *testing.T) {
	tests := []struct {
		name     string
		verdict  daemons.Verdict
		wantOK   bool
		wantWarn bool
	}{
		{"one daemon holding the authority", daemons.Verdict{State: daemons.StateOK, Detail: "1 plumb daemon"}, true, false},
		{"no daemon", daemons.Verdict{State: daemons.StateNone, Detail: "none"}, true, true},
		{
			"two daemons",
			daemons.Verdict{State: daemons.StateViolation, Detail: "2 plumb daemons", Remedy: "run `plumb stop`"},
			false, false,
		},
		{
			"a census that could not read the table",
			daemons.Verdict{State: daemons.StateUnknown, Detail: "cannot tell", Remedy: "re-run"},
			false, false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows := daemonCensusResult(tt.verdict)
			if len(rows) != 1 {
				t.Fatalf("got %d rows, want 1", len(rows))
			}
			row := rows[0]
			if row.name != "census" {
				t.Errorf("row name = %q, want census", row.name)
			}
			if row.ok != tt.wantOK || row.warn != tt.wantWarn {
				t.Errorf("row = {ok:%v warn:%v}, want {ok:%v warn:%v}", row.ok, row.warn, tt.wantOK, tt.wantWarn)
			}
			if !tt.wantOK && row.fix == "" {
				t.Error("a failed census row must carry a fix")
			}
		})
	}
}

func TestCensusMemo_ReusesWithinTheTTLAndRecomputesAfter(t *testing.T) {
	memo := censusMemo{ttl: 10 * time.Second}
	now := time.Now()
	calls := 0
	compute := func() string {
		calls++
		return fmt.Sprintf("row-%d", calls)
	}

	if got := memo.get(now, compute); got != "row-1" {
		t.Fatalf("first get = %q, want row-1", got)
	}
	if got := memo.get(now.Add(9*time.Second), compute); got != "row-1" {
		t.Errorf("get within the TTL = %q, want the cached row-1", got)
	}
	if calls != 1 {
		t.Errorf("compute ran %d times within the TTL, want 1", calls)
	}
	if got := memo.get(now.Add(11*time.Second), compute); got != "row-2" {
		t.Errorf("get after the TTL = %q, want a fresh row-2", got)
	}
	if calls != 2 {
		t.Errorf("compute ran %d times after the TTL, want 2", calls)
	}
}

func TestCensusMemo_DoesNotCacheAnEmptyRow(t *testing.T) {
	// An empty row means "nobody could count". Caching it would keep the row
	// quiet about a real problem for the whole TTL.
	memo := censusMemo{ttl: time.Minute}
	now := time.Now()
	if got := memo.get(now, func() string { return "" }); got != "" {
		t.Fatalf("get with an empty compute = %q, want empty", got)
	}
	calls := 0
	if got := memo.get(now.Add(time.Second), func() string { calls++; return "row" }); got != "row" || calls != 1 {
		t.Errorf("get after an empty row = %q (calls %d), want a fresh row", got, calls)
	}
}

func TestDaemonCensusRowFor(t *testing.T) {
	tests := []struct {
		name    string
		verdict daemons.Verdict
		want    string
	}{
		{"ok", daemons.Verdict{State: daemons.StateOK}, "1 (expected 1), holding the launch authority"},
		{"none", daemons.Verdict{State: daemons.StateNone}, "0 (expected 1 while a client is connected)"},
		{
			"violation",
			daemons.Verdict{State: daemons.StateViolation, Confirmed: 3, Remedy: "run `plumb stop`"},
			"3 (expected 1) — VIOLATION: run `plumb stop`",
		},
		{"unknown", daemons.Verdict{State: daemons.StateUnknown, Detail: "cannot tell"}, "unknown (expected 1) — cannot tell"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := daemonCensusRowFor(tt.verdict); got != tt.want {
				t.Errorf("daemonCensusRowFor = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDebugDaemonsExit(t *testing.T) {
	// The rule a script or the doctor gate reads: a violation and an unreadable
	// census are both failures, a clean account and an idle one are not.
	tests := []struct {
		name    string
		state   daemons.State
		wantErr bool
	}{
		{"clean", daemons.StateOK, false},
		{"idle", daemons.StateNone, false},
		{"violation", daemons.StateViolation, true},
		{"cannot tell", daemons.StateUnknown, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := debugDaemonsExit(daemons.Verdict{State: tt.state}, nil)
			if (err != nil) != tt.wantErr {
				t.Errorf("debugDaemonsExit(%v) error = %v, want error: %v", tt.state, err, tt.wantErr)
			}
		})
	}
}

func TestDaemonCensusOptions_NamesTheLockAndBothSockets(t *testing.T) {
	// The options are one function so the CLI, doctor and daemon_info cannot
	// disagree about which lock is the authority or which sockets are plumb's.
	withTempRuntime(t)
	opts := daemonCensusOptions()
	if opts.Authority != daemonLockPath() {
		t.Errorf("authority = %q, want the lifetime lock %q", opts.Authority, daemonLockPath())
	}
	if len(opts.Sockets) == 0 || opts.Sockets[0] != daemonSocketPath() {
		t.Errorf("sockets = %v, want the current socket first", opts.Sockets)
	}
}
