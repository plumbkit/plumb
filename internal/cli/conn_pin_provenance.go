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
func pinViaLabel(origin sessionstate.PinSource, t pinTrigger) string {
	if t == pinTriggerRestore {
		return "restore:" + pinSourceLabel(origin)
	}
	return pinSourceLabel(origin)
}

// recordPinProvenance stamps the provenance fields on the view being built.
// Call ONLY from inside a mutate closure, beside the v.acquiredRoot write.
func recordPinProvenance(v *sessionView, origin sessionstate.PinSource, t pinTrigger, prevRoot string) {
	v.pinVia = pinViaLabel(origin, t)
	v.pinAt = time.Now()
	v.pinPrev = prevRoot
}

// pinProvenance reports the connection's current pin provenance; the zero
// value while unattached, which renders nothing.
func (s *connSession) pinProvenance() tools.PinProvenance {
	v := s.view()
	if v.pinVia == "" {
		return tools.PinProvenance{}
	}
	return tools.PinProvenance{Source: v.pinVia, At: v.pinAt, Previous: v.pinPrev}
}

// boundedForLog caps a slice for a log field, appending a "+N more" sentinel
// rather than silently truncating. Never aliases or mutates vals.
func boundedForLog(vals []string, limit int) []string {
	if limit <= 0 || len(vals) <= limit {
		return vals
	}
	out := make([]string, 0, limit+1)
	out = append(out, vals[:limit]...)
	return append(out, fmt.Sprintf("+%d more", len(vals)-limit))
}
