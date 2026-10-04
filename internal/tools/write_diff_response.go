package tools

// write_diff_response.go — the policy for whether a write response shows the
// content it changed, and what it says when it does not. diff.go owns the
// renderer mechanics (the Myers script, hunking, the maxDiffLines cap); this
// file owns the decisions around them, so every write site reaches the same
// verdict from the same place.
//
// The gate is the existing [edits].show_write_diff knob, extended here to every
// content-changing write rather than the subset that used to render one. Two
// limits sit on top of it:
//
//   - A sensitive path shows NO content. The globs are [history]
//     sensitive_globs — the store's own withheld:sensitive decision, reached
//     through the same matcher, so the transcript and history.db never disagree
//     about which content may be seen. history.db is local; the transcript is
//     not.
//   - A side this call did not read reports the withholding rather than
//     rendering a diff against an empty string, which would read as "the file
//     was empty".

import (
	"context"
	"fmt"
	"strings"

	"github.com/plumbkit/plumb/internal/history"
	"github.com/plumbkit/plumb/internal/textdiff"
)

// maxResponseDiffBytes bounds the content a response diff is computed from,
// for the sites that do not already hold a bounded baseline (delete_file,
// copy_file, rename_file). It matches write_file's baseline cap and
// find_replace's. The bound is applied where the content arrives — bytesSide
// and sideOf, both fed by an unbounded read — and NOT inside responseDiff, so
// the sites that already render a diff from bounded content (write_file,
// edit_file, undo_edit's restore) keep the behaviour they had.
const maxResponseDiffBytes = 200 * 1024

// maxResponseDiffFiles bounds how many per-file diffs one response shows — the
// same shape find_replace and rename_symbol use, so no single response carries
// an unbounded number of diffs.
const maxResponseDiffFiles = 20

// Response markers. Each names what was withheld, so a reader can tell a
// deliberate withholding from a tool that simply had nothing to show.
const (
	// withheldSensitiveNote names the config key, because changing it is the
	// remedy: a path matching [history] sensitive_globs never shows content.
	withheldSensitiveNote = "… (diff withheld: sensitive path — see [history] sensitive_globs)"
	withheldBinaryNote    = "… (diff withheld: binary content)"
	withheldTooLargeNote  = "… (diff withheld: file too large for a response diff)"
	// diffTruncatedNote points at `plumb history`, not file_diff: the diff can
	// be of a file that no longer exists (a delete), which file_diff cannot show.
	diffTruncatedNote = "… (diff truncated; the full diff is in plumb history)"
)

// relayDiffNote asks the agent to put the change in front of the user. It is
// emitted at most ONCE per response — a twenty-file batch needs one instruction,
// not twenty — and only when a diff was actually shown.
//
// "this diff", not "this change": the line also rides on a dry-run preview,
// where nothing changed yet, and on an undo, where the change is a reversal.
//
// One line by design: it rides on every write response that shows a diff, so its
// own tokens are paid per write.
const relayDiffNote = "> Show this diff to the user in your reply (one diff block per file)."

// sideState says what a write site knows about one side of a change.
type sideState uint8

const (
	// sidePresent: the tool has this side's content.
	sidePresent sideState = iota
	// sideAbsent: the side did not exist — a create's before, a delete's after.
	sideAbsent
	// sideUnknown: the side exists but this call did not read it (over the read
	// cap, or beyond what the recorder carried). Reported, never rendered as
	// empty.
	sideUnknown
)

// diffContent is one side of a response diff.
type diffContent struct {
	Content string
	State   sideState
}

// presentSide is a side whose content this call holds.
func presentSide(s string) diffContent { return diffContent{Content: s, State: sidePresent} }

// absentSide is a side that did not exist.
func absentSide() diffContent { return diffContent{State: sideAbsent} }

// unknownSide is an existing side this call did not read.
func unknownSide() diffContent { return diffContent{State: sideUnknown} }

// sideOf describes a recorded history.Side for the response. A side the recorder
// carried no bytes for (over [history] max_content_bytes), or carried more of
// than a response will render, is unknown rather than empty — the difference
// between "nothing was there" and "we did not look".
func sideOf(s history.Side) diffContent {
	switch {
	case !s.Exists:
		return absentSide()
	case s.Content == nil || len(s.Content) > maxResponseDiffBytes:
		return unknownSide()
	default:
		return presentSide(string(s.Content))
	}
}

// responseDiff renders the diff section for one changed path, or "" when the
// response shows nothing for it. The order of the checks is the policy: the
// knob first (nothing is rendered at all when it is off), then the reasons
// content must not be shown.
func (d WriteDeps) responseDiff(ctx context.Context, path string, before, after diffContent) string {
	return d.responseDiffAcross(ctx, []string{path}, path, before, after)
}

// responseDiffAcross is responseDiff for a change whose content came from more
// than one path — a copy (source → destination), or a rename whose destination
// the move destroyed. The sensitive decision is taken over EVERY path involved,
// not just the one the diff is headed with: copying .env to notes.txt renders
// the secret under a name matching no glob, and the question is where the
// content came from. (The store reaches the same conclusion through its own
// From-side check.)
func (d WriteDeps) responseDiffAcross(ctx context.Context, paths []string, headerPath string, before, after diffContent) string {
	if !d.showWriteDiff() {
		return ""
	}
	for _, p := range paths {
		if m := d.withheldForResponse(ctx, p); m != "" {
			return m
		}
	}
	if before.State == sideUnknown || after.State == sideUnknown {
		return withheldTooLargeNote
	}
	if isBinaryContent(before.Content) || isBinaryContent(after.Content) {
		return withheldBinaryNote
	}
	return unifiedDiff(headerPath, before.Content, after.Content)
}

// ResponseDiffSuffix renders everything a write response appends for one change:
// the relay instruction (when a diff is actually shown, at most once) followed by
// the diff section, each on its own line. It returns "" when there is nothing to
// show, so a caller can concatenate it unconditionally.
//
// Exported for the two kinds of write site that have no response rendering of
// their own: the `.plumb/` writers (write_memory, delete_memory, git_init), which
// carry their own history hook rather than a tool's WriteDeps, and agent_config,
// which lives in the connection layer. Both already hold the before/after sides
// they recorded, so this is one call over bytes nobody reads twice.
func ResponseDiffSuffix(ctx context.Context, deps WriteDeps, path string, before, after history.Side) string {
	section := deps.responseDiff(ctx, path, sideOf(before), sideOf(after))
	if section == "" {
		return ""
	}
	var sb strings.Builder
	if relay := deps.relayNoteFor(section); relay != "" {
		sb.WriteString("\n")
		sb.WriteString(relay)
	}
	sb.WriteString("\n")
	sb.WriteString(section)
	return sb.String()
}

// gatedDiff applies the withholding policy to a diff a site has ALREADY
// rendered from content it held: write_file, edit_file, find_replace, the
// symbol edits, transaction_apply and move_symbol reach the same verdict as
// responseDiff by passing their rendered diff through here, instead of each
// re-deciding what may be shown.
//
// It does NOT re-check show_write_diff: every caller already resolves that
// toggle its own way (the symbol edits carry their own resolver, wired from the
// same config), and second-guessing it here would silently suppress a diff a
// caller's own knob had enabled. An empty diff stays empty.
func (d WriteDeps) gatedDiff(ctx context.Context, path, diff string) string {
	if diff == "" {
		return ""
	}
	if m := d.withheldForResponse(ctx, path); m != "" {
		return m
	}
	return diff
}

// withheldForResponse returns the marker to render INSTEAD of a diff when this
// path's content must not be shown, or "" to render the diff. A nil resolver
// withholds nothing, which is what a bare WriteDeps{} wants.
func (d WriteDeps) withheldForResponse(ctx context.Context, path string) string {
	if d.SensitivePathFn == nil {
		return ""
	}
	if d.SensitivePathFn(ctx, path) {
		return withheldSensitiveNote
	}
	return ""
}

// bytesSide maps a byte slice to a side: nil is absent, empty-but-non-nil is
// present and empty, and content past maxResponseDiffBytes is unknown. The size
// check is here rather than only in responseDiff so a huge side is never copied
// into a string just to be told it cannot be shown.
func bytesSide(b []byte) diffContent {
	switch {
	case b == nil:
		return absentSide()
	case len(b) > maxResponseDiffBytes:
		return unknownSide()
	default:
		return presentSide(string(b))
	}
}

// revertedPath is one path an automatic revert put back: after is what plumb's
// write had left on disk, before is what the revert restored
type revertedPath struct{ path, before, after string }

// revertNote renders the summary of an automatic revert for a failed call's
// error text: one counts-only line per path, then where the diffs are.
//
// Deliberately NOT a diff. An error's first job is the remedy, and every
// reverted path's diff is already in the history store (a revert is recorded as
// history.OpRevert), so the message gains the FACT of the revert and its size
// without paying for the content twice in a response whose reader is working out
// what to do next.
func revertNote(reverted []revertedPath) string {
	if len(reverted) == 0 {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "\nreverted %d path(s):", len(reverted))
	for _, r := range reverted {
		added, removed := textdiff.Counts(textdiff.ComputeExact(r.after, r.before))
		fmt.Fprintf(&sb, "\n  %s (+%d -%d)", r.path, added, removed)
	}
	sb.WriteString("\nsee plumb history for the diffs")
	return sb.String()
}

// withRevertNote appends the revert summary to err, keeping err's own words
// first: the remedy a caller needs must not be buried under the summary.
func withRevertNote(err error, reverted []revertedPath) error {
	if note := revertNote(reverted); note != "" {
		return fmt.Errorf("%w%s", err, note)
	}
	return err
}

// diffShown reports whether a response section renders actual content, as
// opposed to a withholding marker or nothing at all. Only renderUnifiedDiff
// emits the "--- a/" header, so the header is the signal — and the points where
// it matters (whether to ask the agent to relay, whether to count a file as
// shown) are exactly the ones that must not fire on a marker.
func diffShown(section string) bool { return strings.HasPrefix(section, "--- a/") }

// relayNoteFor returns the relay instruction when at least one section actually
// rendered content. A write with nothing to show — a rename that destroyed
// nothing, a withheld sensitive path — does not ask the agent to show anything.
func (d WriteDeps) relayNoteFor(sections ...string) string {
	for _, s := range sections {
		if diffShown(s) {
			return d.relayNote()
		}
	}
	return ""
}

// relayNote is the instruction to show the change to the user, or "" when the
// relay knob is off. Callers append it once per response, and only when a diff
// was shown.
func (d WriteDeps) relayNote() string {
	if !d.relayDiff() {
		return ""
	}
	return relayDiffNote
}

// relayDiff resolves the relay toggle for this call, mirroring showWriteDiff:
// a nil resolver falls back to the struct field, so a bare WriteDeps{} stays
// quiet.
func (d WriteDeps) relayDiff() bool {
	if d.RelayDiffFn != nil {
		return d.RelayDiffFn()
	}
	return d.RelayDiff
}

// diffSections returns the per-file diff sections of a multi-file response: the
// empty ones dropped FIRST, then capped at maxResponseDiffFiles with a "+N more"
// line for the remainder.
//
// Order matters. Filtering after the cap let an empty section consume one of the
// twenty slots, and — because the cap and the relay decision were taken over
// different lists — a response could ask the agent to show a diff it had just
// dropped. Callers take BOTH the rendered sections and the relay decision from
// this return value, so the two cannot disagree.
func diffSections(sections []string) []string {
	nonEmpty := make([]string, 0, len(sections))
	for _, s := range sections {
		if s != "" {
			nonEmpty = append(nonEmpty, s)
		}
	}
	shown, extra := nonEmpty, 0
	if len(shown) > maxResponseDiffFiles {
		shown, extra = shown[:maxResponseDiffFiles], len(shown)-maxResponseDiffFiles
	}
	out := make([]string, 0, len(shown)+1)
	out = append(out, shown...)
	if extra > 0 {
		out = append(out, fmt.Sprintf("… (+%d more file(s) changed — see plumb history)", extra))
	}
	return out
}

// appendSections appends newline-separated non-empty sections to sb.
func appendSections(sb *strings.Builder, sections ...string) {
	for _, s := range sections {
		if s == "" {
			continue
		}
		sb.WriteString("\n")
		sb.WriteString(s)
	}
}

// isBinaryContent reports whether s looks binary by the same NUL sniff the
// search tools use (binarySniffBytes), so "binary" means one thing in this
// codebase.
func isBinaryContent(s string) bool {
	if s == "" {
		return false
	}
	if len(s) > binarySniffBytes {
		s = s[:binarySniffBytes]
	}
	return strings.IndexByte(s, 0) >= 0
}
