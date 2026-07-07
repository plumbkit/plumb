package tools

import (
	"testing"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// mkDiag builds one diagnostic of the given severity, message, and 0-based line.
func mkDiag(sev protocol.DiagnosticSeverity, msg string, line uint32) protocol.Diagnostic {
	d := protocol.Diagnostic{Severity: sev, Message: msg}
	d.Range.Start.Line = line
	return d
}

func msgsOf(ds []protocol.Diagnostic) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.Message
	}
	return out
}

func hasMsg(ds []protocol.Diagnostic, msg string) bool {
	for _, d := range ds {
		if d.Message == msg {
			return true
		}
	}
	return false
}

// TestDiffFileDiagnostics is the core repro-first table for differential
// post-write diagnostics: given a pre-write set and a post-write set, only the
// genuinely NEW diagnostics survive, and known re-index-lag classes near the
// edit are separated from real breakage.
func TestDiffFileDiagnostics(t *testing.T) {
	const errSev = protocol.SevError
	const warnSev = protocol.SevWarning

	tests := []struct {
		name           string
		pre, post      []protocol.Diagnostic
		lo, hi         int
		touched        bool
		wantFresh      []string // messages expected in the fresh (real) group
		wantStale      []string // messages expected in the likely-stale group
		wantDroppedAll bool     // both groups must be empty
	}{
		{
			name: "pre-existing unused import survives an unrelated edit and is dropped",
			// The edit is elsewhere; the pre-existing unused import lingers in the
			// post set (stale copy). It must be dropped, and the genuinely new
			// error reported.
			pre:  []protocol.Diagnostic{mkDiag(warnSev, `"io" imported and not used`, 2)},
			post: []protocol.Diagnostic{mkDiag(warnSev, `"io" imported and not used`, 2), mkDiag(errSev, "not enough arguments in call to f", 40)},
			lo:   38, hi: 42, touched: true,
			wantFresh: []string{"not enough arguments in call to f"},
			wantStale: nil,
		},
		{
			name: "a fixed error lingering in the post set is dropped (the #1 complaint)",
			// The edit resolved "undefined: Foo" (added the import) but gopls still
			// reports it against the same line. It was present pre-write → dropped.
			pre:  []protocol.Diagnostic{mkDiag(errSev, "undefined: Foo", 10)},
			post: []protocol.Diagnostic{mkDiag(errSev, "undefined: Foo", 10)},
			lo:   9, hi: 11, touched: true,
			wantDroppedAll: true,
		},
		{
			name: "a genuinely new error the agent introduced is reported",
			pre:  nil,
			post: []protocol.Diagnostic{mkDiag(errSev, "syntax error: unexpected }", 5)},
			lo:   4, hi: 6, touched: true,
			wantFresh: []string{"syntax error: unexpected }"},
		},
		{
			name: "line-shifted pre-existing diagnostic is not re-reported as new",
			// An insert above shifted a pre-existing error from line 10 to line 13.
			// Message+code identity ignores the line, so it is recognised as carried
			// over and dropped rather than double-counted.
			pre:  []protocol.Diagnostic{mkDiag(errSev, "cannot use x as int", 10)},
			post: []protocol.Diagnostic{mkDiag(errSev, "cannot use x as int", 13)},
			lo:   0, hi: 3, touched: true,
			wantDroppedAll: true,
		},
		{
			name: "an additional occurrence of the same message IS reported new (count-aware)",
			// One pre-existing "declared and not used: x"; two post-write. The count
			// exceeds the baseline, so the extra one is new. (It is a re-index-lag
			// class, but off the touched line → stays a hard error.)
			pre:  []protocol.Diagnostic{mkDiag(errSev, "declared and not used: x", 5)},
			post: []protocol.Diagnostic{mkDiag(errSev, "declared and not used: x", 5), mkDiag(errSev, "declared and not used: x", 90)},
			lo:   4, hi: 6, touched: true,
			wantFresh: []string{"declared and not used: x"},
		},
		{
			name: "new re-index-lag class on a touched line is separated as likely-stale",
			pre:  nil,
			post: []protocol.Diagnostic{mkDiag(errSev, "undefined: Bar", 20)},
			lo:   18, hi: 22, touched: true,
			wantStale: []string{"undefined: Bar"},
		},
		{
			name: "new re-index-lag class AWAY from the edit stays a real error",
			// Deleting a used helper breaks a far-away call site: genuinely the
			// edit's fault, so it must NOT be softened to likely-stale.
			pre:  nil,
			post: []protocol.Diagnostic{mkDiag(errSev, "undefined: helper", 200)},
			lo:   10, hi: 12, touched: true,
			wantFresh: []string{"undefined: helper"},
		},
		{
			name: "unknown touched range never softens a re-index-lag class",
			// touched=false (e.g. write_file with the diff disabled): the class is
			// new but we cannot place the edit, so we report it rather than hide it.
			pre:       nil,
			post:      []protocol.Diagnostic{mkDiag(errSev, "undefined: Baz", 3)},
			touched:   false,
			wantFresh: []string{"undefined: Baz"},
		},
		{
			name: "a NEW warning is never softened even on a touched line",
			// The re-index-lag softening is error-only; a genuinely new warning is
			// reported as-is.
			pre:  nil,
			post: []protocol.Diagnostic{mkDiag(warnSev, "undefined: W", 5)},
			lo:   4, hi: 6, touched: true,
			wantFresh: []string{"undefined: W"},
		},
		{
			name: "hint/info severities are ignored",
			pre:  nil,
			post: []protocol.Diagnostic{mkDiag(protocol.SevInformation, "consider simplifying", 5)},
			lo:   4, hi: 6, touched: true,
			wantDroppedAll: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fresh, stale := diffFileDiagnostics(tt.pre, tt.post, tt.lo, tt.hi, tt.touched)
			if tt.wantDroppedAll {
				if len(fresh) != 0 || len(stale) != 0 {
					t.Fatalf("expected everything dropped, got fresh=%v stale=%v", msgsOf(fresh), msgsOf(stale))
				}
				return
			}
			for _, m := range tt.wantFresh {
				if !hasMsg(fresh, m) {
					t.Errorf("expected %q in fresh, got fresh=%v stale=%v", m, msgsOf(fresh), msgsOf(stale))
				}
				if hasMsg(stale, m) {
					t.Errorf("%q must not be in the likely-stale group", m)
				}
			}
			for _, m := range tt.wantStale {
				if !hasMsg(stale, m) {
					t.Errorf("expected %q in likely-stale, got fresh=%v stale=%v", m, msgsOf(fresh), msgsOf(stale))
				}
				if hasMsg(fresh, m) {
					t.Errorf("%q must not be in the fresh group", m)
				}
			}
		})
	}
}

func TestChangedLineRange(t *testing.T) {
	tests := []struct {
		name           string
		before, after  string
		wantLo, wantHi int
		wantOK         bool
	}{
		{"identical", "a\nb\nc", "a\nb\nc", 0, 0, false},
		{"empty before is unknown", "", "a\nb", 0, 0, false},
		{"single middle line changed", "a\nb\nc", "a\nB\nc", 1, 1, true},
		{"insert one line", "a\nc", "a\nb\nc", 1, 1, true},
		{"append lines", "a\nb", "a\nb\nc\nd", 2, 3, true},
		{"pure deletion collapses to the join point", "a\nb\nc\nd", "a\nd", 1, 1, true},
		{"change spans several lines", "a\nb\nc\nd", "a\nX\nY\nd", 1, 2, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lo, hi, ok := changedLineRange(tt.before, tt.after)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v (lo=%d hi=%d)", ok, tt.wantOK, lo, hi)
			}
			if ok && (lo != tt.wantLo || hi != tt.wantHi) {
				t.Fatalf("range = [%d,%d], want [%d,%d]", lo, hi, tt.wantLo, tt.wantHi)
			}
		})
	}
}

func TestIsReindexLagClass(t *testing.T) {
	lag := []string{
		"undefined: Foo",
		`"io" imported and not used`,
		"declared and not used: x",
		"x declared but not used",
	}
	for _, m := range lag {
		if !isReindexLagClass(m) {
			t.Errorf("expected %q to be a re-index-lag class", m)
		}
	}
	notLag := []string{
		"syntax error: unexpected }",
		"cannot use x (int) as string",
		"not enough arguments in call to f",
		"",
	}
	for _, m := range notLag {
		if isReindexLagClass(m) {
			t.Errorf("did not expect %q to be a re-index-lag class", m)
		}
	}
}
