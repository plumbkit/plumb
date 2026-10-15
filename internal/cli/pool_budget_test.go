package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/cache"
	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/tools"
)

// These tests pin PLAN-534: admission is synchronous, an idle unpinned server
// is evicted before a pinned one, a wake joins the cap, a busy language is
// admitted over its cap and reported, long-hibernated entries are trimmed, and
// the pool's budget is visible.

func TestOverBudgetVictim_UnpinnedBeforePinned(t *testing.T) {
	p := hibernatePool("java", time.Hour, 2)
	installEntryLang(p, "/pinned", "java", &stubClient{})
	installEntryLang(p, "/free", "java", &stubClient{})
	pinned, free := p.lookup("/pinned", "java"), p.lookup("/free", "java")
	now := time.Now()
	pinned.proxy.lastUsed.Store(now.Add(-time.Hour).UnixNano()) // older, but leased
	pinned.refs = 1
	free.proxy.lastUsed.Store(now.UnixNano())
	if v := p.overBudgetVictimLocked("java", 2, nil); v != free {
		t.Fatalf("victim = %v, want the unpinned /free even though /pinned is older", v)
	}
	free.proxy.inflight.Add(1) // busy: never the victim
	if v := p.overBudgetVictimLocked("java", 2, nil); v != pinned {
		t.Fatalf("victim = %v, want the idle pinned /pinned once /free is busy", v)
	}
	if v := p.overBudgetVictimLocked("java", 2, pinned); v != nil {
		t.Fatalf("victim = %v, want none: excluding the entry being woken leaves one running under the cap", v)
	}
}

// TestAdmitLocked_FreesTheSlotUnderTheLock: the victim is already hibernating
// when admitLocked returns — before its teardown runs.
func TestAdmitLocked_FreesTheSlotUnderTheLock(t *testing.T) {
	p := hibernatePool("java", time.Hour, 1)
	installEntryLang(p, "/a", "java", &stubClient{})
	a := p.lookup("/a", "java")
	p.mu.Lock()
	teardown := p.admitLocked("java", 1, nil)
	state := a.state
	p.mu.Unlock()
	if teardown == nil || state != poolHibernating {
		t.Fatalf("admission left the victim %s (teardown %v); want hibernating before the teardown runs", state, teardown != nil)
	}
	teardown()
	if a.state != poolHibernated || a.hibernatedAt.IsZero() {
		t.Fatalf("after the teardown: state %s, hibernatedAt %v", a.state, a.hibernatedAt)
	}
}

// TestAdmitLocked_BusyLanguageIsAdmittedOverItsCapAndReported: with every
// server busy the newcomer is admitted (no victim) and the language is flagged
// until it is back under its cap.
func TestAdmitLocked_BusyLanguageIsAdmittedOverItsCapAndReported(t *testing.T) {
	p := hibernatePool("java", time.Hour, 1)
	installEntryLang(p, "/a", "java", &stubClient{})
	a := p.lookup("/a", "java")
	a.proxy.inflight.Add(1)
	p.mu.Lock()
	teardown := p.admitLocked("java", 1, nil)
	p.mu.Unlock()
	if teardown != nil {
		t.Fatal("a busy server was chosen as the victim")
	}
	if s := p.budgetSummary(); !strings.Contains(s, "over max_workspaces: java") {
		t.Fatalf("summary does not report the language over its cap: %q", s)
	}
	a.proxy.inflight.Add(-1)
	a.state = poolHibernated
	p.mu.Lock()
	p.admitLocked("java", 1, nil)
	p.mu.Unlock()
	if s := p.budgetSummary(); strings.Contains(s, "over max_workspaces") {
		t.Fatalf("the over-cap flag outlived the condition: %q", s)
	}
}

// TestWake_JoinsTheCap: waking a hibernated server at its language's cap
// hibernates another (here the idle one) rather than running over the cap.
func TestWake_JoinsTheCap(t *testing.T) {
	pool, ctx := fastWarmingPool(t, time.Minute)
	defer pool.close()
	for i := range pool.langs {
		pool.langs[i].cfg.MaxWorkspaces = 1
	}
	rootA, rootB := goModRoot(t), goModRoot(t)
	a, err := pool.acquireLang(ctx, rootA, "go", true)
	if err != nil {
		t.Fatal(err)
	}
	pool.hibernateEntry(a)
	b, err := pool.acquireLang(ctx, rootB, "go", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.acquireLang(ctx, rootA, "go", true); err != nil { // wakes A
		t.Fatal(err)
	}
	pool.mu.Lock()
	aState, bState := a.state, b.state
	pool.mu.Unlock()
	if !aState.running() || bState.running() {
		t.Fatalf("after the wake: A %s, B %s; want A running and B hibernated to keep the cap", aState, bState)
	}
}

func TestTrimLongHibernated(t *testing.T) {
	p := hibernatePool("go", time.Hour, 0)
	installEntryLang(p, "/free", "go", &stubClient{})
	installEntryLang(p, "/leased", "go", &stubClient{})
	installEntryLang(p, "/recent", "go", &stubClient{})
	long := time.Now().Add(-hibernatedTrimAfter - time.Minute)
	for root, at := range map[string]time.Time{"/free": long, "/leased": long, "/recent": time.Now()} {
		e := p.lookup(root, "go")
		e.state, e.hibernatedAt = poolHibernated, at
		e.cache = cache.New(time.Minute, 10)
		e.cache.Set("k", 1, time.Minute)
	}
	p.lookup("/leased", "go").refs = 1
	p.lookup("/recent", "go").refs = 1
	p.trimLongHibernated(time.Now())
	if p.lookup("/free", "go") != nil {
		t.Fatal("a long-hibernated entry no session leases was not removed")
	}
	leased := p.lookup("/leased", "go")
	if leased == nil {
		t.Fatal("a leased entry was removed")
	}
	if _, ok := leased.cache.Get("k"); ok {
		t.Fatal("a long-hibernated leased entry kept its warm cache")
	}
	if _, ok := p.lookup("/recent", "go").cache.Get("k"); !ok {
		t.Fatal("a recently hibernated entry lost its cache")
	}
}

func TestPoolBudget_Over(t *testing.T) {
	for _, c := range []struct {
		b    poolBudget
		want bool
	}{
		{poolBudget{active: softTargetActive, rss: softTargetRSS}, false},
		{poolBudget{active: softTargetActive + 1}, true},
		{poolBudget{rss: softTargetRSS + 1}, true},
	} {
		if got := c.b.over(); got != c.want {
			t.Errorf("%+v.over() = %v, want %v", c.b, got, c.want)
		}
	}
}

func TestSampleBudget_CountsActiveServers(t *testing.T) {
	pool, ctx := fastWarmingPool(t, time.Minute)
	defer pool.close()
	if _, err := pool.acquireLang(ctx, goModRoot(t), "go", true); err != nil {
		t.Fatal(err)
	}
	pool.sampleBudget(time.Now())
	pool.mu.Lock()
	b := pool.budget
	pool.mu.Unlock()
	if b.active != 1 || b.rss == 0 || b.sampledAt.IsZero() {
		t.Fatalf("budget sample = %+v, want one active server and a positive RSS estimate", b)
	}
	if s := pool.budgetSummary(); !strings.Contains(s, "1 active") || !strings.Contains(s, "est. RSS") {
		t.Fatalf("summary = %q", s)
	}
}

func TestLSPStatusReport_CarriesRefsCallsAndOverCap(t *testing.T) {
	p := hibernatePool("java", time.Hour, 1)
	installEntryLang(p, "/a", "java", &stubClient{})
	a := p.lookup("/a", "java")
	a.refs = 2
	a.proxy.inflight.Add(1)
	p.overCap = map[string]bool{"java": true}
	f := strings.Split(strings.TrimSpace(p.lspStatusReport()), "\t")
	if len(f) != 10 || f[7] != "2" || f[8] != "1" || f[9] != "1" {
		t.Fatalf("lsp-status fields = %q, want refs 2, calls 1, over-cap 1", f)
	}
	out := renderLSPStatus(strings.Join(f, "\t"))
	if !strings.Contains(out, "REFS") || !strings.Contains(out, "yes") {
		t.Fatalf("rendered table lacks the new columns:\n%s", out)
	}
	if old := renderLSPStatus(strings.Join(f[:7], "\t")); !strings.Contains(old, "/a") {
		t.Fatalf("a seven-field line from an older daemon is no longer rendered:\n%s", old)
	}
}

func TestDaemonInfo_ShowsThePoolRow(t *testing.T) {
	out, err := tools.NewDaemonInfo("s", "n", "v", time.Now()).WithLSPPool(func() string { return "3 active" }).
		Execute(context.Background(), nil)
	if err != nil || !strings.Contains(out, "lsp pool:       3 active") {
		t.Fatalf("daemon_info = %q, %v", out, err)
	}
}

func TestDefaults_EveryLanguageHibernatesWhenIdle(t *testing.T) {
	for name, l := range config.Defaults().LSP {
		if l.IdleTimeout.Duration <= 0 {
			t.Errorf("%s has no default idle timeout", name)
		}
	}
	if got := config.Defaults().LSP["java"].IdleTimeout.Duration; got != 20*time.Minute {
		t.Errorf("java idle timeout = %v, want its own 20m kept", got)
	}
	if got := config.Defaults().LSP["go"].IdleTimeout.Duration; got != 30*time.Minute {
		t.Errorf("go idle timeout = %v, want the 30m default", got)
	}
}

// TestDaemonInfo_RegisteredToolShowsThePoolRow goes through registerAllTools,
// where the pool row is wired.
func TestDaemonInfo_RegisteredToolShowsThePoolRow(t *testing.T) {
	_, srv := buildTestConnSession(t)
	tool, ok := srv.Lookup("daemon_info")
	if !ok {
		t.Fatal("daemon_info is not registered")
	}
	out, err := tool.Execute(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "lsp pool:") {
		t.Fatalf("the registered daemon_info has no pool row:\n%s", out)
	}
}
