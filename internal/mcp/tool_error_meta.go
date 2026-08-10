package mcp

// tool_error_meta.go — the wire rendering of a classified tool failure.
//
// The envelope is a SIDE-CAR. A failed tools/call still returns the same
// `content` text and the same `isError: true` it always did; a rejected
// request still returns the same JSON-RPC `code` and `message`. All this adds
// is a machine-readable description alongside, and only when plumb actually has
// one — an unclassified failure emits nothing at all, so a client can read the
// key's absence as "no structured claim" instead of parsing a hollow object.
//
// envelope is the single renderer of that shape. Both entry points go through
// it so the key set cannot drift between the tool-result path and the
// pre-dispatch rejection path.
//
// classifyOnce is the other half of the same discipline, one level up: the
// classification itself is derived once per call and shared with the telemetry
// observer, so what the client is told and what plumb records cannot diverge.

import (
	"maps"

	"github.com/plumbkit/plumb/internal/toolerror"
)

// classifyOnce derives THE classification of a failed tools/call. It is called
// exactly once per call, and its result is handed to every consumer: the `_meta`
// envelope on the wire and the OnAfterTool observer that records the failure as
// telemetry. Classifying separately at each consumer would leave two derivations
// free to disagree about the same error — a client told `dirty_file` while the
// recorded row said something else is worse than either alone.
//
// Returns nil when err is nil or carries no classification. Both consumers read
// nil as "plumb makes no structured claim about this failure": the envelope is
// omitted, and the telemetry columns stay blank rather than being filled with a
// guess.
//
// op names the operation the failure belongs to. It is applied only when the
// error does not already carry one: the dispatch boundary is the place that
// reliably knows the resolved tool name, but a call site that knew better must
// not have its answer overwritten by the generic one.
func classifyOnce(err error, op string) *toolerror.Error {
	te, ok := toolerror.Classify(err)
	if !ok {
		return nil
	}
	if te.Op == "" && op != "" {
		te = te.WithOp(op)
	}
	return te
}

// toolErrorMeta renders the `_meta` map for a failed tool result from the
// classification classifyOnce derived, or nil when there is none (which json
// omitempty then drops entirely).
func toolErrorMeta(te *toolerror.Error) map[string]any {
	if te == nil {
		return nil
	}
	return map[string]any{MetaToolErrorKey: envelope(te.Kind, te.Op, te.Remediation, te.Details)}
}

// invalidCallEnvelope renders the envelope for a tools/call plumb refused
// before any tool ran — malformed params, an unknown tool name. These two never
// reach the tool-result path, so they are the only failures whose envelope
// travels as a JSON-RPC error rather than as `_meta` on a result.
//
// It names the classification directly instead of wrapping an error: the
// rejection text is already on the wire as `error.message`, and the envelope
// never renders a cause, so routing one through toolerror.Wrap would exist only
// to satisfy an argument nothing reads.
func invalidCallEnvelope(op string) map[string]any {
	return envelope(toolerror.KindInvalidArguments, op,
		toolerror.Remediation{Class: toolerror.ClassFixArguments}, nil)
}

// envelope is the one place the wire shape is written. retryable is derived
// from the remediation class, exactly as toolerror.Error derives it, so the two
// paths can never disagree about the same remedy.
func envelope(kind toolerror.Kind, op string, r toolerror.Remediation, details map[string]string) map[string]any {
	r = r.WithDefaults()
	env := map[string]any{
		"kind":        string(kind),
		"retryable":   r.Class.Retryable(),
		"remediation": remediationWire(r),
	}
	if op != "" {
		env["operation"] = op
	}
	if len(details) > 0 {
		env["details"] = maps.Clone(details)
	}
	return env
}

func remediationWire(r toolerror.Remediation) map[string]any {
	out := map[string]any{"class": string(r.Class)}
	if r.Tool != "" {
		out["tool"] = r.Tool
	}
	if r.Reason != "" {
		out["reason"] = r.Reason
	}
	return out
}
