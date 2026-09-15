package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// daemon_info_proxy_test.go covers the row that exists because this tool
// confidently answered a different question than the one being asked.
//
// "Which version am I running?" has two answers. `plumb restart` replaces the
// daemon; each session's `plumb serve` proxy keeps the binary it launched with
// until its client restarts. So a proxy-side change can be merged, released,
// installed and live in the daemon while the session asking is still executing
// the old code — and daemon_info reported only the daemon's version, which is
// the one that is NOT in question when someone is checking a proxy-side fix.

func TestProxyVersionRow(t *testing.T) {
	cases := []struct {
		name          string
		proxy, daemon string
		want          []string // substrings that must all be present
		absent        []string
	}{
		{
			name:   "unknown when nobody declared one",
			proxy:  "",
			daemon: "0.19.3",
			want:   []string{"unknown"},
			// It must not read as agreement. "" is "nobody said", and saying
			// nothing is exactly how the wrong belief formed in the first place.
			absent: []string{"matches"},
		},
		{
			name:   "matching proxy says so",
			proxy:  "0.19.3",
			daemon: "0.19.3",
			want:   []string{"0.19.3", "matches this daemon"},
			absent: []string{"DIFFERS"},
		},
		{
			// The load-bearing case. Reporting the numbers is not enough — the
			// reader's next move is to restart the daemon, which will not change
			// it. The row has to say that.
			name:   "mismatch names the action that actually fixes it",
			proxy:  "0.19.2",
			daemon: "0.19.3",
			want:   []string{"0.19.2", "DIFFERS", "plumb serve", "daemon restart does not"},
			absent: []string{"matches this daemon"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := proxyVersionRow(tc.proxy, tc.daemon)
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("row %q missing %q", got, want)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(got, absent) {
					t.Errorf("row %q must not contain %q", got, absent)
				}
			}
		})
	}
}

// TestDaemonInfo_ReportsProxyVersion drives Execute, because the row being
// correct is worth nothing if it never reaches the output the agent reads.
func TestDaemonInfo_ReportsProxyVersion(t *testing.T) {
	newTool := func(proxy string) *daemonInfo {
		tool := NewDaemonInfo("sess-1", "swift-falcon", "0.19.3", time.Now())
		if proxy != "" {
			tool = tool.WithProxyVersion(func() string { return proxy })
		}
		return tool
	}

	// Wired and mismatched: the case the tool exists for.
	out, err := newTool("0.19.2").Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "proxy version:") {
		t.Fatalf("no proxy row in output:\n%s", out)
	}
	if !strings.Contains(out, "DIFFERS") || !strings.Contains(out, "plumb serve") {
		t.Errorf("a mismatched proxy must say so and name the remedy:\n%s", out)
	}
	// Both numbers must be present and distinguishable — reporting one of them
	// is the bug.
	if !strings.Contains(out, "daemon version: 0.19.3") || !strings.Contains(out, "0.19.2") {
		t.Errorf("both versions must appear:\n%s", out)
	}

	// NOT wired: no accessor at all, which is a direct client or an older proxy.
	// The row must still appear and must say unknown rather than vanish — a
	// missing row reads as "no such question".
	unwired, err := newTool("").Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(unwired, "proxy version:") || !strings.Contains(unwired, "unknown") {
		t.Errorf("an unwired proxy version must still render an unknown row:\n%s", unwired)
	}
}
