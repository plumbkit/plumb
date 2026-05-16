package cli

import (
	"regexp"
	"strings"
	"testing"
)

var ansiPattern = regexp.MustCompile(`\x1b\[[0-9;:]*m`)

func TestRenderCLIDiagnosticExpandedShape(t *testing.T) {
	out := stripANSI(renderCLIDiagnostic(cliDiagnostic{
		Kind:  "error",
		Title: "no workspace found",
		Body:  "Plumb could not resolve a project from /tmp.",
		Suggestions: []string{
			"plumb init",
			"plumb status --workspace /path/to/project",
		},
	}, 80))

	for _, want := range []string{
		" ✗  no workspace found",
		"    ┊ Plumb could not resolve a project from /tmp.",
		"    ┊",
		"    ┊ Try:",
		"    ┊  plumb init",
		"    ┊  plumb status --workspace /path/to/project",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("rendered diagnostic missing %q:\n%s", want, out)
		}
	}
}

func TestRenderCLIDiagnosticInfoShape(t *testing.T) {
	out := stripANSI(renderCLIDiagnostic(cliDiagnostic{
		Kind:  "info",
		Title: "No statistics recorded yet",
		Body:  "No statistics recorded yet for /tmp.",
	}, 80))

	for _, want := range []string{
		" i  No statistics recorded yet",
		"    ┊ No statistics recorded yet for /tmp.",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("rendered diagnostic missing %q:\n%s", want, out)
		}
	}
}

func TestRenderCLIDiagnosticWrapsBody(t *testing.T) {
	out := stripANSI(renderCLIDiagnostic(cliDiagnostic{
		Title: "error",
		Body:  "Plumb could not resolve a project from a very long path because no workspace marker was found.",
	}, 42))

	if got := strings.Count(out, "    ┊ "); got < 2 {
		t.Fatalf("expected wrapped diagnostic body, got %d bordered lines:\n%s", got, out)
	}
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if strings.Contains(line, "┊") && !strings.HasPrefix(line, "    ┊ ") {
			t.Fatalf("wrapped diagnostic continuation lost body alignment:\n%s", out)
		}
	}
}

func stripANSI(s string) string {
	return ansiPattern.ReplaceAllString(s, "")
}
