package cli

import (
	"io"
	"os"
	"strings"
	"testing"
)

// TestShellLookup_SaysWhatItSkipsAndNeverItsValue pins the notice a capture
// prints for a variable that is not valid UTF-8 (PLAN-554 review follow-up): the
// variable's NAME, so the user knows it was not captured, and never its value —
// the same rule as every other service-environment message.
func TestShellLookup_SaysWhatItSkipsAndNeverItsValue(t *testing.T) {
	const value = "/tmp/ssh-\xe9-s3cr3t"
	t.Setenv("PLUMB_TEST_ODD_VAR", value)

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	_, _ = shellLookup("PLUMB_TEST_ODD_VAR")
	os.Stderr = old
	_ = w.Close()
	out, _ := io.ReadAll(r)

	if !strings.Contains(string(out), "PLUMB_TEST_ODD_VAR") || !strings.Contains(string(out), "not captured") {
		t.Fatalf("the skip notice does not name the variable: %q", out)
	}
	if strings.Contains(string(out), "s3cr3t") {
		t.Fatalf("the skip notice printed the value: %q", out)
	}
}
