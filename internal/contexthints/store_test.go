package contexthints

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTest(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "context_hints.db")
	s, err := openAt(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, path
}

func obs(ws, session, agent string, at time.Time) Observation {
	return Observation{
		At: at, Workspace: ws, SessionID: session, AgentID: agent,
		Host: "claude-code", HostVersion: "2.1.296", Event: "UserPromptSubmit",
		Seeds: []string{"internal/cli/hooks_codex.go"}, EmittedBytes: 120,
		Duration: 40 * time.Millisecond, Outcome: OutcomeEmitted,
	}
}

func TestRecord_RoundTripsAndCountsByOutcome(t *testing.T) {
	s, _ := openTest(t)
	now := time.Now()
	o := obs("/ws", "s1", "", now)
	if err := s.Record(o); err != nil {
		t.Fatal(err)
	}
	o.Outcome, o.Detail, o.EmittedBytes = OutcomeNoop, "off", 0
	if err := s.Record(o); err != nil {
		t.Fatal(err)
	}
	sum, err := s.Summary("/ws")
	if err != nil {
		t.Fatal(err)
	}
	if sum.Rows != 2 || sum.ByOutcome[OutcomeEmitted] != 1 || sum.ByOutcome[OutcomeNoop] != 1 {
		t.Fatalf("summary = %+v, want 2 rows, one emitted and one noop", sum)
	}
	if sum.Evicted != 0 {
		t.Errorf("evicted = %d, want 0", sum.Evicted)
	}
}

// Seeds are capped in count and in bytes before they are stored, so a hostile
// or runaway caller cannot grow the ledger through one row. Each cap is tested
// where it is the one that binds.
func TestRecord_CapsSeeds(t *testing.T) {
	for name, tc := range map[string]struct {
		seed string
		want int
	}{
		"count binds": {seed: "short.go", want: MaxSeeds},
		"bytes bind":  {seed: strings.Repeat("x", 100), want: MaxSeedBytes / 100},
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := openTest(t)
			o := obs("/ws", "s1", "", time.Now())
			o.Seeds = nil
			for range 50 {
				o.Seeds = append(o.Seeds, tc.seed)
			}
			if err := s.Record(o); err != nil {
				t.Fatal(err)
			}
			got, err := s.recent("/ws", 1)
			if err != nil || len(got) != 1 {
				t.Fatalf("recent = %v, %v", got, err)
			}
			if len(got[0].Seeds) != tc.want || seedBytes(got[0].Seeds) > MaxSeedBytes {
				t.Fatalf("stored %d seeds (%d bytes), want %d and <= %d bytes",
					len(got[0].Seeds), seedBytes(got[0].Seeds), tc.want, MaxSeedBytes)
			}
		})
	}
}

// A Detail outside the code alphabet is rejected rather than stored: the ledger
// holds metadata codes, never free text.
func TestRecord_RefusesFreeTextDetail(t *testing.T) {
	s, _ := openTest(t)
	o := obs("/ws", "s1", "", time.Now())
	o.Detail = "func secret() { return 42 }"
	if err := s.Record(o); err == nil {
		t.Fatal("a free-text detail was accepted")
	}
	o.Outcome = "made-up"
	o.Detail = ""
	if err := s.Record(o); err == nil {
		t.Fatal("an unknown outcome was accepted")
	}
}

// The per-workspace row cap evicts the oldest rows and counts them, so a missing
// row is reported as evicted rather than read as a no-op.
func TestRecord_RowCapEvictsOldestAndCounts(t *testing.T) {
	s, _ := openTest(t)
	base := time.Now().Add(-time.Hour)
	for i := range MaxRowsPerWorkspace + 25 {
		if err := s.Record(obs("/ws", "s1", "", base.Add(time.Duration(i)*time.Millisecond))); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Record(obs("/other", "s2", "", base)); err != nil {
		t.Fatal(err)
	}
	sum, _ := s.Summary("/ws")
	if sum.Rows != MaxRowsPerWorkspace || sum.Evicted != 25 {
		t.Fatalf("summary = rows %d evicted %d, want %d and 25", sum.Rows, sum.Evicted, MaxRowsPerWorkspace)
	}
	other, _ := s.Summary("/other")
	if other.Rows != 1 || other.Evicted != 0 {
		t.Errorf("another workspace was touched: %+v", other)
	}
	oldest, _ := s.recent("/ws", MaxRowsPerWorkspace)
	if !oldest[len(oldest)-1].At.After(base.Add(24 * time.Millisecond)) {
		t.Errorf("the oldest surviving row is %v, want the first 25 evicted", oldest[len(oldest)-1].At)
	}
}

// The byte cap binds on its own: rows big enough that the workspace reaches
// MaxBytesPerWorkspace well before MaxRowsPerWorkspace.
func TestRecord_ByteCapEvicts(t *testing.T) {
	s, _ := openTest(t)
	ws := "/" + strings.Repeat("w", 900)
	o := obs(ws, "s1", "", time.Now())
	o.Seeds = []string{strings.Repeat("a", MaxSeedBytes)}
	perCap := MaxBytesPerWorkspace / rowSize(o)
	if perCap >= MaxRowsPerWorkspace {
		t.Fatalf("rows of %d bytes reach the row cap first; the test proves nothing", rowSize(o))
	}
	for range perCap + 10 {
		o.At = o.At.Add(time.Millisecond)
		if err := s.Record(o); err != nil {
			t.Fatal(err)
		}
	}
	sum, _ := s.Summary(ws)
	if sum.Bytes > MaxBytesPerWorkspace || sum.Rows != perCap || sum.Evicted != 10 {
		t.Fatalf("summary = rows %d bytes %d evicted %d, want rows %d, <= %d bytes, 10 evicted",
			sum.Rows, sum.Bytes, sum.Evicted, perCap, MaxBytesPerWorkspace)
	}
}

// Age-out after the retention window also counts as evicted.
func TestPrune_AgesOutAndCounts(t *testing.T) {
	s, _ := openTest(t)
	now := time.Now()
	_ = s.Record(obs("/ws", "s1", "", now.Add(-Retention-time.Hour)))
	_ = s.Record(obs("/ws", "s1", "", now))
	if err := s.Prune(now); err != nil {
		t.Fatal(err)
	}
	sum, _ := s.Summary("/ws")
	if sum.Rows != 1 || sum.Evicted != 1 {
		t.Fatalf("summary = %+v, want 1 row and 1 evicted", sum)
	}
}

// Spend enforces both caps atomically and reports why it refused.
func TestSpend_TurnAndAgentCaps(t *testing.T) {
	s, _ := openTest(t)
	lim := Limits{PerTurn: 1024, PerAgent: 2048}
	if ok, _ := s.Spend("s1", "", "t1", 1000, lim, time.Now()); !ok {
		t.Fatal("first spend refused")
	}
	if ok, _ := s.Spend("s1", "", "t1", 100, lim, time.Now()); ok {
		t.Fatal("spend over the per-turn cap granted")
	}
	if ok, _ := s.Spend("s1", "", "t2", 1000, lim, time.Now()); !ok {
		t.Fatal("a new turn did not reopen the per-turn allowance")
	}
	if ok, _ := s.Spend("s1", "", "t3", 100, lim, time.Now()); ok {
		t.Fatal("spend over the per-agent cap granted")
	}
	if ok, _ := s.Spend("s1", "child", "t3", 100, lim, time.Now()); !ok {
		t.Fatal("another logical agent was charged for this one's spend")
	}
}

// A refused spend leaves both allowances exactly as they were: the rest of the
// turn window, and the rest of the agent's allowance, are still spendable.
func TestSpend_RefusalChargesNothing(t *testing.T) {
	s, _ := openTest(t)
	lim := Limits{PerTurn: 1024, PerAgent: 1524}
	if ok, _ := s.Spend("s1", "", "t1", 24, lim, time.Now()); !ok {
		t.Fatal("the opening spend was refused")
	}
	if ok, _ := s.Spend("s1", "", "t1", 2000, lim, time.Now()); ok {
		t.Fatal("an oversized spend was granted")
	}
	if ok, _ := s.Spend("s1", "", "t1", 1000, lim, time.Now()); !ok {
		t.Fatal("a refused spend consumed the turn allowance")
	}
	if ok, _ := s.Spend("s1", "", "t2", 500, lim, time.Now()); !ok {
		t.Fatal("a refused spend consumed the agent allowance")
	}
}

// M1: neither evicting the agent's observation rows nor reopening the database
// (a daemon restart) refills its allowance.
func TestSpend_EvictionAndRestartDoNotRefill(t *testing.T) {
	s, path := openTest(t)
	lim := Limits{PerTurn: 1024, PerAgent: 2048}
	now := time.Now()
	for _, turn := range []string{"t1", "t2"} {
		if ok, _ := s.Spend("s1", "", turn, 1024, lim, now); !ok {
			t.Fatalf("spend in %s refused", turn)
		}
		_ = s.Record(obs("/ws", "s1", "", now))
	}
	for i := range MaxRowsPerWorkspace + 5 { // evicts every s1 observation
		_ = s.Record(obs("/ws", "other", "", now.Add(time.Duration(i+1)*time.Millisecond)))
	}
	s.Close()
	s2, err := openAt(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if ok, _ := s2.Spend("s1", "", "t3", 1, lim, now); ok {
		t.Fatal("eviction plus restart refilled a spent allowance")
	}
}

// An allowance unused for the retention window ages out.
func TestPrune_AgesOutIdleAllowances(t *testing.T) {
	s, _ := openTest(t)
	lim := Limits{PerTurn: 1024, PerAgent: 1024}
	old := time.Now().Add(-Retention - time.Hour)
	_, _ = s.Spend("s1", "", "t1", 1024, lim, old)
	if err := s.Prune(time.Now()); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.Spend("s1", "", "t2", 10, lim, time.Now()); !ok {
		t.Fatal("an allowance idle past retention was not aged out")
	}
}

// The ledger never holds source or hint text: a marker passed in every field a
// caller controls, other than the capped seeds, is absent from the file.
func TestLedgerFileHoldsNoHintText(t *testing.T) {
	s, path := openTest(t)
	const marker = "PRIVATE-FIXTURE-MARKER-7f3a"
	o := obs("/ws", "s1", "", time.Now())
	if err := s.Record(o); err != nil {
		t.Fatal(err)
	}
	o.Detail = marker
	_ = s.Record(o) // refused: not a code
	s.Close()
	for _, f := range []string{path, path + "-wal"} {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		if strings.Contains(string(b), marker) {
			t.Fatalf("%s contains the marker", filepath.Base(f))
		}
	}
}
