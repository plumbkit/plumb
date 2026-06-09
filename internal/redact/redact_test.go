package redact

import (
	"strings"
	"testing"
)

func TestRedact_Secrets(t *testing.T) {
	cases := []struct {
		name string
		in   string
		kind string // expected placeholder fragment
	}{
		{"aws_key", "key AKIAIOSFODNN7EXAMPLE here", "aws-key"},
		{"github", "tok ghp_0123456789abcdefghijklmnopqrstuvwxyz here", "github-token"},
		{"slack", "xoxb-123456789012-abcdefghijkl", "slack-token"},
		{"jwt", "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N", "jwt"},
		{"url_creds", "clone https://alice:s3cretpw@github.com/x.git", "url-credentials"},
		{"api_key_assign", `api_key = "sk-abcdef123456"`, "secret"},
		{"password_assign", "password: hunter2xyz", "secret"},
		{"auth_header", "Authorization: Bearer abcdef123456", "auth-header"},
		{"private_key", "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA\n-----END RSA PRIVATE KEY-----", "private-key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, n := Redact(tc.in)
			if n == 0 {
				t.Fatalf("expected a redaction, got none for %q", tc.in)
			}
			if !strings.Contains(out, "[REDACTED:"+tc.kind+"]") {
				t.Errorf("expected [REDACTED:%s], got %q", tc.kind, out)
			}
			if !ContainsSecret(tc.in) {
				t.Errorf("ContainsSecret returned false for a secret: %q", tc.in)
			}
		})
	}
}

func TestRedact_FalsePositiveGuards(t *testing.T) {
	// Benign prose — including the path/symbol mentions an episodic summary will
	// contain — must never be redacted.
	clean := []string{
		"We use the token bucket algorithm for rate limiting.",
		"The secret to good code is simplicity.",
		"See config.yaml for the api settings.",
		"## Authentication and authorisation",
		"Note: this paragraph is fine.",
		"He set the timeout to 30 seconds.",
		"Modified internal/auth/login.go and ran find_references on UserSession.",
	}
	for _, s := range clean {
		out, n := Redact(s)
		if n != 0 {
			t.Errorf("over-redacted %q -> %q (n=%d)", s, out, n)
		}
		if ContainsSecret(s) {
			t.Errorf("ContainsSecret true for benign text: %q", s)
		}
	}
}

func TestRedact_PreservesURLScheme(t *testing.T) {
	out, _ := Redact("git clone https://alice:s3cretpw@example.com/repo.git")
	if !strings.Contains(out, "https://[REDACTED:url-credentials]@example.com") {
		t.Errorf("scheme/host not preserved: %q", out)
	}
}

func TestRedact_NoSecretIsNoOp(t *testing.T) {
	in := "plain text with no secrets"
	out, n := Redact(in)
	if out != in || n != 0 {
		t.Errorf("expected no-op, got %q n=%d", out, n)
	}
}
