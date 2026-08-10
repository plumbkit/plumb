package mcp

// tool_error_meta.go — the wire rendering of a classified tool failure.
//
// The envelope is a SIDE-CAR. A failed tools/call still returns the same
// `content` text and the same `isError: true` it always did; a rejected
// request still returns the same JSON-RPC `code` and `message`. All this adds
// is a machine-readable description alongside, and only when plumb actually has
// one — an unclassified failure emits nothing at all, so a client can read the
// key's absence as "no structured claim" instead of parsing a hollow object.

import (
	"maps"

	"github.com/plumbkit/plumb/internal/toolerror"
)

// toolErrorMeta renders the `_meta` map for a failed tool result, or nil when
// err carries no classification (which json omitempty then drops entirely).
// op is the resolved tool name; see toolErrorEnvelope for how it is applied.
func toolErrorMeta(err error, op string) map[string]any {
	env := toolErrorEnvelope(err, op)
	if env == nil {
		return nil
	}
	return map[string]any{MetaToolErrorKey: env}
}

// toolErrorEnvelope renders the classification of err, or nil when there is
// none.
//
// op names the operation the failure belongs to. It is applied only when the
// error does not already carry one: the dispatch boundary is the place that
// reliably knows the resolved tool name, but a call site that knew better must
// not have its answer overwritten by the generic one.
func toolErrorEnvelope(err error, op string) map[string]any {
	te, ok := toolerror.Classify(err)
	if !ok {
		return nil
	}
	if te.Op == "" && op != "" {
		te = te.WithOp(op)
	}
	env := map[string]any{
		"kind":        string(te.Kind),
		"retryable":   te.Retryable,
		"remediation": remediationWire(te.Remediation),
	}
	if te.Op != "" {
		env["operation"] = te.Op
	}
	if len(te.Details) > 0 {
		env["details"] = maps.Clone(te.Details)
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

// invalidCallEnvelope classifies a tools/call plumb refused before any tool
// ran — malformed params, an unknown tool name — and renders it for
// `error.data`. These two never reach the tool-result path, so they are the
// only failures whose envelope travels as a JSON-RPC error rather than as
// `_meta` on a result.
func invalidCallEnvelope(msg, op string) map[string]any {
	return toolErrorEnvelope(
		toolerror.Wrap(invalidCallErr(msg), toolerror.KindInvalidArguments, toolerror.ClassFixArguments),
		op,
	)
}

// invalidCallErr carries the already-rendered rejection text. The message on
// the wire is built by the call site and must not change, so the envelope is
// derived from it rather than the other way round.
type invalidCallErr string

func (e invalidCallErr) Error() string { return string(e) }
