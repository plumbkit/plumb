package cli

// conn_pinprovenance_test.go — pin-drift observability (issue #182): the
// source/trigger fields on the re-pin log, the provenance stamped on the
// session view, and its arrival in boundary errors. The confirmation grep the
// issue depends on is `grep -E 'session re-pinned|roots changed'`, so these
// assert the appended fields without ever touching the protected message text.

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/sessionstate"
	"github.com/plumbkit/plumb/internal/tools"
)

// captureLog points the session's logger at a buffer. connSession.log() returns
// the field when set, so no production hook is required.
func captureLog(s *connSession) *bytes.Buffer {
	var buf bytes.Buffer
	s.logger = slog.New(slog.NewTextHandler(&buf, nil))
	return &buf
}

func TestRepinLog_CarriesSourceAndTrigger(t *testing.T) {
	store, ss := newOriginStore(t)
	rootA, rootB := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, rootA)
	mustGitDir(t, rootB)

	s := newPersistSession(t, store, ss, "proxyX")
	s.attachWorkspace(context.Background(), "file://"+rootA)
	buf := captureLog(s)

	if _, err := s.repinWorkspace(context.Background(), rootB, ""); err != nil {
		t.Fatalf("repinWorkspace: %v", err)
	}
	for _, want := range []string{"session re-pinned", "source=session_start", "trigger=live"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("re-pin log missing %q:\n%s", want, buf.String())
		}
	}
}

func TestRepinLog_RootsChangedSource(t *testing.T) {
	store, ss := newOriginStore(t)
	rootA, rootB := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, rootA)
	mustGitDir(t, rootB)

	s := newPersistSession(t, store, ss, "proxyX")
	s.attachWorkspace(context.Background(), "file://"+rootA)
	buf := captureLog(s)

	s.onRootsChanged(context.Background(), []string{"file://" + rootB})
	for _, want := range []string{"session re-pinned", "source=roots", "trigger=live"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("roots-changed re-pin log missing %q:\n%s", want, buf.String())
		}
	}
}

func TestRepinLog_RestoreTrigger(t *testing.T) {
	// A daemon restart replays the persisted session_start pin through the OnInit
	// ladder. The origin is the same as a live call's — only the trigger tells a
	// reader the caller did not act — so the restore path must not log as live.
	store, ss := newOriginStore(t)
	root := freshTempDir(t)
	mustGitDir(t, root)

	calls := 0
	before := newPersistSession(t, store, ss, "proxyX")
	if _, err := before.repinWorkspace(context.Background(), root, ""); err != nil {
		t.Fatalf("repinWorkspace: %v", err)
	}
	before.close()

	after := newPersistSession(t, store, ss, "proxyX")
	buf := captureLog(after)
	after.attachOnInit(context.Background(), rootsReplying(root, &calls))

	for _, want := range []string{"session re-pinned", "source=session_start", "trigger=restore"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("restore re-pin log missing %q:\n%s", want, buf.String())
		}
	}
	if calls != 0 {
		t.Errorf("roots/list was asked %d time(s); the persisted session_start pin must bypass the roots rung", calls)
	}
}

func TestPinProvenance_UnknownOriginSuppressed(t *testing.T) {
	// An incidental auto-attach (tool-path seed, cwd hint, synthetic root) has no
	// provenance worth telling an agent about — "via unknown" is noise. The log's
	// source field keeps the label; the agent-facing provenance stays zero.
	store, ss := newOriginStore(t)
	root := freshTempDir(t)
	mustGitDir(t, root)

	s := newPersistSession(t, store, ss, "proxyX")
	s.attachWorkspacePin(context.Background(), "file://"+root, sessionstate.PinSourceUnknown)
	if got := s.pinProvenance(); got != (tools.PinProvenance{}) {
		t.Errorf("pinProvenance() = %+v, want the zero value for an unknown origin", got)
	}
}

func TestPinProvenance_RecordedOnRepin(t *testing.T) {
	store, ss := newOriginStore(t)
	rootA, rootB := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, rootA)
	mustGitDir(t, rootB)

	s := newPersistSession(t, store, ss, "proxyX")
	s.attachWorkspace(context.Background(), "file://"+rootA)
	if _, err := s.repinWorkspace(context.Background(), rootB, ""); err != nil {
		t.Fatalf("repinWorkspace: %v", err)
	}

	prov := s.pinProvenance()
	if prov.Source != "session_start" {
		t.Errorf("Source = %q, want session_start", prov.Source)
	}
	if prov.At.IsZero() {
		t.Error("At is zero; the re-pin must stamp its time")
	}
	if prov.Previous != rootA {
		t.Errorf("Previous = %q, want %q", prov.Previous, rootA)
	}
}

func TestBoundaryError_CarriesPinProvenance(t *testing.T) {
	// The drifted peer's first symptom is a boundary refusal; that refusal must
	// say who moved the pin and from where. Never assert the age — it reads the
	// wall clock.
	store, ss := newOriginStore(t)
	rootA, rootB, rootC := freshTempDir(t), freshTempDir(t), freshTempDir(t)
	mustGitDir(t, rootA)
	mustGitDir(t, rootB)

	s := newPersistSession(t, store, ss, "proxyX")
	s.attachWorkspace(context.Background(), "file://"+rootA)
	if _, err := s.repinWorkspace(context.Background(), rootB, ""); err != nil {
		t.Fatalf("repinWorkspace: %v", err)
	}

	err := s.checkBoundary(filepath.Join(rootC, "x.go"), tools.AccessRead)
	if err == nil {
		t.Fatal("out-of-tree path must be refused")
	}
	for _, want := range []string{"workspace boundary violation", "via session_start", "previously " + rootA} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("boundary error missing %q:\n%s", want, err.Error())
		}
	}
}

func TestPinProvenance_SameRootPromotionUpgradesViaOnly(t *testing.T) {
	// A session_start naming the already-attached root promotes the recorded
	// origin without moving the pin: the label upgrades, Previous stays empty.
	store, ss := newOriginStore(t)
	root := freshTempDir(t)
	mustGitDir(t, root)

	s := newPersistSession(t, store, ss, "proxyX")
	s.attachWorkspace(context.Background(), "file://"+root)
	if got := s.pinProvenance().Source; got != "roots" {
		t.Fatalf("precondition: Source = %q, want roots", got)
	}

	if _, err := s.repinWorkspace(context.Background(), root, ""); err != nil {
		t.Fatalf("repinWorkspace: %v", err)
	}
	prov := s.pinProvenance()
	if prov.Source != "session_start" {
		t.Errorf("Source = %q, want session_start after promotion", prov.Source)
	}
	if prov.Previous != "" {
		t.Errorf("Previous = %q, want empty — the root did not move", prov.Previous)
	}
}

func TestBoundedForLog(t *testing.T) {
	in := []string{"a", "b", "c", "d"}
	cases := []struct {
		name  string
		limit int
		want  []string
	}{
		{"zero limit returns all", 0, []string{"a", "b", "c", "d"}},
		{"limit at length returns all", 4, []string{"a", "b", "c", "d"}},
		{"limit above length returns all", 9, []string{"a", "b", "c", "d"}},
		{"limit below length appends sentinel", 2, []string{"a", "b", "+2 more"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := boundedForLog(in, tc.limit)
			if !slices.Equal(got, tc.want) {
				t.Errorf("boundedForLog(%v, %d) = %v, want %v", in, tc.limit, got, tc.want)
			}
		})
	}
	if !slices.Equal(in, []string{"a", "b", "c", "d"}) {
		t.Errorf("input slice mutated: %v", in)
	}
}
