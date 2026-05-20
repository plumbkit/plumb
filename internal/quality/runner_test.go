package quality

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stubAnalyser is a controllable Analyser for tests.
type stubAnalyser struct {
	name     string
	supports func(string) bool
	findings []Finding
	called   int
}

func (s *stubAnalyser) Name() string { return s.name }
func (s *stubAnalyser) Supports(path string) bool {
	if s.supports != nil {
		return s.supports(path)
	}
	return true
}

func (s *stubAnalyser) Analyse(_ context.Context, _ []string) ([]Finding, error) {
	s.called++
	return s.findings, nil
}

func goStub(findings []Finding) *stubAnalyser {
	return &stubAnalyser{
		name: "stub",
		supports: func(p string) bool {
			return filepath.Ext(p) == ".go"
		},
		findings: findings,
	}
}

func writeTempFile(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "*.go")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return f.Name()
}

func TestRunner_SyncMode_ReturnsFindingsInline(t *testing.T) {
	path := writeTempFile(t, "package main\n")
	stub := goStub([]Finding{
		{File: path, Line: 1, Code: "dummy", Message: "test finding", Severity: SeverityWarning},
	})

	r := NewRunner(RunnerConfig{
		Analysers:          []Analyser{stub},
		Mode:               "sync",
		Timeout:            2 * time.Second,
		MaxFindingsPerFile: 5,
	})
	r.Start()
	defer r.Stop()

	out := r.Report(context.Background(), path)
	if !strings.Contains(out, "dummy") {
		t.Errorf("Report() = %q, want it to contain finding code", out)
	}
	if !strings.Contains(out, "test finding") {
		t.Errorf("Report() = %q, want it to contain finding message", out)
	}
	if stub.called != 1 {
		t.Errorf("stub called %d times, want 1", stub.called)
	}
}

func TestRunner_SyncMode_CacheHitSkipsReanalysis(t *testing.T) {
	path := writeTempFile(t, "package main\n")
	stub := goStub([]Finding{
		{File: path, Line: 1, Code: "dummy", Message: "msg", Severity: SeverityWarning},
	})

	r := NewRunner(RunnerConfig{
		Analysers:          []Analyser{stub},
		Mode:               "sync",
		Timeout:            2 * time.Second,
		MaxFindingsPerFile: 5,
	})
	r.Start()
	defer r.Stop()

	r.Report(context.Background(), path)
	r.Report(context.Background(), path)

	if stub.called != 1 {
		t.Errorf("stub called %d times, want 1 (cache should prevent re-analysis)", stub.called)
	}
}

func TestRunner_BackgroundMode_CachedFindingsReturnedOnSecondReport(t *testing.T) {
	path := writeTempFile(t, "package main\n")
	stub := goStub([]Finding{
		{File: path, Line: 2, Code: "vet", Message: "vet issue", Severity: SeverityError},
	})

	r := NewRunner(RunnerConfig{
		Analysers:          []Analyser{stub},
		Mode:               "background",
		Timeout:            2 * time.Second,
		MaxFindingsPerFile: 5,
	})
	r.Start()
	defer r.Stop()

	// First call enqueues — no cached result yet.
	first := r.Report(context.Background(), path)
	if first != "" {
		t.Logf("first Report = %q (acceptable: may be empty in background mode)", first)
	}

	// Wait for the background worker to process.
	deadline := time.Now().Add(3 * time.Second)
	var second string
	for time.Now().Before(deadline) {
		second = r.Report(context.Background(), path)
		if second != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if second == "" {
		t.Error("expected cached findings on second Report after background worker ran")
	}
	if !strings.Contains(second, "vet") {
		t.Errorf("second Report = %q, want it to contain finding code", second)
	}
}

func TestRunner_NoAnalysers_ReturnsEmpty(t *testing.T) {
	r := NewRunner(RunnerConfig{Mode: "sync"})
	r.Start()
	defer r.Stop()

	out := r.Report(context.Background(), "/some/file.go")
	if out != "" {
		t.Errorf("Report with no analysers = %q, want empty", out)
	}
}

func TestRunner_UnsupportedFile_ReturnsEmpty(t *testing.T) {
	stub := &stubAnalyser{
		name:     "go-only",
		supports: func(p string) bool { return filepath.Ext(p) == ".go" },
		findings: []Finding{{Line: 1, Code: "x", Message: "y"}},
	}
	r := NewRunner(RunnerConfig{
		Analysers: []Analyser{stub},
		Mode:      "sync",
	})
	r.Start()
	defer r.Stop()

	out := r.Report(context.Background(), "/project/main.py")
	if out != "" {
		t.Errorf("Report for .py file = %q, want empty", out)
	}
}

func TestRunner_Format_OverflowNote(t *testing.T) {
	findings := make([]Finding, 7)
	for i := range findings {
		findings[i] = Finding{Line: i + 1, Code: "lint", Message: "msg"}
	}
	r := NewRunner(RunnerConfig{
		Analysers:          []Analyser{&stubAnalyser{name: "stub"}},
		MaxFindingsPerFile: 3,
	})

	out := r.format(findings)
	if !strings.Contains(out, "and 4 more") {
		t.Errorf("format() = %q, want overflow note '… and 4 more'", out)
	}
}

func TestRunner_Format_NoOverflowWhenUnderLimit(t *testing.T) {
	findings := []Finding{
		{Line: 1, Code: "lint", Message: "msg"},
	}
	r := NewRunner(RunnerConfig{
		Analysers:          []Analyser{&stubAnalyser{name: "stub"}},
		MaxFindingsPerFile: 5,
	})

	out := r.format(findings)
	if strings.Contains(out, "more") {
		t.Errorf("format() = %q, should not contain 'more'", out)
	}
}

func TestRunner_Format_EmptyFindings(t *testing.T) {
	r := NewRunner(RunnerConfig{
		Analysers:          []Analyser{&stubAnalyser{name: "stub"}},
		MaxFindingsPerFile: 5,
	})
	out := r.format(nil)
	if out != "" {
		t.Errorf("format() with nil findings = %q, want empty", out)
	}
}

func TestRunner_Findings_ReturnsCachedSlice(t *testing.T) {
	path := writeTempFile(t, "package main\n")
	stub := goStub([]Finding{
		{File: path, Line: 1, Code: "x", Message: "y"},
	})

	r := NewRunner(RunnerConfig{
		Analysers:          []Analyser{stub},
		Mode:               "sync",
		MaxFindingsPerFile: 5,
	})
	r.Start()
	defer r.Stop()

	r.Report(context.Background(), path)
	fs := r.Findings(path)
	if len(fs) != 1 {
		t.Errorf("Findings() = %d, want 1", len(fs))
	}
}

func TestRunner_Stop_IdempotentNoPanic(t *testing.T) {
	r := NewRunner(RunnerConfig{Mode: "background"})
	r.Start()
	r.Stop()
	r.Stop() // second call must not panic
}

func TestStale_ZeroTime(t *testing.T) {
	if !stale("/nonexistent", time.Time{}) {
		t.Error("stale with zero time should return true")
	}
}

func TestStale_NonexistentFile(t *testing.T) {
	if !stale("/no/such/path.go", time.Now()) {
		t.Error("stale for nonexistent file should return true")
	}
}

func TestStale_FreshFile(t *testing.T) {
	path := writeTempFile(t, "x")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// cachedAt == file mtime means not stale.
	if stale(path, info.ModTime()) {
		t.Error("stale with exact mtime should return false")
	}
}
