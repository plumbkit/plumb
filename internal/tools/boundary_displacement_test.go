package tools

// boundary_displacement_test.go — what a boundary error tells an agent whose
// workspace was taken out from under it.
//
// The case: two agents multiplexed one `plumb serve` without declaring a
// session_id, so plumb could not keep their pins apart. One force-re-pinned the
// connection to its own project. The other's next call was refused with
// "this connection is pinned to <a project it never named>" — no indication a
// peer had moved it — and the incident was diagnosed as a daemon-restart bug
// for a day. These pin the sentences that make it self-explaining instead.

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPinProvenance_ForcedRendersInString(t *testing.T) {
	set2mAgo := time.Now().Add(-2*time.Minute - 5*time.Second)
	tests := []struct {
		name string
		prov PinProvenance
		want string
	}{
		{
			name: "forced over an explicit pin",
			prov: PinProvenance{Source: "session_start", At: set2mAgo, Previous: "/x/cvex", Forced: true},
			want: "Pin provenance: set 2m ago via session_start, forced over an explicit pin (previously /x/cvex).",
		},
		{
			// force: true on a FIRST attach overrode nothing. Rendering it would
			// invent a victim, and an operator reading "forced" would go looking
			// for a displacement that never happened.
			name: "forced with no previous root renders nothing extra",
			prov: PinProvenance{Source: "session_start", At: set2mAgo, Forced: true},
			want: "Pin provenance: set 2m ago via session_start.",
		},
		{
			name: "unforced is unchanged",
			prov: PinProvenance{Source: "session_start", At: set2mAgo, Previous: "/x/cvex"},
			want: "Pin provenance: set 2m ago via session_start (previously /x/cvex).",
		},
		{
			// Contested changes the REMEDY, not the description of the pin. It
			// must not leak into the provenance sentence, which is spliced into
			// daemon_info as well.
			name: "contested does not alter the provenance sentence",
			prov: PinProvenance{Source: "session_start", At: set2mAgo, Previous: "/x/cvex", Contested: true},
			want: "Pin provenance: set 2m ago via session_start (previously /x/cvex).",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.prov.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestDisplacementNotice_OnlyForTheDisplacedProject: the notice fires for a path
// belonging to the project the connection was moved OFF, and stays silent
// otherwise. A blanket notice on every boundary error would be noise on the far
// more common case of an agent simply naming the wrong path.
func TestDisplacementNotice_OnlyForTheDisplacedProject(t *testing.T) {
	displaced := t.TempDir()
	unrelated := t.TempDir()
	forced := PinProvenance{
		Source:   "session_start",
		At:       time.Now().Add(-45 * time.Second),
		Previous: displaced,
		Forced:   true,
	}

	tests := []struct {
		name string
		prov PinProvenance
		path string
		want bool
	}{
		{
			name: "a file in the displaced project",
			prov: forced, path: filepath.Join(displaced, "main.go"), want: true,
		},
		{
			name: "a file nested deep in the displaced project",
			prov: forced, path: filepath.Join(displaced, "a", "b", "c.go"), want: true,
		},
		{
			name: "the displaced root itself",
			prov: forced, path: displaced, want: true,
		},
		{
			name: "a file in some unrelated project",
			prov: forced, path: filepath.Join(unrelated, "main.go"), want: false,
		},
		{
			// The ordinary case by a wide margin: the pin moved because one agent
			// switched projects. Nobody was displaced, so nobody is told they were.
			name: "an unforced pin move is not a displacement",
			prov: PinProvenance{Source: "session_start", At: time.Now(), Previous: displaced},
			path: filepath.Join(displaced, "main.go"), want: false,
		},
		{
			name: "forced with no previous root",
			prov: PinProvenance{Source: "session_start", At: time.Now(), Forced: true},
			path: filepath.Join(displaced, "main.go"), want: false,
		},
		{
			name: "no path to judge",
			prov: forced, path: "", want: false,
		},
		{
			name: "zero provenance",
			prov: PinProvenance{}, path: filepath.Join(displaced, "main.go"), want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.prov.DisplacementNotice(tt.path)
			if (got != "") != tt.want {
				t.Fatalf("DisplacementNotice(%q) = %q, want present=%v", tt.path, got, tt.want)
			}
			if !tt.want {
				return
			}
			// When it does fire it must name the displaced root, when it happened,
			// and the fix — those three are what turn the refusal into a diagnosis.
			for _, frag := range []string{"force-re-pinned away from " + displaced, "45s ago", "session_start.session_id"} {
				if !strings.Contains(got, frag) {
					t.Errorf("notice is missing %q:\n%s", frag, got)
				}
			}
		})
	}
}

// TestWorkspaceBoundaryError_ContestedSwapsTheAdvice: the parenthetical that
// tells a refused caller what to do next is the sentence both agents in the
// incident were following. On a contested connection it must name what actually
// separates them, and must not lead with force.
func TestWorkspaceBoundaryError_ContestedSwapsTheAdvice(t *testing.T) {
	ordinary := WorkspaceBoundaryError{Workspace: "/w", Path: "/other"}.Error()
	if !strings.Contains(ordinary, "retry with force: true") {
		t.Fatalf("the ordinary advice changed; a lone agent must still be told how to switch projects:\n%s", ordinary)
	}

	contested := WorkspaceBoundaryError{
		Workspace:  "/w",
		Path:       "/other",
		Provenance: PinProvenance{Source: "session_start", At: time.Now(), Contested: true},
	}.Error()
	if strings.Contains(contested, "retry with force: true") {
		t.Errorf("contested error still leads with force: true:\n%s", contested)
	}
	for _, frag := range []string{
		"identify yourself with session_start.session_id",
		"one `plumb serve` per agent",
		"use force: true only if",
	} {
		if !strings.Contains(contested, frag) {
			t.Errorf("contested advice is missing %q:\n%s", frag, contested)
		}
	}
	// The refusal itself is unchanged: this changes advice, never permission.
	if !strings.Contains(contested, "workspace boundary violation: this connection is pinned to /w") {
		t.Errorf("the refusal sentence was disturbed:\n%s", contested)
	}
}

// TestWorkspaceBoundaryError_ReadOnlyRootIgnoresDisplacement: a read-only-root
// denial is about dependency source, not about who holds the pin. It never
// carried provenance and must not start carrying a displacement notice.
func TestWorkspaceBoundaryError_ReadOnlyRootIgnoresDisplacement(t *testing.T) {
	displaced := t.TempDir()
	msg := WorkspaceBoundaryError{
		Workspace:    "/w",
		Path:         filepath.Join(displaced, "dep.go"),
		ReadOnlyRoot: "/deps",
		Provenance:   PinProvenance{Source: "session_start", Previous: displaced, Forced: true, Contested: true},
	}.Error()
	if strings.Contains(msg, "force-re-pinned away from") {
		t.Errorf("a read-only-root denial grew a displacement notice:\n%s", msg)
	}
	if !strings.Contains(msg, "is under a read-only root") {
		t.Errorf("the read-only sentence was disturbed:\n%s", msg)
	}
}
