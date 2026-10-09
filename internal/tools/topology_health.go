package tools

import (
	"fmt"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/textfmt"
	"github.com/plumbkit/plumb/internal/topology"
)

// staleIndexMarker prefixes the notice a topology-backed answer carries while
// the indexer is failing. Like truncationMarker it carries a symbol so the
// notice is greppable and visually distinct from content.
const staleIndexMarker = "⚠ STALE INDEX"

// maxHealthErrorBytes bounds the indexer error quoted in the notice. SQLite
// errors are short, but a wrapped chain is not bounded by anything here. The
// quote is also collapsed onto one line so the notice stays a single line.
const maxHealthErrorBytes = 200

// withIndexHealth puts a stale-index notice ahead of a topology-backed answer
// when the indexer's most recent cycle failed, and returns body untouched
// otherwise.
//
// An index whose cycles fail keeps answering from its last good snapshot, and
// nothing in that answer says so: the workspace index sat in error for most of
// a day (PLAN-467) while explore, impact and affected answered as if current,
// and only topology_status mentioned it. A "no callers" or "not found" from
// that snapshot reads as fact. The notice leads rather than trails for the
// reason truncation.go gives: nothing reaches the last line of a long result.
func withIndexHealth(store *topology.Store, body string) string {
	note := indexHealthNote(store.Health(), time.Now())
	if note == "" {
		return body
	}
	return note + "\n\n" + body
}

// withIndexHealthErr attaches the same notice to a topology-backed tool's
// error. "Symbol not found in the index" from a failing index is the absence
// answer that most needs it.
func withIndexHealthErr(store *topology.Store, err error) error {
	note := indexHealthNote(store.Health(), time.Now())
	if note == "" {
		return err
	}
	return fmt.Errorf("%w\n\n%s", err, note)
}

// indexHealthNote renders the stale-index notice for h, or "" when the most
// recent indexing cycle succeeded.
func indexHealthNote(h topology.Health, now time.Time) string {
	if !h.Failing {
		return ""
	}
	since := "no indexing cycle has succeeded since the index was opened"
	if !h.LastSync.IsZero() {
		since = fmt.Sprintf("last good sync %s, %s ago",
			h.LastSync.Format(time.RFC3339), now.Sub(h.LastSync).Round(time.Minute))
	}
	return fmt.Sprintf("%s — the topology indexer is failing (%s; last error: %s). "+
		"This answer may be missing recent changes, so an absence here is not evidence of absence. "+
		"The indexer retries with backoff; topology_status shows its state.",
		staleIndexMarker, since, textfmt.ClampBytes(strings.Join(strings.Fields(h.LastError), " "), maxHealthErrorBytes))
}
