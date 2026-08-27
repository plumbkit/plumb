package cli

// conn_pin_provenance.go — pin-drift observability for issue #182: how, when,
// and from where this connection's workspace pin was last set.

import (
	"fmt"
	"time"

	"github.com/plumbkit/plumb/internal/sessionstate"
	"github.com/plumbkit/plumb/internal/tools"
)

// pinTrigger distinguishes a pin set by a live call from one replayed on
// reconnect. The source alone cannot: a restored session_start pin and a fresh
// one record the same origin, yet only the second is the caller acting now.
type pinTrigger string

const (
	pinTriggerLive    pinTrigger = "live"    // a live session_start call or roots notification
	pinTriggerRestore pinTrigger = "restore" // OnInit / rehydrate replay of a persisted or proxy-replayed pin
)

// pinSourceLabel renders a pin origin for logs and provenance, naming the
// unknown origin explicitly rather than leaving an empty field.
func pinSourceLabel(origin sessionstate.PinSource) string {
	if origin == sessionstate.PinSourceUnknown {
		return "unknown"
	}
	return string(origin)
}

// pinViaLabel is the provenance label for a pin: the origin, prefixed with
// "restore:" when it was replayed on reconnect rather than set by a live call.
// An unknown origin returns "" — an incidental auto-attach has no provenance
// worth telling an agent about, and "via unknown" in a boundary error is noise
// that invites speculation. The log's source field keeps the explicit
// "unknown" label via pinSourceLabel; only the agent-facing provenance is
// suppressed.
func pinViaLabel(origin sessionstate.PinSource, t pinTrigger) string {
	if origin == sessionstate.PinSourceUnknown {
		return ""
	}
	if t == pinTriggerRestore {
		return "restore:" + pinSourceLabel(origin)
	}
	return pinSourceLabel(origin)
}

// recordPinProvenance stamps the provenance fields on the view being built.
// Call ONLY from inside a mutate closure, beside the v.acquiredRoot write.
//
// forced is the force flag of the call that set this pin, not a judgement about
// it: force on a first attach displaced nothing, and the renderers ask for
// prevRoot alongside before saying anyone was displaced.
func recordPinProvenance(v *sessionView, origin sessionstate.PinSource, t pinTrigger, prevRoot string, forced bool) {
	v.pinVia = pinViaLabel(origin, t)
	v.pinAt = time.Now()
	v.pinPrev = prevRoot
	v.pinOrigin = origin
	v.pinForced = forced
	// Every pin write runs through here, so clearing by default is what keeps
	// the unauthenticated-replay mark describing the CURRENT pin: rung 1 of the
	// attach ladder re-sets it immediately after its attach, and any later pin —
	// a deliberate session_start above all — drops it (issue #318).
	v.pinUnverifiedReplay = false
}

// pinExplicitlyHeld reports whether the connection's current pin was set by an
// explicit session_start call (live or restored on reconnect) — a workspace
// argument, or a language override applied to the current root: both are
// deliberate acts on this connection, so either records session_start origin.
// Only such a pin is sticky (issue #182 — a multiplexed peer's session_start
// or roots notification must not silently steal the pin a caller deliberately
// chose); incidental auto-attaches (unknown origin) and client-roots attaches
// are not, so the first explicit pin can always land. This snapshot read is
// the onRootsChanged fast path; the AUTHORITATIVE guard runs on the view under
// mutation, inside attachOrRepinTo, where it cannot race a concurrent re-pin.
func (s *connSession) pinExplicitlyHeld() bool {
	v := s.view()
	return v.acquiredRoot != "" && v.pinOrigin == sessionstate.PinSourceSessionStart
}

// pinProvenanceOf reads a view's pin provenance; the zero value while
// unattached, which renders nothing. Usable inside a mutate closure (on the
// view under mutation) as well as on a snapshot.
//
// It cannot report Contested: that fact lives on the connection's displacement
// history, not on the view, and this function takes only a view (the mutate
// lane needs it that way). Callers that have the connSession use
// s.pinProvenance, which fills it in.
func pinProvenanceOf(v *sessionView) tools.PinProvenance {
	if v.pinVia == "" {
		return tools.PinProvenance{}
	}
	return tools.PinProvenance{Source: v.pinVia, At: v.pinAt, Previous: v.pinPrev, Forced: v.pinForced}
}

// pinProvenance reports the connection's current pin provenance from the
// latest snapshot, including whether the connection's pin is contested.
func (s *connSession) pinProvenance() tools.PinProvenance {
	v := s.view()
	p := pinProvenanceOf(&v)
	if p.Source != "" {
		p.Contested = s.pinContested()
	}
	return p
}

// boundedForLog caps a slice for a log field, appending a "+N more" sentinel
// rather than silently truncating. Never mutates vals; the within-limit branch
// returns the input slice itself, so callers must not modify the result.
func boundedForLog(vals []string, limit int) []string {
	if limit <= 0 || len(vals) <= limit {
		return vals
	}
	out := make([]string, 0, limit+1)
	out = append(out, vals[:limit]...)
	return append(out, fmt.Sprintf("+%d more", len(vals)-limit))
}
