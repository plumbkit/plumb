package cli

import (
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/plumbkit/plumb/internal/daemons"
	"github.com/plumbkit/plumb/internal/tui"
)

// The daemon census (PLAN-531) answers the one question 64 leaked daemons made
// nobody ask: how many plumb daemons are running on this account, and is the one
// that should be? It is deliberately local — it dials no socket, because the
// situation it exists for is a daemon whose socket has been unlinked — and
// deliberately read-only: it opens nothing for write and takes no lock, since
// even a momentary hold on the launch authority would make a concurrent
// `plumb serve` wait out its whole 30 s start deadline and then fail.

var debugDaemonsCmd = &cobra.Command{
	Use:   "daemons",
	Short: "List this account's plumb daemons and flag any that is not the authority holder",
	Long: `List every plumb daemon running as this user, and say whether the account has
exactly the one it should.

A daemon that has lost its runtime directory is invisible in the usual way: its
socket is gone, so nothing dials it and nothing complains. It still holds memory
and per-project databases, and it is what "one daemon per user" is supposed to
prevent. This command finds those by their own argv where the platform can be
trusted to give it (Linux: after skipping processes that have forked but not yet
exec'd; macOS: after skipping them the same way), and on any platform by which
process owns a plumb socket or holds the lifetime lock.

It reports, and nothing else: it never signals a process, never removes a stale
pid file, and never takes a lock. Exit status is 0 only when the account has
exactly one daemon and that daemon holds the authority; a census that could not
read the process table exits non-zero too, because "cannot tell" is not "fine".`,
	Args: cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error { return runDebugDaemons(os.Stdout) },
}

// daemonCensusOptions names what the census should inspect. It is one function
// so the CLI, doctor and the daemon_info tool cannot drift apart about which
// lock is the authority or which sockets belong to plumb.
//
// The AUTHORITY is the ACCOUNT lock, never the runtime lifetime lock it used to
// name. PLAN-527 made the account authority the thing every launcher takes and
// every daemon holds, so it is what "holds the launch authority" means; the
// lifetime lock is a runtime file that can be deleted with its directory while
// the flock lives on the unlinked inode (PLAN-529's self-heal does exactly that),
// and reading it as the authority made a healthy daemon report as a violation.
// The lifetime locks are still named — as LegacyLocks, where holding one is
// evidence of a daemon of an older runtime directory — and the sockets of those
// directories are named with them, so a survivor is found however it presents
// itself.
// PLAN-529's socket state is checked against the account authority's
// record, not this lock.
func daemonCensusOptions() daemons.Options {
	opts := daemons.Options{}
	if path, err := authorityPath(); err == nil {
		opts.Authority = path
	}
	opts.Sockets = append(opts.Sockets, daemonSocketPath())
	for _, loc := range legacyLocations() {
		opts.Sockets = append(opts.Sockets, loc.Socket)
		opts.LegacyLocks = append(opts.LegacyLocks, loc.Lock)
	}
	if legacy := legacyDaemonSocketPath(); legacy != "" && !slices.Contains(opts.Sockets, legacy) {
		opts.Sockets = append(opts.Sockets, legacy)
	}
	return opts
}

func runDebugDaemons(out io.Writer) error {
	report := daemons.Census(daemonCensusOptions())
	verdict := report.Verdict()
	states := daemonSocketStates(report)
	fmt.Fprint(out, renderDaemonCensus(report, verdict, states))
	return debugDaemonsExit(verdict, states)
}

// debugDaemonsExit is the exit rule. The report has already said what is wrong
// and what to do about it; this exists so a script (or the doctor gate) can read
// the answer, and it is a rule rather than an expression so it can be tested: a
// violation, a census that could not read the process table, and a socket path
// that no longer holds the socket its live daemon published are all non-zero,
// while a clean account and an idle one are success.
//
// The socket fault is part of the exit rule because the count alone cannot see
// it: exactly one daemon holding the authority, whose socket path has been
// replaced under it, is one daemon that no client can reach — the state that used
// to read as perfectly healthy.
func debugDaemonsExit(v daemons.Verdict, states []socketPathState) error {
	switch v.State {
	case daemons.StateOK, daemons.StateNone:
	default:
		return silentExitError{}
	}
	for _, s := range states {
		if s.fault() != "" {
			return silentExitError{}
		}
	}
	return nil
}

// daemonCensusRow is the one-line form of the census for daemon_info, which has
// no columns to align and one job for this fact: to make "exactly one daemon"
// visible in the place an agent already looks. It is memoised for
// daemonCensusTTL, because an agent calls daemon_info repeatedly and an uncached
// census costs a process-table read plus, on macOS, two execs (measured at 1.46 s
// cold on the owner's machine).
func daemonCensusRow() string {
	return daemonCensusMemo.get(time.Now(), daemonCensusCompute)
}

// daemonCensusCompute runs the census and renders the row.
func daemonCensusCompute() string {
	report := daemons.Census(daemonCensusOptions())
	return daemonCensusRowFor(report.Verdict())
}

// daemonCensusTTL is how long the daemon_info row may be reused. It is short on
// purpose: the row exists so that a violation is noticed, and a stale "1" would
// be exactly the blind spot this tool was written to remove.
const daemonCensusTTL = 10 * time.Second

// daemonCensusMemo is the single memo the daemon_info row uses.
//
// Only that row is memoised. `plumb debug daemons` and `doctor` call
// daemons.Census directly, so a diagnosis is never answered from a cached
// reading — a person asking "how many daemons are there right now?" gets an
// answer about now.
var daemonCensusMemo = censusMemo{ttl: daemonCensusTTL}

// censusMemo is a one-entry, short-lived memo, split out from the row so the TTL
// behaviour can be tested without a process table or a clock.
type censusMemo struct {
	mu   sync.Mutex
	row  string
	when time.Time
	ttl  time.Duration
}

// get returns the memoised row when it is younger than the TTL, and otherwise
// computes, stores and returns a fresh one.
func (m *censusMemo) get(now time.Time, compute func() string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.row != "" && now.Sub(m.when) < m.ttl {
		return m.row
	}
	// An empty row is stored as empty, which the check above then reads as "no
	// cache": nobody could count, so the next caller must try again rather than
	// be told the same nothing.
	m.row, m.when = compute(), now
	return m.row
}

// daemonCensusRowFor is the pure half of daemonCensusRow.
func daemonCensusRowFor(v daemons.Verdict) string {
	switch v.State {
	case daemons.StateOK:
		return "1 (expected 1), holding the launch authority"
	case daemons.StateNone:
		return "0 (expected 1 while a client is connected)"
	case daemons.StateViolation:
		return fmt.Sprintf("%d (expected 1) — VIOLATION: %s", v.Confirmed, v.Remedy)
	default:
		return "unknown (expected 1) — " + v.Detail
	}
}

// renderDaemonCensus lays the report out for a human: one row per process, then
// the socket paths with something to say, then whatever could not be read, then
// the verdict. Presentation lives here rather than in the census so the census
// stays reusable by doctor and daemon_info, which have no columns to align.
func renderDaemonCensus(r daemons.Report, v daemons.Verdict, sockets []socketPathState) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n", verdictLine(v))
	if len(r.Daemons) == 0 {
		b.WriteString("  no plumb daemon process was found for this account\n")
	}
	if len(r.Daemons) > 0 {
		w := tabwriter.NewWriter(&b, 0, 2, 2, ' ', 0)
		fmt.Fprintln(w, "  PID\tPPID\tSTARTED\tEXECUTABLE\tAUTHORITY\tSOCKET\tFOUND BY")
		for _, d := range r.Daemons {
			fmt.Fprintf(w, "  %d\t%s\t%s\t%s\t%s\t%s\t%s\n",
				d.PID,
				dashIfZero(d.PPID),
				formatDaemonStart(d.Start),
				dashIfEmpty(d.Executable),
				yesNo(d.HoldsAuthority),
				yesNo(len(d.OwnsSocket) > 0),
				d.Identity.String(),
			)
		}
		_ = w.Flush()
	}
	// A socket path is printed only when there is something to say about it: a
	// path a live daemon published that no longer holds its socket (a fault), or a
	// socket file nothing serves. A healthy path is already visible in the table's
	// SOCKET column.
	for _, s := range sockets {
		switch {
		case s.fault() != "":
			fmt.Fprintf(&b, "  %s %s: %s\n", tui.WarnStyle.Render("!"), s.Path, s.fault())
		case s.note() != "":
			fmt.Fprintf(&b, "  %s %s: %s\n", tui.MutedStyle.Render("-"), s.Path, s.note())
		}
	}
	for _, reason := range r.Unknown {
		fmt.Fprintf(&b, "  %s unknown: %s\n", tui.WarnStyle.Render("!"), reason)
	}
	if v.Remedy != "" {
		fmt.Fprintf(&b, "\n  %s\n", v.Remedy)
	}
	return b.String()
}

func verdictLine(v daemons.Verdict) string {
	switch v.State {
	case daemons.StateOK:
		return tui.OkStyle.Render("OK") + " — " + v.Detail
	case daemons.StateNone:
		return tui.MutedStyle.Render("none") + " — " + v.Detail
	case daemons.StateViolation:
		return tui.WarnStyle.Render("VIOLATION") + " — " + v.Detail
	default:
		return tui.WarnStyle.Render("UNKNOWN") + " — " + v.Detail
	}
}

// formatDaemonStart renders a start time, or a dash when the platform could not
// read one — never "now", which would read as a daemon that just started.
func formatDaemonStart(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Format(time.RFC3339)
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "-"
}

func dashIfZero(n int) string {
	if n <= 0 {
		return "-"
	}
	return strconv.Itoa(n)
}
