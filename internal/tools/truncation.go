package tools

// Where a truncation notice belongs, and why it is not the end.
//
// A truncation notice is not a footnote. It changes how everything ABOVE it
// must be read: a list that was cut is not a list of "the results", it is a
// list of *some* results, and a reader who does not know that will draw a
// conclusion the data does not support. Printing it only after the payload puts
// the correction after the conclusion.
//
// That is not hypothetical. `topology_affected` announced its cut on the last
// line of a 119 KB response; the 1,037 rows above read as complete, and the one
// test that actually covered the change had been cut. The marker was present and
// correct the whole time, and it did not help, because nothing reaches the last
// line of a wall of results — an LLM least of all, since a long tool result is
// exactly what gets skimmed or elided.
//
// So every truncated payload leads with the notice and keeps its trailing one.
// The duplication is deliberate: the top line is what changes the reading, the
// bottom line is what a reader who did make it to the end sees at the point the
// data stops.
//
// Each notice must say three things, because a notice that only says "truncated"
// leaves the reader unable to act: what was cut, how much is missing where that
// is known, and the specific parameter that returns the rest.

// truncationMarker prefixes both the leading and trailing notice. The symbol is
// carried so a notice is greppable and visually distinct from content in a
// payload that is otherwise plain text.
const truncationMarker = "⚠ TRUNCATED"

// withTruncationBanner puts the notice ahead of the payload it qualifies.
// detail should complete the sentence "TRUNCATED — ..." and name the remedy.
// An empty detail returns body untouched, so callers can pass through
// unconditionally rather than branching at every return.
func withTruncationBanner(body, detail string) string {
	if detail == "" {
		return body
	}
	return truncationMarker + " — " + detail + "\n\n" + body
}
