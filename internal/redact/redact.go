// Package redact strips likely secrets from text before it is persisted.
//
// It is deliberately biased toward *catching* secrets: a generated memory or
// episodic summary must never carry an API key, token, or private key into
// durable storage. Plain prose that merely mentions "token" or "secret"
// (without an assignment) is left intact, so benign summaries are not mangled.
//
// Concurrency: all exported functions are safe for concurrent use — the
// compiled patterns are immutable package state.
package redact

import "regexp"

type rule struct {
	re   *regexp.Regexp
	repl string
}

// rules are applied in order. Order matters only in that broader structural
// secrets (PEM blocks, JWTs) are redacted before the generic assignment rule,
// so a key embedded in a header collapses to a single placeholder.
var rules = []rule{
	// PEM private-key blocks (any key type).
	{regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`), "[REDACTED:private-key]"},
	// JWTs (header.payload.signature, all base64url).
	{regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), "[REDACTED:jwt]"},
	// AWS access key IDs.
	{regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`), "[REDACTED:aws-key]"},
	// GitHub tokens (ghp_/gho_/ghs_/ghr_/ghu_).
	{regexp.MustCompile(`\bgh[posru]_[0-9A-Za-z]{36,}\b`), "[REDACTED:github-token]"},
	// Slack tokens (bot/user/app-level/refresh/legacy).
	{regexp.MustCompile(`\bxox[baprs]-[0-9A-Za-z-]{10,}\b`), "[REDACTED:slack-token]"},
	// Slack app-level tokens (xapp-<ver>-<id>-<secret>).
	{regexp.MustCompile(`\bxapp-[0-9]-[A-Za-z0-9-]{10,}\b`), "[REDACTED:slack-token]"},
	// Stripe secret/restricted keys (sk_live_… / rk_test_…).
	{regexp.MustCompile(`\b[sr]k_(live|test)_[0-9a-zA-Z]{10,}\b`), "[REDACTED:stripe-key]"},
	// Google API keys (AIza + 35 chars).
	{regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`), "[REDACTED:google-key]"},
	// OpenAI-style keys (sk-… / sk-proj-…). The 20-char lower bound keeps short
	// hyphenated tokens like "sk-learn" out; Stripe's underscore form is caught
	// above and does not match this hyphenated shape.
	{regexp.MustCompile(`\bsk-(proj-)?[A-Za-z0-9_-]{20,}\b`), "[REDACTED:api-key]"},
	// Credentials embedded in a URL (scheme://user:pass@host) — keep the scheme.
	{regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)[^/\s:@]+:[^/\s:@]+@`), "${1}[REDACTED:url-credentials]@"},
	// Authorization / bearer headers.
	{regexp.MustCompile(`(?i)\b(authorization|bearer)\b\s*[:=]\s*\S+`), "[REDACTED:auth-header]"},
	// Generic secret assignments: <key> = <value> (value at least 6 chars, so a
	// short non-secret like "token: ok" is not flagged).
	{regexp.MustCompile(`(?i)\b(api[_-]?key|access[_-]?key|secret[_-]?key|client[_-]?secret|secret|token|password|passwd|passphrase)\b\s*[:=]\s*["']?[^\s"',;]{6,}`), "[REDACTED:secret]"},
}

// Redact returns text with detected secrets replaced by a labelled placeholder
// (e.g. "[REDACTED:aws-key]") and the number of replacements made. A zero count
// means nothing looked like a secret.
func Redact(s string) (string, int) {
	n := 0
	for _, r := range rules {
		matches := r.re.FindAllStringIndex(s, -1)
		if len(matches) == 0 {
			continue
		}
		n += len(matches)
		s = r.re.ReplaceAllString(s, r.repl)
	}
	return s, n
}

// ContainsSecret reports whether s appears to contain a secret, without
// modifying it. Cheaper than Redact when only a yes/no decision is needed.
func ContainsSecret(s string) bool {
	for _, r := range rules {
		if r.re.MatchString(s) {
			return true
		}
	}
	return false
}
