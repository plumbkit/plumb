package tools

// post_write_diag_delta.go — the STRUCTURED post-write diagnostics delta
// (PLAN-362 PR2). PR1 made every non-empty post-write block carry one of three
// fixed labels; prose alone still forces an agent to parse English to answer
// "did my edit break anything?". This file renders the same computed result as
// one machine-parseable line:
//
//	diagnostics delta: {"fresh":true,"scopes":{...},"new_errors":[...],"resolved":[...],"pre_existing":0}
//
// It is emitted only when the caller asked for a confirmed pass
// (await_diagnostics / fail_on_new_errors), so the default write path is
// byte-for-byte unchanged.
//
// FRESHNESS IS PER SCOPE. The top-level "fresh" is the EDITED FILE's freshness
// — the only scope fail_on_new_errors acts on. "scopes" reports each scope
// separately, because they are confirmed by different mechanisms and can
// disagree: the edited file is confirmed by a publish/pull that followed this
// write, while the cross-file sweep is off by default, bounded by a settle
// grace, and non-exhaustive in pull mode.

import (
	"encoding/json"
	"strings"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/paths"
)

// diagDeltaPrefix is the fixed prefix of the structured delta line. Callers
// match on it, then json.Unmarshal the remainder — no prose parsing.
const diagDeltaPrefix = "diagnostics delta: "

// maxDeltaEntries caps how many diagnostics each list carries, so a write that
// breaks a hundred things does not return a hundred-entry payload. What is cut
// is counted in omitted_new_errors / omitted_resolved rather than dropped
// silently.
const maxDeltaEntries = 10

// Scope states. A scope is "fresh" only when the analysis it reports was
// CONFIRMED to have happened after this write; every other value names the
// specific reason it was not, because "not fresh" for a disabled window and
// "not fresh" for a failed pull call for different responses.
const (
	// diagScopeFresh — re-analysis after this write was confirmed.
	diagScopeFresh = "fresh"
	// diagScopeUnconfirmed — no confirmation arrived: the wait timed out, the
	// post-write window is disabled, or the language server was never told the
	// file changed. Any diagnostics reported may predate the write.
	diagScopeUnconfirmed = "unconfirmed"
	// diagScopeUnverified — the check itself failed (a pull-mode request
	// errored), so there is no answer of any age.
	diagScopeUnverified = "unverified"
	// diagScopeNoSource — no diagnostics source is wired for this file at all
	// (no language server for its type, or none attached). Silence here means
	// "nothing analysed", NOT "clean".
	diagScopeNoSource = "no_source"
	// diagScopeNotChecked — the cross-file sweep did not run (disabled, or the
	// edited file itself was never confirmed).
	diagScopeNotChecked = "not_checked"
	// diagScopeIncomplete — the cross-file sweep ran but cannot claim to have
	// seen every dependent file.
	diagScopeIncomplete = "incomplete"
)

// diagEntry is one diagnostic in the structured delta.
type diagEntry struct {
	File     string `json:"file"`
	Line     int    `json:"line"` // 1-based, matching editor and diagnostics() output
	Severity string `json:"severity"`
	Message  string `json:"message"`
	Code     string `json:"code,omitempty"`
}

// diagScopes reports freshness per scope. See the file comment: the two scopes
// are confirmed by different mechanisms and routinely disagree.
type diagScopes struct {
	EditedFile string `json:"edited_file"`
	CrossFile  string `json:"cross_file"`
}

// diagDelta is the structured post-write result.
//
// Fresh mirrors Scopes.EditedFile == diagScopeFresh and is the ONLY freshness
// fail_on_new_errors acts on: a rollback demands a confirmed answer, and no
// other scope provides one.
//
// NewErrors covers the edited file. CrossFileNewErrors covers other files the
// sweep attributed to this write; they are reported but never roll a write
// back — see failOnNewErrorsRollback for why.
type diagDelta struct {
	Fresh              bool        `json:"fresh"`
	Scopes             diagScopes  `json:"scopes"`
	NewErrors          []diagEntry `json:"new_errors"`
	CrossFileNewErrors []diagEntry `json:"cross_file_new_errors,omitempty"`
	Resolved           []diagEntry `json:"resolved"`
	PreExisting        int         `json:"pre_existing"`
	OmittedNewErrors   int         `json:"omitted_new_errors,omitempty"`
	OmittedResolved    int         `json:"omitted_resolved,omitempty"`
}

// unconfirmedDelta is the delta for every path that could not confirm
// re-analysis of the edited file. It never carries findings: reporting a
// pre-write snapshot's diagnostics as a "delta" is exactly the false confidence
// PLAN-362 exists to remove.
func unconfirmedDelta(editedScope string) diagDelta {
	return diagDelta{
		Fresh:     false,
		Scopes:    diagScopes{EditedFile: editedScope, CrossFile: diagScopeNotChecked},
		NewErrors: []diagEntry{},
		Resolved:  []diagEntry{},
	}
}

// line renders the delta as its own fixed-prefix line, ready to append to a
// labelled block. Empty lists render as [] rather than null so a consumer can
// index without a nil check.
func (d diagDelta) line() string {
	if d.NewErrors == nil {
		d.NewErrors = []diagEntry{}
	}
	if d.Resolved == nil {
		d.Resolved = []diagEntry{}
	}
	b, err := json.Marshal(d)
	if err != nil {
		// Marshalling a struct of strings, ints and bools cannot fail; if it
		// somehow does, say so rather than emitting a half-line that looks
		// parseable.
		return "\n" + diagDeltaPrefix + `{"fresh":false,"error":"delta could not be rendered"}`
	}
	return "\n" + diagDeltaPrefix + string(b)
}

// hasNewErrors reports whether the edited file gained errors this write is
// answerable for — the single predicate fail_on_new_errors branches on. It is
// deliberately narrow: only a CONFIRMED-fresh result counts, so an unconfirmed
// or failed check never rolls a write back.
func (d diagDelta) hasNewErrors() bool {
	return d.Fresh && len(d.NewErrors) > 0
}

// newDiagEntries converts diagnostics for one file into delta entries, capped at
// maxDeltaEntries. The second return is how many were omitted.
func newDiagEntries(uri string, ds []protocol.Diagnostic) ([]diagEntry, int) {
	out := make([]diagEntry, 0, min(len(ds), maxDeltaEntries))
	file := diagDeltaFile(uri)
	for i, d := range ds {
		if i >= maxDeltaEntries {
			return out, len(ds) - maxDeltaEntries
		}
		id := identityOf(d)
		out = append(out, diagEntry{
			File:     file,
			Line:     int(d.Range.Start.Line) + 1,
			Severity: deltaSeverity(d.Severity),
			Message:  d.Message,
			Code:     id.code,
		})
	}
	return out, 0
}

// crossFileEntries converts the cross-file sweep's per-file breaks into delta
// entries. A break carries one representative NEW error message; that is what
// the prose block shows and all the sweep knows.
func crossFileEntries(breaks []crossFileBreak) []diagEntry {
	if len(breaks) == 0 {
		return nil
	}
	out := make([]diagEntry, 0, min(len(breaks), maxDeltaEntries))
	for i, b := range breaks {
		if i >= maxDeltaEntries {
			break
		}
		out = append(out, diagEntry{
			File:     diagDeltaFile(b.uri),
			Line:     b.exampleLine,
			Severity: "error",
			Message:  b.exampleMsg,
		})
	}
	return out
}

// diagDeltaFile renders a URI as a plain filesystem path — the form every other
// plumb response uses, and the form a caller can feed straight back to read_file.
func diagDeltaFile(uri string) string {
	return paths.URIToPath(strings.TrimSpace(uri))
}

// deltaSeverity is the JSON spelling of a severity. Distinct from
// severityLabel, whose fixed-width upper-case padding is for a human reading the
// diagnostics tool's column output; a JSON consumer wants a bare lower-case
// token it can compare.
func deltaSeverity(s protocol.DiagnosticSeverity) string {
	switch s {
	case protocol.SevError:
		return "error"
	case protocol.SevWarning:
		return "warning"
	case protocol.SevInformation:
		return "information"
	case protocol.SevHint:
		return "hint"
	default:
		return "unknown"
	}
}

// resolvedDiagnostics returns the pre-write diagnostics this write appears to
// have FIXED: those whose (message, code) identity is absent from the post-write
// set entirely.
//
// Deliberately stricter than the carried-over matching used for the new-error
// side. There, an unclaimed pre diagnostic inside the touched range is treated
// as possibly-fixed so a genuinely new one is never hidden — the safe direction
// for BREAKAGE. For "resolved" the safe direction is the opposite: claiming an
// edit fixed something it did not is the harmful error, so a pre diagnostic
// whose identity still exists anywhere in the post set is not claimed at all.
func resolvedDiagnostics(pre, post []protocol.Diagnostic) []protocol.Diagnostic {
	if len(pre) == 0 {
		return nil
	}
	postIDs := make(map[diagIdentity]bool, len(post))
	for i := range post {
		if isRenderedSeverity(post[i].Severity) {
			postIDs[identityOf(post[i])] = true
		}
	}
	var out []protocol.Diagnostic
	for i := range pre {
		if !isRenderedSeverity(pre[i].Severity) {
			continue
		}
		if !postIDs[identityOf(pre[i])] {
			out = append(out, pre[i])
		}
	}
	return out
}
