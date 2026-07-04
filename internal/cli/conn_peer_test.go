package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
)

// TestApplyProjectConfig_SnapshotsCollab asserts the [collab] block is captured
// into the session view by applyProjectConfig, so collabConfig() (read on the hot
// peer-hint path) returns the project-resolved value with no per-call disk read.
func TestApplyProjectConfig_SnapshotsCollab(t *testing.T) {
	ws := t.TempDir()
	plumbDir := filepath.Join(ws, ".plumb")
	if err := os.MkdirAll(plumbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plumbDir, "config.toml"),
		[]byte("[collab]\npeer_awareness = false\nhint_budget_bytes = 64\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := config.NewStore(config.Defaults()) // global: peer_awareness on, budget 512
	s := &connSession{store: store}
	s.mutate(func(v *sessionView) { v.acquiredRoot = ws })

	s.applyProjectConfig(ws)

	cc := s.collabConfig()
	if cc.PeerAwareness {
		t.Error("project peer_awareness=false not snapshotted into the view")
	}
	if cc.HintBudgetBytes != 64 {
		t.Errorf("hint_budget_bytes = %d, want 64", cc.HintBudgetBytes)
	}
}

// seedPeerWrite prepares a connSession whose peer-write cache is pre-populated
// (bypassing the session/stats scan) so peerHint's pure gating/formatting can be
// tested hermetically.
func seedPeerWrite(t *testing.T, ws string, cc config.CollabConfig, idle int, writeAge time.Duration) *connSession {
	t.Helper()
	s := &connSession{store: config.NewStore(config.Defaults()), peerWrites: &peerWriteCache{}, sessID: "self"}
	s.mutate(func(v *sessionView) {
		v.acquiredRoot = ws
		v.collab = cc
		v.session = config.SessionConfig{IdleThresholdMinutes: idle}
	})
	now := time.Now()
	abs := filepath.Join(ws, "a.go")
	s.peerWrites.ws = ws
	s.peerWrites.selfID = "self"
	s.peerWrites.builtAt = now
	s.peerWrites.byAbsPath = map[string]peerWrite{
		abs: {session: "swift-falcon", at: now.Add(-writeAge)},
	}
	return s
}

func TestPeerHint_HappyPath(t *testing.T) {
	ws := t.TempDir()
	s := seedPeerWrite(t, ws, config.CollabConfig{PeerAwareness: true, HintBudgetBytes: 512}, 30, 3*time.Minute)
	args := []byte(`{"file_path":"` + filepath.Join(ws, "a.go") + `"}`)
	got := s.peerHint(args, ws)
	if !strings.Contains(got, "swift-falcon") || !strings.Contains(got, "[Peer:") {
		t.Errorf("expected a peer hint naming the peer, got %q", got)
	}
}

func TestPeerHint_DisabledCleanly(t *testing.T) {
	ws := t.TempDir()
	s := seedPeerWrite(t, ws, config.CollabConfig{PeerAwareness: false, HintBudgetBytes: 512}, 30, 3*time.Minute)
	args := []byte(`{"file_path":"` + filepath.Join(ws, "a.go") + `"}`)
	if got := s.peerHint(args, ws); got != "" {
		t.Errorf("peer_awareness=false must suppress the hint, got %q", got)
	}
}

func TestPeerHint_OutsideRecencyWindow(t *testing.T) {
	ws := t.TempDir()
	// Write 40 min ago; window = min(idle=30, 30m) = 30m ⇒ suppressed.
	s := seedPeerWrite(t, ws, config.CollabConfig{PeerAwareness: true, HintBudgetBytes: 512}, 30, 40*time.Minute)
	args := []byte(`{"file_path":"` + filepath.Join(ws, "a.go") + `"}`)
	if got := s.peerHint(args, ws); got != "" {
		t.Errorf("a write older than the recency window must not hint, got %q", got)
	}
}

func TestPeerHint_BudgetCap(t *testing.T) {
	ws := t.TempDir()
	s := seedPeerWrite(t, ws, config.CollabConfig{PeerAwareness: true, HintBudgetBytes: 30}, 30, 3*time.Minute)
	args := []byte(`{"file_path":"` + filepath.Join(ws, "a.go") + `"}`)
	got := s.peerHint(args, ws)
	if got == "" {
		t.Fatal("expected a (clamped) hint")
	}
	if len([]byte(got)) > 30 {
		t.Errorf("peer hint %q exceeds the 30-byte budget (%d bytes)", got, len(got))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("clamped hint should end with an ellipsis, got %q", got)
	}
}

func TestPeerHint_NoPathArg(t *testing.T) {
	ws := t.TempDir()
	s := seedPeerWrite(t, ws, config.CollabConfig{PeerAwareness: true, HintBudgetBytes: 512}, 30, 3*time.Minute)
	if got := s.peerHint([]byte(`{"query":"x"}`), ws); got != "" {
		t.Errorf("no path arg must yield no hint, got %q", got)
	}
}

func TestPeerRecencyWindow(t *testing.T) {
	cases := []struct {
		idle int
		want time.Duration
	}{
		{10, 10 * time.Minute},
		{60, 30 * time.Minute}, // capped at peerHintMaxWindow
		{0, 30 * time.Minute},  // non-positive falls back to the 30-min default
	}
	for _, tc := range cases {
		s := &connSession{store: config.NewStore(config.Defaults())}
		s.mutate(func(v *sessionView) { v.session = config.SessionConfig{IdleThresholdMinutes: tc.idle} })
		if got := s.peerRecencyWindow(); got != tc.want {
			t.Errorf("idle=%d ⇒ window %s, want %s", tc.idle, got, tc.want)
		}
	}
}
