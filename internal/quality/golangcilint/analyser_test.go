package golangcilint

import (
	"testing"

	"github.com/golimpio/plumb/internal/quality"
)

func TestSupports(t *testing.T) {
	a := New()
	cases := []struct {
		path string
		want bool
	}{
		{"/project/foo.go", true},
		{"/project/foo.py", false},
		{"/project/foo.ts", false},
		{"/project/noext", false},
		{"/project/foo.go.bak", false},
	}
	for _, tc := range cases {
		if got := a.Supports(tc.path); got != tc.want {
			t.Errorf("Supports(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestParseOutput_Valid(t *testing.T) {
	data := []byte(`{
		"Issues": [
			{
				"FromLinter": "ineffassign",
				"Text": "assignment to err is ineffective",
				"Severity": "warning",
				"SourceLines": ["err = foo()"],
				"Pos": {"Filename": "foo.go", "Line": 10, "Column": 3}
			},
			{
				"FromLinter": "errcheck",
				"Text": "Error return value of doSomething is not checked",
				"Severity": "error",
				"SourceLines": ["doSomething()"],
				"Pos": {"Filename": "bar.go", "Line": 25, "Column": 1}
			}
		]
	}`)

	findings, err := parseOutput(data, "golangci-lint")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(findings) != 2 {
		t.Fatalf("got %d findings, want 2", len(findings))
	}

	f0 := findings[0]
	if f0.Code != "ineffassign" {
		t.Errorf("findings[0].Code = %q, want %q", f0.Code, "ineffassign")
	}
	if f0.Severity != quality.SeverityWarning {
		t.Errorf("findings[0].Severity = %v, want Warning", f0.Severity)
	}
	if f0.Line != 10 || f0.Column != 3 {
		t.Errorf("findings[0] pos = (%d,%d), want (10,3)", f0.Line, f0.Column)
	}
	if f0.Source != "golangci-lint" {
		t.Errorf("findings[0].Source = %q, want %q", f0.Source, "golangci-lint")
	}

	f1 := findings[1]
	if f1.Severity != quality.SeverityError {
		t.Errorf("findings[1].Severity = %v, want Error", f1.Severity)
	}
	if f1.File != "bar.go" {
		t.Errorf("findings[1].File = %q, want %q", f1.File, "bar.go")
	}
}

func TestParseOutput_EmptyIssues(t *testing.T) {
	data := []byte(`{"Issues": []}`)
	findings, err := parseOutput(data, "golangci-lint")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("got %d findings, want 0", len(findings))
	}
}

func TestParseOutput_Malformed(t *testing.T) {
	// Malformed JSON should return nil, nil (graceful degradation).
	findings, err := parseOutput([]byte(`not json`), "golangci-lint")
	if err != nil {
		t.Fatalf("expected nil error for malformed JSON, got: %v", err)
	}
	if findings != nil {
		t.Errorf("expected nil findings for malformed JSON")
	}
}

func TestParseOutput_NullIssues(t *testing.T) {
	data := []byte(`{"Issues": null}`)
	findings, err := parseOutput(data, "golangci-lint")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("got %d findings, want 0", len(findings))
	}
}

func TestAnalyse_EmptyFiles(t *testing.T) {
	a := New()
	findings, err := a.Analyse(t.Context(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if findings != nil {
		t.Errorf("expected nil findings for empty file list")
	}
}
