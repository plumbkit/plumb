package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// fakeCrossDiag implements postWriteDiagSource AND crossFileDiagSource so the
// cross-file sweep can be exercised end-to-end. all/times are mutated between
// baseline capture and the post-write read to simulate a language server
// re-publishing after an edit.
type fakeCrossDiag struct {
	all   map[string][]protocol.Diagnostic
	times map[string]time.Time
}

func (f *fakeCrossDiag) Diagnostics(uri string) []protocol.Diagnostic { return f.all[uri] }

func (f *fakeCrossDiag) WaitNextDiagnostics(_ context.Context, uri string) ([]protocol.Diagnostic, error) {
	return f.all[uri], nil
}

func (f *fakeCrossDiag) AllDiagnostics() map[string][]protocol.Diagnostic { return f.all }

func (f *fakeCrossDiag) AllDiagnosticTimes() map[string]time.Time { return f.times }

func errAt(msg string, line uint32) protocol.Diagnostic {
	return protocol.Diagnostic{
		Severity: protocol.SevError,
		Message:  msg,
		Range:    protocol.Range{Start: protocol.Position{Line: line}},
	}
}

func TestComputeCrossFileDelta(t *testing.T) {
	t0 := time.Unix(1000, 0)
	after := t0.Add(time.Second)
	before := t0.Add(-time.Second)
	edited := "file://edited.go"

	base := &diagBaseline{
		at: t0,
		errCount: map[string]int{
			"file://pre.go":    2,
			"file://grew.go":   1,
			"file://edited.go": 0,
		},
		messages: map[string]map[string]bool{
			"file://pre.go":  {"old1": true, "old2": true},
			"file://grew.go": {"kept": true},
		},
	}
	post := map[string][]protocol.Diagnostic{
		"file://broke.go":   {errAt("boom", 43)},                             // 0 -> 1, brand new
		"file://grew.go":    {errAt("kept", 0), errAt("fresh", 7)},           // 1 -> 2, one new
		"file://pre.go":     {errAt("old1", 0), errAt("old2", 1)},            // unchanged count
		"file://edited.go":  {errAt("self", 0)},                              // edited file, excluded
		"file://lagging.go": {errAt("late", 0)},                              // new but re-published before the write
		"file://warn.go":    {{Severity: protocol.SevWarning, Message: "w"}}, // warning only, ignored
	}
	postTimes := map[string]time.Time{
		"file://broke.go":   after,
		"file://grew.go":    after,
		"file://pre.go":     after,
		"file://edited.go":  after,
		"file://lagging.go": before,
		"file://warn.go":    after,
	}

	got := computeCrossFileDelta(base, post, postTimes, edited)
	if len(got) != 2 {
		t.Fatalf("want 2 breaks (broke.go, grew.go), got %d: %+v", len(got), got)
	}
	// Sorted by URI: broke.go before grew.go.
	if got[0].uri != "file://broke.go" || got[0].baseErrs != 0 || got[0].postErrs != 1 ||
		got[0].exampleMsg != "boom" || got[0].exampleLine != 44 {
		t.Errorf("broke.go break wrong: %+v", got[0])
	}
	if got[1].uri != "file://grew.go" || got[1].baseErrs != 1 || got[1].postErrs != 2 ||
		got[1].exampleMsg != "fresh" || got[1].exampleLine != 8 {
		t.Errorf("grew.go break wrong: %+v", got[1])
	}
}

func TestComputeCrossFileDelta_NilBaseline(t *testing.T) {
	if got := computeCrossFileDelta(nil, map[string][]protocol.Diagnostic{"a": errDiag("x")}, nil, ""); got != nil {
		t.Errorf("nil baseline must yield nil, got %+v", got)
	}
}

func TestFormatCrossFileDiagnostics(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		if s := formatCrossFileDiagnostics(nil, "/ws"); s != "" {
			t.Errorf("no breaks should render empty, got %q", s)
		}
	})
	t.Run("single break relativised", func(t *testing.T) {
		breaks := []crossFileBreak{{uri: "file:///ws/internal/foo.go", baseErrs: 0, postErrs: 3, exampleMsg: "not enough arguments", exampleLine: 44}}
		s := formatCrossFileDiagnostics(breaks, "/ws")
		for _, want := range []string{"introduced new errors in 1 other file", "internal/foo.go", "+3 errors (0 → 3)", "e.g. L44: not enough arguments", "mid-refactor", "diagnostics()"} {
			if !strings.Contains(s, want) {
				t.Errorf("output missing %q\n---\n%s", want, s)
			}
		}
	})
	t.Run("overflow", func(t *testing.T) {
		var breaks []crossFileBreak
		for i := 0; i < 5; i++ {
			breaks = append(breaks, crossFileBreak{uri: "file:///ws/f.go", postErrs: 1})
		}
		s := formatCrossFileDiagnostics(breaks, "/ws")
		if !strings.Contains(s, "…(+2 more)") {
			t.Errorf("expected overflow line, got %q", s)
		}
	})
}

func TestWriteDeps_crossFileDiagnostics(t *testing.T) {
	f := &fakeCrossDiag{all: map[string][]protocol.Diagnostic{}, times: map[string]time.Time{}}
	d := WriteDeps{Diag: f, CrossFileDiag: true, WorkspaceFn: func() string { return "/ws" }}

	baseline := d.capturePreWriteBaseline("file:///ws/edited.go")
	if baseline == nil {
		t.Fatal("expected a baseline from a cross-file-capable source")
	}
	// Simulate the LSP re-publishing a NEW error in another file after the write.
	time.Sleep(2 * time.Millisecond)
	f.all["file:///ws/b.go"] = []protocol.Diagnostic{errAt("broke", 9)}
	f.times["file:///ws/b.go"] = time.Now()

	out := d.crossFileDiagnostics("file:///ws/edited.go", true, baseline)
	if !strings.Contains(out, "b.go") || !strings.Contains(out, "introduced new errors") {
		t.Fatalf("expected cross-file heads-up, got %q", out)
	}

	if got := d.crossFileDiagnostics("file:///ws/edited.go", false, baseline); got != "" {
		t.Errorf("fresh=false must suppress the sweep, got %q", got)
	}
	if got := d.crossFileDiagnostics("file:///ws/edited.go", true, nil); got != "" {
		t.Errorf("nil baseline must suppress the sweep, got %q", got)
	}

	disabled := WriteDeps{Diag: f, CrossFileDiag: false, WorkspaceFn: func() string { return "/ws" }}
	if got := disabled.crossFileDiagnostics("file:///ws/edited.go", true, baseline); got != "" {
		t.Errorf("disabled sweep must be silent, got %q", got)
	}
}

// TestWriteDeps_postWriteDiagnostics_StandingPreExistingNote exercises the
// omitted-pre-existing-issues heads-up through the full postWriteDiagnostics
// path: a clean edit over a file that still carries a pre-existing error appends
// the note, while a clean baseline stays silent.
func TestWriteDeps_postWriteDiagnostics_StandingPreExistingNote(t *testing.T) {
	edited := "file:///ws/edited.go"

	t.Run("standing pre-existing error appends the note", func(t *testing.T) {
		// Pre-existing error on line 2 (0-based 1); it persists after the write.
		f := &fakeCrossDiag{
			all:   map[string][]protocol.Diagnostic{edited: {errAt("undefined: Foo", 1)}},
			times: map[string]time.Time{},
		}
		d := WriteDeps{Diag: f, WorkspaceFn: func() string { return "/ws" }}
		baseline := d.capturePreWriteBaseline(edited)

		// The edit touches the last line only; the pre-existing error is elsewhere,
		// so it is carried over (dropped from the delta) and the edit is otherwise
		// clean.
		out := d.postWriteDiagnostics(edited, "a\nb\nc\nd", "a\nb\nc\nD", false, baseline)
		if !strings.Contains(out, "1 pre-existing issue in this file not shown") {
			t.Fatalf("expected the standing pre-existing note, got:\n%q", out)
		}
		if !strings.Contains(out, "diagnostics()") {
			t.Fatalf("note must point at diagnostics(), got:\n%q", out)
		}
	})

	t.Run("clean baseline stays silent", func(t *testing.T) {
		f := &fakeCrossDiag{all: map[string][]protocol.Diagnostic{}, times: map[string]time.Time{}}
		d := WriteDeps{Diag: f, WorkspaceFn: func() string { return "/ws" }}
		baseline := d.capturePreWriteBaseline(edited)

		out := d.postWriteDiagnostics(edited, "a\nb", "a\nB", false, baseline)
		if strings.Contains(out, "pre-existing") {
			t.Fatalf("a clean baseline must not mention pre-existing issues, got:\n%q", out)
		}
		if out != "" {
			t.Fatalf("a clean edit over a clean file must render nothing, got:\n%q", out)
		}
	})
}

func TestWriteDeps_capturePreWriteBaseline_NarrowSource(t *testing.T) {
	stub := newStubDiag()
	stub.set(errDiag("pre-existing"))
	d := WriteDeps{Diag: stub, CrossFileDiag: true}

	// A narrow (non-cross-file) source still yields a single-file baseline (the
	// edited file's own pre-write diagnostics), so the differential block works;
	// it carries no whole-workspace error maps, so the cross-file sweep is a no-op.
	b := d.capturePreWriteBaseline("file:///ws/edited.go")
	if b == nil {
		t.Fatal("expected a single-file baseline from a narrow source")
	}
	if len(b.editedPre) != 1 || b.editedPre[0].Message != "pre-existing" {
		t.Errorf("expected the edited file's pre-write diagnostics, got %+v", b.editedPre)
	}
	if b.errCount != nil || b.messages != nil {
		t.Errorf("a narrow source must not populate the cross-file maps, got errCount=%v messages=%v", b.errCount, b.messages)
	}
	if got := d.crossFileDiagnostics("file:///ws/edited.go", true, b); got != "" {
		t.Errorf("a narrow source must make the cross-file sweep a no-op, got %q", got)
	}
}
