package mcp

// resume_credential_ctx.go — a resume credential PRESENTED on a tools/call.
//
// The proxy attaches it to the request `_meta` (MetaResumeCredentialKey), and it
// rides ctx from there for the same reason the logical-agent identity does: the
// server dispatches requests concurrently, so a field on the connection would let
// two racing session_start calls read each other's credential. It is deliberately
// unexported on the way in, so nothing but handleToolsCall can place one.

import (
	"context"
	"encoding/json"
)

type resumeCredentialCtxKey struct{}

// resumeCredentialFromMeta extracts the presented credential from an already-decoded
// tools/call `_meta` map, fail-safe: an absent, wrong-type or malformed value yields
// "". Only the request `_meta` is consulted; the arguments are the model's and a
// credential typed there is not a presentation.
func resumeCredentialFromMeta(meta map[string]json.RawMessage) string {
	raw, ok := meta[MetaResumeCredentialKey]
	if !ok {
		return ""
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	return v
}

// withResumeCredential derives the per-request ctx carrying a presented credential.
// An empty value returns the parent unchanged, so a call that presents nothing pays
// nothing and cannot inherit a value from an earlier ctx.
func withResumeCredential(ctx context.Context, secret string) context.Context {
	if secret == "" {
		return ctx
	}
	return context.WithValue(ctx, resumeCredentialCtxKey{}, secret)
}

// ResumeCredentialFromCtx returns the resume credential a tools/call presented in its
// `_meta`, or "" when it presented none. The caller must treat the value as a bearer
// secret: never log it, never echo it into a tool result.
func ResumeCredentialFromCtx(ctx context.Context) string {
	v, _ := ctx.Value(resumeCredentialCtxKey{}).(string)
	return v
}
