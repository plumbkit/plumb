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
		{"slack_app", "token xapp-1-A024BE7LH-1234567890123-abcdef here", "slack-token"},
		{"stripe_live", "STRIPE_KEY=sk_live_0123456789abcdefABCDxyz", "stripe-key"},
		{"stripe_restricted_test", "key rk_test_0123456789abcdefABCD here", "stripe-key"},
		{"google_api", "GOOGLE_KEY AIzaabcdefghijklmnopqrstuvwxyz012345678 here", "google-key"},
		{"openai", "use sk-abcdefghijklmnopqrstuvwxyz0123 now", "api-key"},
		{"openai_proj", "use sk-proj-abcdefghijklmnopqrstuvwxyz0123 now", "api-key"},
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
		// sk-learn is a well-known library, not an OpenAI key (under the length bound).
		"We trained the model with sk-learn and pandas.",
		"pip install scikit-learn sk-learn-extra",
		// A short AIza-prefixed string is not a Google key (needs 35 trailing chars).
		"The variable AIzaShort is just a placeholder name.",
		// Prose mentioning a provider's key, with no actual key material.
		"Rotate the stripe key and the google api key after the incident.",
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
