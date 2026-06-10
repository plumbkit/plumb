package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/redact"
	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/stats"
)

func TestBuildEpisodic(t *testing.T) {
	calls := []stats.Call{
		{Tool: "edit_file", InputJSON: `{"file_path":"/ws/internal/a.go"}`},
		{Tool: "edit_file", InputJSON: `{"file_path":"/ws/internal/b.go"}`},
		{Tool: "find_references", InputJSON: `{"name":"UserSession"}`},
		{Tool: "read_file", InputJSON: `{"file_path":"/ws/c.go"}`},
		{Tool: "read_file", InputJSON: `{}`},
	}
	summary, touched, readN, writeN := buildEpisodic(calls)
	if writeN != 2 {
		t.Errorf("writeN = %d, want 2", writeN)
	}
	if readN != 3 {
		t.Errorf("readN = %d, want 3", readN)
	}
	if len(touched) != 2 {
		t.Errorf("touched = %v, want [a.go b.go]", touched)
	}
	for _, want := range []string{"modified", "a.go", "b.go", "UserSession"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary missing %q: %s", want, summary)
		}
	}
}

func TestBuildEpisodic_EmptyWhenNoActivity(t *testing.T) {
	if s, _, _, _ := buildEpisodic(nil); s != "" {
		t.Errorf("empty calls should yield empty summary, got %q", s)
	}
}

// TestBuildEpisodic_RedactionComposes proves the pipeline scrubs a secret: a
// symbol name carrying a token must not survive redaction of the summary.
func TestBuildEpisodic_RedactionComposes(t *testing.T) {
	calls := []stats.Call{
		{Tool: "find_symbol", InputJSON: `{"name":"ghp_0123456789abcdefghijklmnopqrstuvwxyz1"}`},
		{Tool: "edit_file", InputJSON: `{"file_path":"/ws/a.go"}`},
	}
	summary, _, _, _ := buildEpisodic(calls)
	cleaned, n := redact.Redact(summary)
	if n == 0 || strings.Contains(cleaned, "ghp_0123456789abcdefghijklmnopqrstuvwxyz1") {
		t.Errorf("secret survived redaction: %s", cleaned)
	}
}

func TestClampRunes(t *testing.T) {
	if got := clampRunes("hello world", 5); got != "hello…" {
		t.Errorf("clampRunes = %q", got)
	}
	if got := clampRunes("short", 0); got != "short" {
		t.Errorf("zero budget should be a no-op, got %q", got)
	}
}

// TestGenerateEpisodicSummary_Integration exercises the full connSession path:
// seed a session's tool_calls → generateEpisodicSummary reads them, builds a
// summary, redacts it, and writes it via the stats Writer → read it back through
// the same LatestEpisodic accessor session_start uses. This is the
// idle→episodic→surface loop minus the reaper trigger (covered separately below).
func TestGenerateEpisodicSummary_Integration(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ws := t.TempDir()

	store := config.NewStore(config.Defaults()) // Memory.GeneratedSummaries defaults true
	ss := newStatsStore()
	defer ss.Close()

	s := newConnSession(context.Background(), detectTestPool(), nil, store, ss, newSharedBudgets())
	defer s.close()
	s.mutate(func(v *sessionView) { v.acquiredRoot = ws })

	// Seed this session's tool_calls via a direct synchronous RW handle, then
	// close it so only the stats Writer touches the DB during generation.
	db, err := stats.Open()
	if err != nil {
		t.Fatalf("stats.Open: %v", err)
	}
	now := time.Now()
	for _, c := range []stats.Call{
		{Workspace: ws, SessionID: s.sessID, Tool: "edit_file", CalledAt: now, InputJSON: `{"file_path":"/p/auth.go"}`, Success: true},
		{Workspace: ws, SessionID: s.sessID, Tool: "find_references", CalledAt: now, InputJSON: `{"name":"UserSession"}`, Success: true},
		{Workspace: ws, SessionID: s.sessID, Tool: "read_file", CalledAt: now, InputJSON: `{}`, Success: true},
	} {
		if err := db.Record(c); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	db.Close()

	s.generateEpisodicSummary()

	// The episodic insert rides the async stats Writer (200ms flush); retry.
	var got stats.Episodic
	deadline := time.After(3 * time.Second)
	for {
		if ro, _ := stats.SharedReadOnly(); ro != nil {
			if e, ok, _ := ro.LatestEpisodic(ws); ok {
				got = e
				break
			}
		}
		select {
		case <-deadline:
			t.Fatal("episodic summary was not written within the deadline")
		case <-time.After(50 * time.Millisecond):
		}
	}
	if !strings.Contains(got.Summary, "auth.go") {
		t.Errorf("summary missing touched file: %q", got.Summary)
	}
	if !strings.Contains(got.Summary, "UserSession") {
		t.Errorf("summary missing symbol: %q", got.Summary)
	}
	if got.WriteCount != 1 || got.ReadCount != 2 {
		t.Errorf("counts: write=%d read=%d (want 1/2)", got.WriteCount, got.ReadCount)
	}
}

// TestSummariseIdle_FiresClosureOncePerSpell covers the reaper trigger: a session
// idle past the threshold has its episodic-summary closure fired exactly once per
// idle spell (re-arming only after new activity).
func TestSummariseIdle_FiresClosureOncePerSpell(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	sessID, err := session.Register(session.Info{Name: "idle-test", DaemonVersion: "test"})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	// Age the session file so List() reports it as idle (LastSeenAt = mtime).
	dir, err := session.Dir()
	if err != nil {
		t.Fatalf("session.Dir: %v", err)
	}
	old := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(filepath.Join(dir, sessID+".json"), old, old); err != nil {
		t.Fatalf("age session file: %v", err)
	}

	var fired int32
	reg := newConnRegistry()
	reg.add(sessID, connHandle{summarise: func() { atomic.AddInt32(&fired, 1) }})

	reg.summariseIdle(1 * time.Minute)
	deadline := time.After(time.Second)
	for atomic.LoadInt32(&fired) == 0 {
		select {
		case <-deadline:
			t.Fatal("summarise closure did not fire for an idle session")
		case <-time.After(10 * time.Millisecond):
		}
	}

	// Second pass within the same idle spell must not re-fire (dedup).
	reg.summariseIdle(1 * time.Minute)
	time.Sleep(100 * time.Millisecond)
	if n := atomic.LoadInt32(&fired); n != 1 {
		t.Errorf("expected exactly one summarise per idle spell, got %d", n)
	}
}
