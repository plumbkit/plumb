package tools

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func contestedTrue() bool { return true }

// TestResolvePath_ContestedRefusesRelative pins the seam §3 (fail closed on
// contested) lives on: once the connection's pin is contested, a RELATIVE path
// is refused before it can be anchored to whichever root holds the pin, while an
// absolute path and an empty path are untouched.
func TestResolvePath_ContestedRefusesRelative(t *testing.T) {
	ws := "/work/space"
	cases := []struct {
		name      string
		in        string
		contested ContestedFn
		wantErr   bool
		want      string
	}{
		{name: "relative refused when contested", in: "app/x.go", contested: contestedTrue, wantErr: true},
		{name: "file uri relative refused when contested", in: "file://app/x.go", contested: contestedTrue, wantErr: true},
		{name: "absolute untouched when contested", in: "/abs/x.go", contested: contestedTrue, want: "/abs/x.go"},
		{name: "empty path untouched when contested (means workspace root)", in: "", contested: contestedTrue, want: ws},
		{name: "relative anchored when not contested", in: "app/x.go", contested: nil, want: filepath.Join(ws, "app/x.go")},
		{name: "relative anchored when contested reporter says false", in: "app/x.go", contested: func() bool { return false }, want: filepath.Join(ws, "app/x.go")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolvePath(context.Background(), tc.in, wsFn(ws), tc.contested)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("resolvePath(%q) succeeded, want a contested-relative refusal", tc.in)
				}
				msg := strings.ToLower(err.Error())
				if !strings.Contains(msg, "absolute path") || !strings.Contains(msg, "contested") {
					t.Errorf("refusal is not instructive: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolvePath(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("resolvePath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestWriteDeps_ContestedRefusesRelative is the WriteDeps counterpart of
// TestResolvePath_ContestedRefusesRelative — the write tools resolve through the
// deps method, so the seam must refuse there too.
func TestWriteDeps_ContestedRefusesRelative(t *testing.T) {
	ws := "/work/space"
	deps := WriteDeps{WorkspaceFn: wsFn(ws), Contested: contestedTrue}

	if got, err := deps.resolvePath(context.Background(), "app/x.go"); err == nil {
		t.Fatalf("WriteDeps.resolvePath relative on contested succeeded (%q)", got)
	} else if !strings.Contains(strings.ToLower(err.Error()), "absolute path") {
		t.Errorf("refusal is not instructive: %v", err)
	}

	if got, err := deps.resolvePath(context.Background(), "/abs/x.go"); err != nil || got != "/abs/x.go" {
		t.Errorf("absolute path on contested = %q, %v; want %q, nil", got, err, "/abs/x.go")
	}

	// A nil Contested is the zero-value WriteDeps{} contract: relative paths still
	// anchor, so bare test setups are unaffected.
	bare := WriteDeps{WorkspaceFn: wsFn(ws)}
	if got, err := bare.resolvePath(context.Background(), "app/x.go"); err != nil || got != filepath.Join(ws, "app/x.go") {
		t.Errorf("relative on bare WriteDeps = %q, %v; want anchored", got, err)
	}
}
