package tools

import (
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// TestApplyTextEdits_ClampEndPastEOF is the RC2 regression suite: an edit whose
// END position points at (or a little past) end-of-file must clamp to len(data)
// and apply, instead of failing "edit end position out of range". A START past
// EOF, an END overshooting by more than maxEndOvershootLines, and an intra-line
// overrun on an earlier line must all still fail — the clamp is bounded so a
// genuinely stale range can never silently eat the file.
func TestApplyTextEdits_ClampEndPastEOF(t *testing.T) {
	rng := func(sl, sc, el, ec uint32) protocol.Range {
		return protocol.Range{
			Start: protocol.Position{Line: sl, Character: sc},
			End:   protocol.Position{Line: el, Character: ec},
		}
	}
	cases := []struct {
		name    string
		data    string
		edit    protocol.TextEdit
		want    string // expected result when wantErr is false
		wantErr bool
		errHas  string
	}{
		{
			// Control: end exactly at len(data) already resolves — no clamp needed.
			name: "end exactly at len(data)",
			data: "abc",
			edit: protocol.TextEdit{Range: rng(0, 0, 0, 3), NewText: "Z"},
			want: "Z",
		},
		{
			// No trailing newline: an LSP end one char past the last line's content
			// (0,4 when the line ends at col 3) is the "one past EOF" case.
			name: "end one char past EOF, no trailing newline",
			data: "abc",
			edit: protocol.TextEdit{Range: rng(0, 0, 0, 4), NewText: "Z"},
			want: "Z",
		},
		{
			// Trailing newline: an end on the (non-existent) line after the final
			// newline — a legitimate "to end of file" range off by one line.
			name: "end on non-existent trailing line",
			data: "abc\n",
			edit: protocol.TextEdit{Range: rng(0, 0, 2, 0), NewText: "Z\n"},
			want: "Z\n",
		},
		{
			// Wild overshoot — the classic stale range against a longer former
			// version of the file. Must fail, never truncate.
			name:    "wild end overshoot still errors",
			data:    "abc\n",
			edit:    protocol.TextEdit{Range: rng(0, 0, 12, 0), NewText: "Z\n"},
			wantErr: true,
			errHas:  "end position out of range",
		},
		{
			// A START past EOF keeps the stricter guard — it is always an error.
			name:    "start past EOF still errors",
			data:    "abc",
			edit:    protocol.TextEdit{Range: rng(5, 0, 5, 0), NewText: "Z"},
			wantErr: true,
			errHas:  "start position out of range",
		},
		{
			// An intra-line overrun on an EARLIER line is a broken range, not an
			// end-of-file end — it must not clamp to EOF and swallow the tail.
			name:    "end char past a middle line still errors",
			data:    "ab\ncd\n",
			edit:    protocol.TextEdit{Range: rng(0, 0, 0, 50), NewText: "Z"},
			wantErr: true,
			errHas:  "end position out of range",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := applyTextEdits([]byte(c.data), []protocol.TextEdit{c.edit})
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got result %q", got)
				}
				if c.errHas != "" && !strings.Contains(err.Error(), c.errHas) {
					t.Fatalf("error %q does not contain %q", err, c.errHas)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if string(got) != c.want {
				t.Fatalf("result mismatch\n got: %q\nwant: %q", got, c.want)
			}
		})
	}
}

// TestEndOffsetForPosition_Bound documents the clamp boundary directly: exactly
// maxEndOvershootLines past the final line clamps, one beyond fails.
func TestEndOffsetForPosition_Bound(t *testing.T) {
	data := []byte("a\n") // one newline → final line index 1
	atBound := protocol.Position{Line: 1 + maxEndOvershootLines, Character: 0}
	if off, ok := endOffsetForPosition(data, atBound); !ok || off != len(data) {
		t.Fatalf("at bound: got (%d,%v), want (%d,true)", off, ok, len(data))
	}
	beyond := protocol.Position{Line: 1 + maxEndOvershootLines + 1, Character: 0}
	if _, ok := endOffsetForPosition(data, beyond); ok {
		t.Fatalf("beyond bound should not clamp")
	}
}
