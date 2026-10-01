package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/mcp"
)

// These tests pin the identity hook's resistance to a daemon that is slow or
// unreachable at the moment of the probe (issue #556). The hook used to fail
// open: any probe failure was a record with no version, so the call went out
// unstamped — and on a shared connection an unstamped write is refused. The
// instance marker already proves WHICH daemon a cached record describes, so a
// failing probe can fall back to it; these tests hold both halves of that rule.

const resilienceInstance = "pid=1 ino=2 mtime=3"

var errProbeTimeout = errors.New("dialling the daemon control socket: i/o timeout")

func failingProbe() (identityProbeRecord, error) { return identityProbeRecord{}, errProbeTimeout }

// goodProbe answers like a current daemon that lifts plumb_agent, counting its calls.
func goodProbe(probes *int) func() (identityProbeRecord, error) {
	return func() (identityProbeRecord, error) {
		*probes++
		return identityProbeRecord{DaemonVersion: "0.20.5", DeclaredKey: true}, nil
	}
}

func stampKeyOf(rec identityProbeRecord) string {
	return identityStampKey(func() identityProbeRecord { return rec })
}

// TestDaemonIdentity_FailingProbeServesTheCacheOfTheSameInstance is the
// inverse of the failure the investigation measured: the TTL has expired, the
// daemon is the same process, the probe times out — and the cached answer
// still stands, so the call is stamped with the key the daemon accepts.
func TestDaemonIdentity_FailingProbeServesTheCacheOfTheSameInstance(t *testing.T) {
	cache := filepath.Join(t.TempDir(), identityProbeCacheFile)
	t0 := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	probes := 0
	if rec := daemonIdentity(goodProbe(&probes), resilienceInstance, cache, t0); !rec.DeclaredKey || probes != 1 {
		t.Fatalf("control: the first probe must answer and be cached: %+v, probes=%d", rec, probes)
	}

	expired := t0.Add(identityProbeTTL + 5*time.Minute)
	for name, failure := range map[string]error{
		"timeout":                 errProbeTimeout,
		"daemon reads as not up":  errDaemonNotRunning, // a full listen queue on macOS refuses the dial
		"budget spent":            errIdentityProbeBudget,
		"daemon reads as too old": errDaemonVersionUnknown,
	} {
		t.Run(name, func(t *testing.T) {
			asked := 0
			probe := func() (identityProbeRecord, error) { asked++; return identityProbeRecord{}, failure }
			rec := daemonIdentity(probe, resilienceInstance, cache, expired)
			if asked != 1 {
				t.Fatalf("the probe was asked %d times; the expired record must be re-checked, not trusted blind", asked)
			}
			if rec.DaemonVersion != "0.20.5" || !rec.DeclaredKey {
				t.Fatalf("a failing probe discarded the same instance's record: %+v", rec)
			}
			if got := stampKeyOf(rec); got != mcp.ArgLogicalAgentDeclaredKey {
				t.Fatalf("stamp key %q, want %q — the call would go out unstamped", got, mcp.ArgLogicalAgentDeclaredKey)
			}
		})
	}

	// The record served in a failure is not re-stamped as fresh: nobody
	// verified it, so the next call asks again, and a probe that works then
	// replaces it.
	if rec, ok := readIdentityProbe(cache); !ok || !rec.CheckedAt.Equal(t0) {
		t.Fatalf("a failed probe rewrote the cache: %+v ok=%v", rec, ok)
	}
	probes = 0
	if rec := daemonIdentity(goodProbe(&probes), resilienceInstance, cache, expired); !rec.DeclaredKey || probes != 1 {
		t.Fatalf("the next call must probe again: %+v, probes=%d", rec, probes)
	}
	if rec, _ := readIdentityProbe(cache); !rec.CheckedAt.Equal(expired) {
		t.Fatalf("a working probe must refresh the cache: %+v", rec)
	}
}

// TestDaemonIdentity_UncertainProbeYieldsToTheCacheOfTheSameInstance: a probe
// that learned the version but not the keys (its identity-keys ask failed) is a
// guess at the safe key. For a daemon the cache already knows, the known answer
// is better — a desktop connector strips the safe key, so guessing it would
// lose the stamp the cache could have given.
func TestDaemonIdentity_UncertainProbeYieldsToTheCacheOfTheSameInstance(t *testing.T) {
	cache := filepath.Join(t.TempDir(), identityProbeCacheFile)
	t0 := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	probes := 0
	daemonIdentity(goodProbe(&probes), resilienceInstance, cache, t0)
	uncertain := func() (identityProbeRecord, error) {
		return identityProbeRecord{DaemonVersion: "0.20.5", uncertain: true}, nil
	}
	expired := t0.Add(identityProbeTTL + time.Minute)

	if got := stampKeyOf(daemonIdentity(uncertain, resilienceInstance, cache, expired)); got != mcp.ArgLogicalAgentDeclaredKey {
		t.Fatalf("an uncertain probe overrode the same instance's known answer: key %q", got)
	}
	// Control: with no record for this instance the uncertain answer is used,
	// for this call only — the safe key — and nothing is cached.
	other := filepath.Join(t.TempDir(), identityProbeCacheFile)
	if got := stampKeyOf(daemonIdentity(uncertain, resilienceInstance, other, expired)); got != mcp.ArgLogicalAgentKey {
		t.Fatalf("with no cache the uncertain answer must stamp the safe key, got %q", got)
	}
	if _, ok := readIdentityProbe(other); ok {
		t.Fatal("an uncertain answer was cached")
	}
}

// TestDaemonIdentity_FailingProbeNeverServesAnotherInstancesCache is the other
// half of the rule: the cache may vouch only for the process it asked. A
// different instance is a restarted or swapped daemon — possibly an older
// build that rejects the stamp as an unknown parameter — and a record for the
// one before it must not be used, fresh or stale.
func TestDaemonIdentity_FailingProbeNeverServesAnotherInstancesCache(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		recorded string // DaemonInstance in the cached record
		instance string // the instance the hook sees now
		age      time.Duration
	}{
		{"different instance, expired", "pid=1 ino=2 mtime=3", "pid=9 ino=8 mtime=7", identityProbeTTL + time.Minute},
		{"different instance, fresh", "pid=1 ino=2 mtime=3", "pid=9 ino=8 mtime=7", time.Second},
		{"same pid, re-bound socket", "pid=1 ino=2 mtime=3", "pid=1 ino=5 mtime=6", identityProbeTTL + time.Minute},
		{"marker unreadable now, record written without one", "", "", identityProbeTTL + time.Minute},
		{"marker unreadable now, record has one", "pid=1 ino=2 mtime=3", "", identityProbeTTL + time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := filepath.Join(t.TempDir(), identityProbeCacheFile)
			writeIdentityProbe(cache, identityProbeRecord{DaemonVersion: "0.21.0", DeclaredKey: true, DaemonInstance: tc.recorded, CheckedAt: t0})
			rec := daemonIdentity(failingProbe, tc.instance, cache, t0.Add(tc.age))
			if rec.DaemonVersion != "" || rec.DeclaredKey {
				t.Fatalf("another instance's record was served: %+v", rec)
			}
			if got := stampKeyOf(rec); got != "" {
				t.Fatalf("stamp key %q for a daemon nothing has verified", got)
			}
		})
	}
}

// TestDaemonIdentity_TTLBoundary: a record answers while it is strictly
// younger than the TTL; at the TTL it is asked again.
func TestDaemonIdentity_TTLBoundary(t *testing.T) {
	cache := filepath.Join(t.TempDir(), identityProbeCacheFile)
	t0 := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	writeIdentityProbe(cache, identityProbeRecord{DaemonVersion: "0.20.5", DeclaredKey: true, DaemonInstance: resilienceInstance, CheckedAt: t0})
	probes := 0
	older := func() (identityProbeRecord, error) {
		probes++
		return identityProbeRecord{DaemonVersion: "0.20.3"}, nil // a different answer, so a hit and a re-probe differ
	}

	if rec := daemonIdentity(older, resilienceInstance, cache, t0.Add(identityProbeTTL-time.Nanosecond)); !rec.DeclaredKey || probes != 0 {
		t.Fatalf("just inside the TTL must hit: %+v, probes=%d", rec, probes)
	}
	if rec := daemonIdentity(older, resilienceInstance, cache, t0.Add(identityProbeTTL)); rec.DeclaredKey || probes != 1 {
		t.Fatalf("at the TTL the record must be asked again: %+v, probes=%d", rec, probes)
	}
}

// TestIdentityProbeTTL_CoversAnAgentsPause: the cache has to outlast the gaps
// in a working session, or the first call after each one pays a cold probe —
// the call that used to fail open under load.
func TestIdentityProbeTTL_CoversAnAgentsPause(t *testing.T) {
	if identityProbeTTL < 5*time.Minute {
		t.Fatalf("identityProbeTTL = %s; a pause of a few minutes would leave the next call a cold probe", identityProbeTTL)
	}
}

// TestIdentityProbeLimits pins the probe's time budget against the hook's
// timeout, which is where it is measured from: a hook killed by Claude Code
// never reaches its cache fallback, so the probe has to finish inside it with
// room for process start-up and the output, and each phase has to be long
// enough for a daemon that is starved rather than gone.
func TestIdentityProbeLimits(t *testing.T) {
	if identityProbeDialTimeout < time.Second || identityProbeReplyTimeout < time.Second {
		t.Errorf("probe phases are %s dial and %s reply; under CPU saturation anything under a second misses a live daemon",
			identityProbeDialTimeout, identityProbeReplyTimeout)
	}
	var timeout time.Duration
	for _, e := range claudeHookEntries("plumb") {
		if e.event == "PreToolUse" {
			timeout = time.Duration(e.handler["timeout"].(float64) * float64(time.Second))
		}
	}
	if timeout == 0 {
		t.Fatal("no PreToolUse entry")
	}
	if identityProbeBudget+1500*time.Millisecond > timeout {
		t.Errorf("probe budget %s leaves under 1.5 s of the hook's %s timeout", identityProbeBudget, timeout)
	}
	if identityProbeBudget < identityProbeDialTimeout+identityProbeReplyTimeout {
		t.Errorf("probe budget %s cannot fit one full ask (%s + %s)", identityProbeBudget, identityProbeDialTimeout, identityProbeReplyTimeout)
	}
}

func TestParseIdentityKeysReply(t *testing.T) {
	for _, tc := range []struct {
		reply        string
		declared     bool
		wantVersion  string
		wantOldHooks bool // identityKeysReplyHasDeclared, what a hook from before the field reads
	}{
		{"ok " + mcp.ArgLogicalAgentKey + " " + mcp.ArgLogicalAgentDeclaredKey + " version=0.20.4\n", true, "0.20.4", true},
		{"ok " + mcp.ArgLogicalAgentKey + " " + mcp.ArgLogicalAgentDeclaredKey + "\n", true, "", true},
		{"ok " + mcp.ArgLogicalAgentKey + "\n", false, "", false},
		{"ok " + mcp.ArgLogicalAgentKey + " version=0.19.1\n", false, "0.19.1", false},
		{"ok " + mcp.ArgLogicalAgentDeclaredKey + " version=\n", true, "", true},
		{`error: unknown command "identity-keys"` + "\n", false, "", false},
		{"", false, "", false},
	} {
		got := parseIdentityKeysReply(tc.reply)
		if got.declared != tc.declared || got.version != tc.wantVersion || identityKeysReplyHasDeclared(tc.reply) != tc.wantOldHooks {
			t.Errorf("parseIdentityKeysReply(%q) = %+v", tc.reply, got)
		}
	}
}

// countingDaemon is fakeCtrlDaemon plus a count of connections.
func countingDaemon(t *testing.T, reply func(net.Conn, string)) *atomic.Int32 {
	t.Helper()
	var dials atomic.Int32
	fakeCtrlDaemon(t, func(c net.Conn, line string) {
		dials.Add(1)
		reply(c, line)
	})
	return &dials
}

// TestIdentityProbe_CurrentDaemonAnswersInOneDial: the daemon's identity-keys
// reply carries its version, so a current daemon is one ask, not two — each
// extra dial was another chance for a loaded machine to lose the answer.
func TestIdentityProbe_CurrentDaemonAnswersInOneDial(t *testing.T) {
	probeTestEnv(t)
	old := Version
	Version = "0.20.9"
	t.Cleanup(func() { Version = old })
	dials := countingDaemon(t, func(c net.Conn, line string) {
		p1, p2 := net.Pipe()
		go handleCtrlConn(p2, "info", "text", ctrlHandlers{}) // the real handler
		_, _ = p1.Write([]byte(line + "\n"))
		r, _ := bufio.NewReader(p1).ReadString('\n')
		_ = p1.Close()
		_, _ = c.Write([]byte(r))
	})
	rec, err := probeDaemonIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if rec.DaemonVersion != "0.20.9" || !rec.DeclaredKey || rec.uncertain {
		t.Fatalf("record %+v, want version 0.20.9 with the declared key", rec)
	}
	if n := dials.Load(); n != 1 {
		t.Fatalf("a current daemon was dialled %d times, want 1", n)
	}
}

// TestIdentityProbe_DaemonThatPredatesTheVersionFieldNeedsTwoDials: a daemon
// that answers identity-keys without its version (built before the field) or
// does not know the command still gets the full, correct probe.
func TestIdentityProbe_DaemonThatPredatesTheVersionFieldNeedsTwoDials(t *testing.T) {
	probeTestEnv(t)
	dials := countingDaemon(t, func(c net.Conn, line string) {
		switch line {
		case "version":
			_, _ = c.Write([]byte("ok 0.20.3\n"))
		case ctrlIdentityKeysCommand:
			_, _ = c.Write([]byte("ok " + mcp.ArgLogicalAgentKey + " " + mcp.ArgLogicalAgentDeclaredKey + "\n"))
		}
	})
	rec, err := probeDaemonIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if rec.DaemonVersion != "0.20.3" || !rec.DeclaredKey || rec.uncertain {
		t.Fatalf("record %+v, want 0.20.3 with the declared key, certain", rec)
	}
	if n := dials.Load(); n != 2 {
		t.Fatalf("dialled %d times, want identity-keys then version", n)
	}
}

// TestIdentityProbe_NoDaemonIsOneFailedDial: a daemon that is not there is not
// retried against a second command.
func TestIdentityProbe_NoDaemonIsOneFailedDial(t *testing.T) {
	probeTestEnv(t)
	if _, err := probeDaemonIdentity(); !errors.Is(err, errDaemonNotRunning) {
		t.Fatalf("err = %v, want errDaemonNotRunning", err)
	}
}

// TestIdentityProbe_UnansweredKeysStillGetsTheVersion: a daemon that takes the
// identity-keys connection and says nothing is "version known, keys unknown":
// an uncertain record, never a guess dressed as an answer.
func TestIdentityProbe_UnansweredKeysStillGetsTheVersion(t *testing.T) {
	probeTestEnv(t)
	fakeCtrlDaemon(t, func(c net.Conn, line string) {
		if line == "version" {
			_, _ = c.Write([]byte("ok 0.21.0\n"))
		}
	})
	rec, err := probeDaemonIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if rec.DaemonVersion != "0.21.0" || rec.DeclaredKey || !rec.uncertain {
		t.Fatalf("record %+v, want 0.21.0, no declared key, uncertain", rec)
	}
}

// TestAskDaemonCtrl_DeadlineCapsTheAsk: the probe's overall budget bounds an
// ask whose daemon never answers, and an ask begun with no budget left does not
// dial at all.
func TestAskDaemonCtrl_DeadlineCapsTheAsk(t *testing.T) {
	probeTestEnv(t)
	release := make(chan struct{})
	fakeCtrlDaemon(t, func(net.Conn, string) { <-release })
	t.Cleanup(func() { close(release) })

	start := time.Now()
	_, connected, err := askDaemonCtrl("version", start.Add(100*time.Millisecond))
	if err == nil || !connected {
		t.Fatalf("a silent daemon must be a connected failure, got connected=%v err=%v", connected, err)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("the ask took %s; its deadline was 100 ms", took)
	}
	if _, connected, err := askDaemonCtrl("version", time.Now().Add(-time.Second)); !errors.Is(err, errIdentityProbeBudget) || connected {
		t.Fatalf("no budget left must not dial: connected=%v err=%v", connected, err)
	}
}

// --- the breadcrumb -----------------------------------------------------

func breadcrumbInput(tool string) claudeHookInput {
	return claudeHookInput{
		Event: "PreToolUse", SessionID: "conv-1", AgentID: "agent-7",
		ToolName: tool, ToolUseID: "toolu_01ABC", ToolInput: json.RawMessage(`{"file_path":"/w/a.go"}`),
	}
}

func runHook(in claudeHookInput, env func(string) string, daemon func() identityProbeRecord) (stdout, stderr string) {
	var out, errOut bytes.Buffer
	runClaudePreToolUse(in, env, daemon, &out, &errOut)
	return out.String(), errOut.String()
}

// TestIdentityHook_FailingProbeWithNoCacheLeavesABreadcrumb: no usable cache and
// a failing probe is no stamp — and one stderr line saying why, with the ids
// that let the miss be matched to a call afterwards.
func TestIdentityHook_FailingProbeWithNoCacheLeavesABreadcrumb(t *testing.T) {
	cache := filepath.Join(t.TempDir(), identityProbeCacheFile)
	now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	daemon := func() identityProbeRecord { return daemonIdentity(failingProbe, resilienceInstance, cache, now) }

	stdout, stderr := runHook(breadcrumbInput("mcp__plumb__read_file"), noEnv, daemon)
	if stdout != "" {
		t.Fatalf("a call with no verifiable daemon was stamped: %s", stdout)
	}
	for _, want := range []string{`tool_name="mcp__plumb__read_file"`, `tool_use_id="toolu_01ABC"`, "i/o timeout", "no cached answer"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("breadcrumb %q lacks %q", stderr, want)
		}
	}
	if strings.Count(stderr, "\n") != 1 || !strings.HasSuffix(stderr, "\n") {
		t.Errorf("breadcrumb is not exactly one line: %q", stderr)
	}
}

// TestIdentityHook_BreadcrumbForEveryUnstampedPlumbCall: every reason a plumb
// call goes out unstamped is reported; a stamped call and a foreign tool are
// silent.
func TestIdentityHook_BreadcrumbForEveryUnstampedPlumbCall(t *testing.T) {
	predates := func() identityProbeRecord { return identityProbeRecord{DaemonVersion: "0.19.0"} }
	for _, tc := range []struct {
		name       string
		mutate     func(*claudeHookInput)
		env        func(string) string
		daemon     func() identityProbeRecord
		wantStdout bool
		wantReason string // "" means nothing on stderr
	}{
		{"stamped", func(*claudeHookInput) {}, noEnv, acceptingDaemon, true, ""},
		{"foreign tool", func(in *claudeHookInput) { in.ToolName = "mcp__other__read_file" }, noEnv, acceptingDaemon, false, ""},
		{"kill switch", func(*claudeHookInput) {}, func(k string) string {
			if k == claudeIdentityKillSwitch {
				return "off"
			}
			return ""
		}, acceptingDaemon, false, claudeIdentityKillSwitch},
		{"no session id", func(in *claudeHookInput) { in.SessionID = "" }, noEnv, acceptingDaemon, false, "no session_id"},
		{"tool input not an object", func(in *claudeHookInput) { in.ToolInput = json.RawMessage(`[1]`) }, noEnv, acceptingDaemon, false, "not a JSON object"},
		{"daemon predates the channel", func(*claudeHookInput) {}, noEnv, predates, false, "0.19.0"},
		{"daemon never probed", func(*claudeHookInput) {}, noEnv, nil, false, "no daemon check"},
		{"reason with a newline stays one line", func(*claudeHookInput) {}, noEnv, func() identityProbeRecord {
			return identityProbeRecord{probeFailure: "first\nsecond"}
		}, false, "first second"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := breadcrumbInput("mcp__plumb__read_file")
			tc.mutate(&in)
			stdout, stderr := runHook(in, tc.env, tc.daemon)
			if (stdout != "") != tc.wantStdout {
				t.Fatalf("stdout = %q, want output=%v", stdout, tc.wantStdout)
			}
			if tc.wantReason == "" {
				if stderr != "" {
					t.Fatalf("expected silence, got %q", stderr)
				}
				return
			}
			if !strings.Contains(stderr, tc.wantReason) || !strings.Contains(stderr, "toolu_01ABC") || strings.Count(stderr, "\n") != 1 {
				t.Fatalf("breadcrumb %q must be one line naming %q and the tool_use_id", stderr, tc.wantReason)
			}
		})
	}
}

// TestClaudeHookInput_ReadsToolUseID: the field the breadcrumb prints comes from
// Claude Code's own PreToolUse payload.
func TestClaudeHookInput_ReadsToolUseID(t *testing.T) {
	var in claudeHookInput
	payload := fmt.Sprintf(`{"hook_event_name":"PreToolUse","session_id":"s","tool_name":"mcp__plumb__git","tool_use_id":%q,"tool_input":{}}`, "toolu_77")
	if err := json.Unmarshal([]byte(payload), &in); err != nil {
		t.Fatal(err)
	}
	if in.ToolUseID != "toolu_77" {
		t.Fatalf("ToolUseID = %q", in.ToolUseID)
	}
}
