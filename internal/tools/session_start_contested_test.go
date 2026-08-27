package tools

// session_start_contested_test.go — the contested-connection warning in the
// identity block.
//
// session_start is the one section every re-orienting agent reads. In the
// incident this answers, an agent re-oriented, saw a workspace, and had no way
// to learn that workspace had been taken from it minutes earlier — so the
// warning belongs here and not only in daemon_info.

import (
	"context"
	"strings"
	"testing"
)

func newContestedStart(t *testing.T, ws string, prov PinProvenance) *SessionStart {
	t.Helper()
	return NewSessionStart(
		func(context.Context) string { return ws },
		&stubDiagnostics{all: nil}, nil, nil,
		func() string { return "" }, nil,
	).WithPinProvenance(func() PinProvenance { return prov })
}

const contestedNoteFragment = "workspace pin has been force-taken between projects more than once"

func TestSessionStart_ContestedNote(t *testing.T) {
	ws := t.TempDir()

	tests := []struct {
		name string
		tool *SessionStart
		want bool
	}{
		{
			name: "contested connection is announced",
			tool: newContestedStart(t, ws, PinProvenance{Source: "session_start", Contested: true}),
			want: true,
		},
		{
			name: "an ordinary pin says nothing",
			tool: newContestedStart(t, ws, PinProvenance{Source: "session_start"}),
			want: false,
		},
		{
			name: "no provenance accessor wired",
			tool: NewSessionStart(func(context.Context) string { return ws },
				&stubDiagnostics{all: nil}, nil, nil, func() string { return "" }, nil),
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := tt.tool.Execute(context.Background(), []byte(`{}`))
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if got := strings.Contains(out, contestedNoteFragment); got != tt.want {
				t.Errorf("contested note present = %v, want %v\n%s", got, tt.want, out)
			}
		})
	}
}

// TestSessionStart_ContestedNoteNotGatedOnLinked: the warning is a property of
// the CONNECTION, not of the caller. An agent that did pass a session_id is the
// one best placed to notice its peers have not — gating the note on `linked`
// would hide it from exactly the session most able to act on it.
func TestSessionStart_ContestedNoteNotGatedOnLinked(t *testing.T) {
	ws := t.TempDir()
	tool := newContestedStart(t, ws, PinProvenance{Source: "session_start", Contested: true}).
		WithExternalID(func(string) string { return "" })

	out, err := tool.Execute(context.Background(), []byte(`{"session_id":"agent-a"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, contestedNoteFragment) {
		t.Errorf("a linked session was not told its connection is contested:\n%s", out)
	}
	if !strings.Contains(out, "session_start.session_id") {
		t.Errorf("the note does not name the fix:\n%s", out)
	}
}

// TestSessionStart_ContestedNoteInBrief: the brief packet is what a woken or
// resumed session gets by default, and a resumed session is precisely the one
// whose workspace may have moved while it was idle. Dropping the warning there
// would lose it for the case that needs it most.
func TestSessionStart_ContestedNoteInBrief(t *testing.T) {
	ws := t.TempDir()
	tool := newContestedStart(t, ws, PinProvenance{Source: "session_start", Contested: true})

	out, err := tool.Execute(context.Background(), []byte(`{"detail":"brief"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, contestedNoteFragment) {
		t.Errorf("the brief packet dropped the contested-connection warning:\n%s", out)
	}
}
