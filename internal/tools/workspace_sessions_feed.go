package tools

import (
	"encoding/json"

	"github.com/plumbkit/plumb/internal/stats"
)

// workspace_sessions_feed.go — accuracy filtering for the recent-writes feed.
// The stats table records tool CALLS; the feed's readers act on tool WRITES.
// The gap between the two is closed here, in one place, for every consumer of
// RecentWritesByWorkspace (the workspace_sessions feed, the session_start peer
// digest, and the connection-side peer hint):
//
//   - a `git` row is classified with the git tool's own tier classifier
//     (classifyGit, subcommand + args from the recorded input), so `git status`
//     or `git log` never appears as a write while `git add`/`commit`/`push` do;
//   - a call to a tool whose dry_run parameter DEFAULTS to true is a preview —
//     it wrote nothing — unless the recorded input carries an explicit
//     `"dry_run": false`;
//   - a failed or refused call wrote nothing either, but IS still evidence a
//     peer is working in that file, so the feed keeps it and marks it
//     (feedOutcomeMarker) while the landed-writes consumers drop it.
//
// Rows recorded before this filtering existed degrade sensibly: `success` and
// `input_json` (subcommand, args, dry_run) have been recorded since the table's
// first schema, so old rows classify exactly like new ones; a row whose input
// does not parse is treated as not-a-write rather than crashing or being
// presented as one.

// dryRunDefaultTools names the feed tools whose dry_run parameter defaults to
// true. For these, a recorded call WITHOUT an explicit "dry_run": false was a
// preview, not a write. Keep in sync with each tool's InputSchema default.
var dryRunDefaultTools = map[string]bool{
	"find_replace":         true,
	"rename_symbol":        true,
	"replace_symbol_body":  true,
	"insert_before_symbol": true,
	"insert_after_symbol":  true,
	"safe_delete_symbol":   true,
	"move_symbol":          true,
}

// isRecordedWrite reports whether a recorded call was an operation that could
// have modified workspace files — regardless of whether it succeeded. It is
// the shared write/not-a-write judgement behind feedRecentWrites and
// LandedWrites.
func isRecordedWrite(w stats.RecentCall) bool {
	if w.Tool == "git" {
		return gitCallTier(w.InputJSON) >= tierWrite
	}
	if dryRunDefaultTools[w.Tool] {
		return dryRunDisabled(w.InputJSON)
	}
	return true
}

// gitCallTier classifies a recorded git call with the same classifier the git
// tool itself uses, so the feed can never disagree with the policy gate about
// which subcommands mutate. Args matter: `branch --list` reads while
// `branch <name>` writes. A row whose input does not parse or names no
// subcommand classifies as tierReject (nothing ran).
func gitCallTier(inputJSON string) gitTier {
	var a struct {
		Subcommand string   `json:"subcommand"`
		Args       []string `json:"args"`
	}
	if inputJSON == "" || json.Unmarshal([]byte(inputJSON), &a) != nil || a.Subcommand == "" {
		return tierReject
	}
	return classifyGit(a.Subcommand, a.Args)
}

// dryRunDisabled reports whether the recorded input carries an explicit
// "dry_run": false — the only shape under which a default-dry-run tool wrote
// anything. Absent, true, non-boolean, or unparseable all mean preview.
func dryRunDisabled(inputJSON string) bool {
	var a struct {
		DryRun *bool `json:"dry_run"`
	}
	if inputJSON == "" || json.Unmarshal([]byte(inputJSON), &a) != nil || a.DryRun == nil {
		return false
	}
	return !*a.DryRun
}

// feedFetchLimit is how many raw rows runSync requests for a feed of `limit`
// entries: filtering drops the rows that were not writes, so the fetch
// over-provisions (×5, floor 50) while staying bounded (cap 250) — the feed can
// honestly run short in a pathological all-reads window rather than scanning
// the whole table.
func feedFetchLimit(limit int) int {
	const factor, floor, ceiling = 5, 50, 250
	n := limit * factor
	if n < floor {
		n = floor
	}
	if n > ceiling {
		n = ceiling
	}
	return n
}

// feedRecentWrites reduces raw recent write-tool rows to the entries the
// workspace_sessions feed presents: read-only operations are dropped, failed
// calls are KEPT (they render with feedOutcomeMarker — a refused write is
// still evidence a peer is active in that file), and the result is truncated
// to limit AFTER filtering so dropped rows never consume feed slots. The
// caller over-fetches to compensate (see feedFetchLimit).
func feedRecentWrites(writes []stats.RecentCall, limit int) []stats.RecentCall {
	if limit <= 0 {
		return nil
	}
	var out []stats.RecentCall
	for _, w := range writes {
		if !isRecordedWrite(w) {
			continue
		}
		out = append(out, w)
		if len(out) == limit {
			break
		}
	}
	return out
}

// feedOutcomeMarker returns the suffix a feed entry carries when the recorded
// call failed or was refused: nothing landed on disk, so a reader must not
// treat the entry as a change to re-read — only as peer activity.
func feedOutcomeMarker(w stats.RecentCall) string {
	if w.Success {
		return ""
	}
	return "  [failed — no change applied]"
}

// LandedWrites returns only the calls that were actual write operations AND
// succeeded — the rows a consumer may honestly describe as "session X edited
// this file". The connection-side peer hint and the session_start peer digest
// use it; the workspace_sessions feed instead keeps failures and marks them.
func LandedWrites(writes []stats.RecentCall) []stats.RecentCall {
	var out []stats.RecentCall
	for _, w := range writes {
		if w.Success && isRecordedWrite(w) {
			out = append(out, w)
		}
	}
	return out
}
