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
// account, it is the one holding the launch authority, and — since PLAN-529 — the
// socket path it published still holds the socket it bound. The three questions
// come from ONE census, so the rows cannot disagree about what was seen.
func checkDaemonCensus() []checkResult {
	report := daemons.Census(daemonCensusOptions())
	rows := daemonCensusResult(report.Verdict())
	return append(rows, daemonSocketResults(daemonSocketStates(report))...)
}

// daemonSocketResults is the socket half of the daemon section, and its pure half
// so the state-to-row mapping is testable without a filesystem.
//
// A path a live daemon published that no longer holds its socket is a FAILURE:
// the daemon is running and no client can reach it, which is the state 64 leaked
// daemons reached and the reason this row exists. Anything weaker — a socket file
// nothing serves, a question the census could not answer — is a warning, because
// accusing a daemon of losing a path nobody could read would be a guess.
func daemonSocketResults(states []socketPathState) []checkResult {
	var rows []checkResult
	for _, s := range states {
		switch {
		case s.fault() != "":
			rows = append(rows, checkResult{
				name:   "socket",
				ok:     false,
				detail: s.Path + ": " + s.fault(),
				fix:    "if the file at that path is not yours, move it aside and run `plumb restart`; a daemon deliberately never removes a file it did not create",
			})
		case s.note() != "":
			rows = append(rows, checkResult{name: "socket", ok: true, warn: true, detail: s.Path + ": " + s.note()})
		}
	}
	return rows
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
