package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/memory"
	"github.com/plumbkit/plumb/internal/redact"
	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/stats"
)

// episodicWaitDeadline bounds the polls that wait for an episodic summary to land
// via the asynchronous stats Writer (a ~200ms batched flush). It is deliberately
// generous: a healthy run satisfies the poll in well under a second, so a large
// ceiling never slows the suite — it only stops a starved shared CI runner (the
// whole `go test ./...` matrix competing for CPU) from tripping the deadline and
// flaking. See plumbkit/plumb#80.
const episodicWaitDeadline = 30 * time.Second

func TestBuildEpisodic(t *testing.T) {
	calls := []stats.Call{
		{Tool: "edit_file", InputJSON: `{"file_path":"/ws/internal/a.go"}`},
		{Tool: "edit_file", InputJSON: `{"file_path":"/ws/internal/b.go"}`},
		{Tool: "find_references", InputJSON: `{"name":"UserSession"}`},
		{Tool: "read_file", InputJSON: `{"file_path":"/ws/c.go"}`},
		{Tool: "read_file", InputJSON: `{}`},
	}
	d := buildEpisodicDetail(calls, "/ws")
	if d.WriteN != 2 {
		t.Errorf("WriteN = %d, want 2", d.WriteN)
	}
	if d.ReadN != 3 {
		t.Errorf("ReadN = %d, want 3", d.ReadN)
	}
	if len(d.Touched) != 2 {
		t.Errorf("Touched = %v, want [a.go b.go]", d.Touched)
	}
	for _, want := range []string{"modified", "a.go", "b.go", "UserSession"} {
		if !strings.Contains(d.Summary, want) {
			t.Errorf("summary missing %q: %s", want, d.Summary)
		}
	}
	// Source paths are workspace-relative; the in-workspace writes contribute, the
	// read does not.
	if len(d.SourcePaths) != 2 || d.SourcePaths[0] != "internal/a.go" || d.SourcePaths[1] != "internal/b.go" {
		t.Errorf("SourcePaths = %v, want [internal/a.go internal/b.go]", d.SourcePaths)
	}
}

func TestBuildEpisodic_EmptyWhenNoActivity(t *testing.T) {
	if d := buildEpisodicDetail(nil, ""); d.Summary != "" {
		t.Errorf("empty calls should yield empty summary, got %q", d.Summary)
	}
}

// TestBuildEpisodic_RedactionComposes proves the pipeline scrubs a secret: a
// symbol name carrying a token must not survive redaction of the summary.
func TestBuildEpisodic_RedactionComposes(t *testing.T) {
	calls := []stats.Call{
		{Tool: "find_symbol", InputJSON: `{"name":"ghp_0123456789abcdefghijklmnopqrstuvwxyz1"}`},
		{Tool: "edit_file", InputJSON: `{"file_path":"/ws/a.go"}`},
	}
	summary := buildEpisodicDetail(calls, "").Summary
	cleaned, n := redact.Redact(summary)
	if n == 0 || strings.Contains(cleaned, "ghp_0123456789abcdefghijklmnopqrstuvwxyz1") {
		t.Errorf("secret survived redaction: %s", cleaned)
	}
}

// TestBuildEpisodic_TransactionAndFindReplace: transaction_apply paths are
// nested under operations[], and find_replace is a write only when dry_run=false.
func TestBuildEpisodic_TransactionAndFindReplace(t *testing.T) {
	calls := []stats.Call{
		{Tool: "transaction_apply", InputJSON: `{"operations":[{"path":"/ws/a.go"},{"from":"/ws/b.go","to":"/ws/c.go"}]}`},
		{Tool: "find_replace", InputJSON: `{"file_path":"/ws/default-dry.go"}`},             // default dry-run → read
		{Tool: "find_replace", InputJSON: `{"file_path":"/ws/dry.go","dry_run":true}`},      // explicit dry-run → read
		{Tool: "find_replace", InputJSON: `{"file_path":"/ws/applied.go","dry_run":false}`}, // applied → write
	}
	d := buildEpisodicDetail(calls, "")
	summary, touched := d.Summary, d.Touched
	if d.WriteN != 2 {
		t.Errorf("WriteN = %d, want 2 (transaction_apply + applied find_replace)", d.WriteN)
	}
	if d.ReadN != 2 {
		t.Errorf("ReadN = %d, want 2 (default + explicit dry-run find_replace)", d.ReadN)
	}
	want := map[string]bool{"a.go": true, "b.go": true, "c.go": true, "applied.go": true}
	for _, f := range touched {
		if !want[f] {
			t.Errorf("unexpected touched file %q", f)
		}
		delete(want, f)
	}
	if len(want) > 0 {
		t.Errorf("missing touched files: %v (got %v)", want, touched)
	}
	for _, name := range []string{"default-dry.go", "dry.go"} {
		if strings.Contains(summary, name) {
			t.Errorf("a dry-run find_replace must not contribute touched file %s: %s", name, summary)
		}
	}
}

// TestEvictIdle_SummarisesBeforeCancel: an idle session past the eviction TTL is
// summarised (once) before its connection is cancelled — so a short eviction TTL
// never robs a session of its episodic summary.
func TestEvictIdle_SummarisesBeforeCancel(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	sessID, err := session.Register(session.Info{Name: "evict-test", DaemonVersion: "test"})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	dir, err := session.Dir()
	if err != nil {
		t.Fatalf("session.Dir: %v", err)
	}
	old := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(filepath.Join(dir, sessID+".json"), old, old); err != nil {
		t.Fatalf("age session: %v", err)
	}

	var summarised, cancelled int32
	reg := newConnRegistry()
	reg.add(sessID, connHandle{
		summarise: func() { atomic.AddInt32(&summarised, 1) },
		cancel:    func() { atomic.AddInt32(&cancelled, 1) },
	})

	reg.evictIdle(1 * time.Minute) // idle 2min > 1min ttl
	deadline := time.After(episodicWaitDeadline)
	for atomic.LoadInt32(&summarised) == 0 {
		select {
		case <-deadline:
			t.Fatal("evictIdle did not summarise the session before eviction")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if atomic.LoadInt32(&cancelled) != 1 {
		t.Errorf("expected the session to be cancelled once, got %d", cancelled)
	}
	// A second eviction pass must not re-summarise (dedup).
	reg.evictIdle(1 * time.Minute)
	time.Sleep(50 * time.Millisecond)
	if n := atomic.LoadInt32(&summarised); n != 1 {
		t.Errorf("expected exactly one summarise, got %d", n)
	}
}

// TestRunIdleReaper_SummarisesWhenGlobalSummariesOff: the reaper fires the
// per-session summarise closure even when the GLOBAL generated_summaries is off,
// so a per-project episodic opt-in (re-checked inside the closure) is reachable.
func TestRunIdleReaper_SummarisesWhenGlobalSummariesOff(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	sessID, err := session.Register(session.Info{Name: "reaper-test", DaemonVersion: "test"})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	dir, _ := session.Dir()
	old := time.Now().Add(-40 * time.Minute) // past the default 30min idle threshold
	if err := os.Chtimes(filepath.Join(dir, sessID+".json"), old, old); err != nil {
		t.Fatalf("age session: %v", err)
	}

	cfg := config.Defaults()
	cfg.Memory.GeneratedSummaries = false // GLOBAL off
	cfg.Session.EvictionTTLMinutes = 0    // disable eviction to isolate the summarise path
	store := config.NewStore(cfg)

	var summarised int32
	reg := newConnRegistry()
	reg.add(sessID, connHandle{summarise: func() { atomic.AddInt32(&summarised, 1) }})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticks := make(chan time.Time)
	go runIdleReaper(ctx, store, reg, nil, ticks)
	ticks <- time.Now()

	deadline := time.After(episodicWaitDeadline)
	for atomic.LoadInt32(&summarised) == 0 {
		select {
		case <-deadline:
			t.Fatal("reaper did not summarise an idle session with global summaries off (per-project opt-in unreachable)")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestClampBytes(t *testing.T) {
	if got := clampBytes("short", 0); got != "short" {
		t.Errorf("zero budget should be a no-op, got %q", got)
	}
	if got := clampBytes("short", 100); got != "short" {
		t.Errorf("fits-in-budget should be a no-op, got %q", got)
	}
	if got := clampBytes("hello world", 5); got != "he…" {
		t.Errorf("clampBytes(hello world, 5) = %q, want \"he…\" (5 bytes)", got)
	}
	// Multi-byte: every result must stay within the BYTE budget and be valid UTF-8.
	for _, c := range []struct {
		in     string
		budget int
	}{
		{"日本語テスト", 9}, // 3 bytes/char
		{"😀😀😀😀", 7},   // 4 bytes/char
		{"abc", 2},    // budget below the ellipsis width
		{"héllo wörld", 6},
	} {
		got := clampBytes(c.in, c.budget)
		if len(got) > c.budget {
			t.Errorf("clampBytes(%q, %d) = %q is %d bytes, over budget", c.in, c.budget, got, len(got))
		}
		if !utf8.ValidString(got) {
			t.Errorf("clampBytes(%q, %d) = %q is not valid UTF-8", c.in, c.budget, got)
		}
	}
}

// TestGenerateEpisodicSummary_Integration exercises the full connSession path:
// seed a session's tool_calls → generateEpisodicSummary reads them, builds a
// summary, redacts it, and writes it via the stats Writer → read it back through
// the same LatestEpisodic accessor session_start uses — and also writes the
// durable episodic-* markdown memory with generated provenance.
func TestGenerateEpisodicSummary_Integration(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ws := t.TempDir()

	store := config.NewStore(config.Defaults()) // Memory.GeneratedSummaries defaults true
	ss := newStatsStore()
	defer ss.Close()

	s := newConnSession(context.Background(), detectTestPool(), nil, store, ss, nil, newSharedBudgets())
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
		{Workspace: ws, SessionID: s.sessID, Tool: "edit_file", CalledAt: now, InputJSON: fmt.Sprintf(`{"file_path":%q}`, filepath.Join(ws, "auth.go")), Success: true},
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
	deadline := time.After(episodicWaitDeadline)
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
	assertGeneratedEpisodicMemory(t, ws, s.sessID)
}

// assertGeneratedEpisodicMemory checks the durable episodic-* markdown memory
// written alongside the stats row: attached to the touched file, marked
// generated, and provenance-stamped.
func assertGeneratedEpisodicMemory(t *testing.T, ws, sessID string) {
	t.Helper()
	mems, err := memory.List(ws)
	if err != nil {
		t.Fatalf("memory.List: %v", err)
	}
	if len(mems) != 1 || !strings.HasPrefix(mems[0].Name, "episodic-") {
		t.Fatalf("expected one generated episodic memory, got %+v", mems)
	}
	if !mems[0].MatchesPath("auth.go") {
		t.Fatalf("generated memory should attach to auth.go, paths=%v", mems[0].Paths)
	}
	if mems[0].UserAuthored() {
		t.Errorf("generated episodic memory must not read as user-authored, confidence=%q", mems[0].Confidence)
	}
	rec, err := memory.ReadMeta(ws, mems[0].Name)
	if err != nil {
		t.Fatalf("ReadMeta: %v", err)
	}
	if rec.Confidence != memory.ConfidenceGenerated {
		t.Errorf("confidence = %q, want generated", rec.Confidence)
	}
	if rec.SourceSession != sessID {
		t.Errorf("source_session = %q, want %q", rec.SourceSession, sessID)
	}
	if rec.CreatedAt.IsZero() {
		t.Error("created_at must be set so retention pruning orders correctly")
	}
}

// TestEpisodicRelPath pins the source-path derivation: workspace-relative slash
// paths in, anything outside the workspace (or escaping it) dropped.
func TestEpisodicRelPath(t *testing.T) {
	ws := "/ws"
	cases := map[string]string{
		"/ws/internal/a.go":        "internal/a.go",
		"file:///ws/internal/a.go": "internal/a.go", // rename_symbol reports file:// URIs
		"internal/a.go":            "internal/a.go", // already relative
		"/other/x.go":              "",              // outside the workspace
		"../escape.go":             "",              // relative escape
		"/ws":                      "",              // the root itself is not a file
		"":                         "",
	}
	for in, want := range cases {
		if got := episodicRelPath(ws, in); got != want {
			t.Errorf("episodicRelPath(%q, %q) = %q, want %q", ws, in, got, want)
		}
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
	deadline := time.After(episodicWaitDeadline)
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
