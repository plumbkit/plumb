package cli

import (
	"github.com/plumbkit/plumb/internal/daemons"
)

// The daemon census as doctor reports it (PLAN-531).
//
// It is its own file because it answers a different question from the rest of
// doctor_checks.go: checkDaemon asks "can I reach a daemon?" and answers yes
// whenever one is reachable, while this asks "is it the only one?" — and the
// state that left 64 daemons running is exactly one that checkDaemon reports as
// perfectly healthy.

// checkDaemonCensus answers E1 in doctor: exactly one plumb daemon for this
// account, and it is the one holding the launch authority.
func checkDaemonCensus() []checkResult {
	report := daemons.Census(daemonCensusOptions())
	return daemonCensusResult(report.Verdict())
}

// daemonCensusResult is the pure half of checkDaemonCensus, so the verdict-to-row
// mapping is testable without a process table.
//
// "Cannot tell" is a FAILURE, not a warning: this row is the host's gate for
// "exactly one daemon", and a gate that passes when the census could not read the
// process table is worth nothing. A violation is likewise a failure. No daemon
// at all is a warning, because that is the normal state of an idle machine —
// `plumb serve` starts one on demand.
func daemonCensusResult(v daemons.Verdict) []checkResult {
	switch v.State {
	case daemons.StateOK:
		return []checkResult{{name: "census", ok: true, detail: v.Detail}}
	case daemons.StateNone:
		return []checkResult{{name: "census", ok: true, warn: true, detail: v.Detail}}
	default:
		return []checkResult{{name: "census", ok: false, detail: v.Detail, fix: v.Remedy}}
	}
}
