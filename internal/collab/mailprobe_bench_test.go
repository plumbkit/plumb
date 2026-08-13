package collab

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"slices"
	"sync"
	"testing"
	"time"
)

// mailprobe_bench_test.go measures the candidate answers to the question the
// mailbox asks on EVERY successful tool call: "is there mail for me?".
//
// The design argument these numbers exist to settle is whether the in-process
// generation counter (notify.go, gated by chatWatch.due in internal/cli) earns
// its complexity, or whether a plain SQLite read would do:
//
//   - v1-naked    — ClaimNotes on every call. One UPDATE … RETURNING, which
//     takes SQLite's WAL writer lock even when it matches nothing.
//   - v2-probe    — a SELECT … LIMIT 1 read first, claiming only when it
//     returns a row. WAL readers do not block, so the steady state never
//     touches the writer lock.
//   - v3-notifier — today's design: a map read behind a mutex, and a claim only
//     when a generation has moved (or the 30s backstop is due).
//
// v1p / v2p are the same two SQL variants driven through a cached *sql.Stmt.
// They are here because database/sql's one-shot Query path re-parses the SQL on
// EVERY call, which turns out to be a large fraction of what the "SQLite is
// cheap" and "SQLite is expensive" positions were both actually measuring, and
// neither position had separated it out.
//
// WHERE THE COST ACTUALLY IS (round-robin interleaved, per-op p50, measured by
// TestDiagDecompose-style decomposition; earlier revisions of this file blamed
// the ORDER BY, which was wrong and is corrected here):
//
//	claim unprepared (production shape)   ~101us    probe unprepared   ~22us
//	claim prepared once                    ~62us    probe prepared      ~6us
//	sqlite3_prepare of the claim           ~42us    prepare of probe   ~16us
//	claim prepared, ORDER BY removed       ~55us  <- the sort costs ~6us
//	claim prepared, subquery flattened     ~11us  <- id IN (...) is ~44us
//
// So two mechanisms, neither of them the sort: the `id IN (subquery)` LIST
// SUBQUERY is ~70% of execution (it materialises an ephemeral b-tree even when
// the subquery yields nothing), and per-call statement PARSING is ~42% of the
// production claim and ~70% of the production probe. The ORDER BY does appear
// in EXPLAIN QUERY PLAN as USE TEMP B-TREE FOR ORDER BY, and it does nothing
// measurable when zero rows reach it — do not go and rewrite the sort.
//
// WHAT IS FAITHFUL HERE: a real on-disk collab.db opened through the production
// Open() (so the real sqlitex pragmas, WAL, busy timeout and schema apply), the
// real ClaimNotes statement, the real Notifier, one Store shared by every
// session exactly as the cli collabPool shares it, and a table carrying the
// residue a live mailbox has rather than being empty.
//
// WHAT IS NOT: the gate is a copy of (*chatWatch).due, because that type lives
// in internal/cli which cannot reach this package's unexported *sql.DB. The
// prepared variants likewise re-declare ClaimNotes' SQL, since the production
// store has no prepared-statement cache to borrow —
// TestMailprobePreparedClaim_MirrorsClaimNotes fails if the copy drifts. The
// per-call config lookup, Inbox construction and RenderMessages that
// messageHint also performs are outside the measurement for every variant
// alike, as is the cross-project store (a second ClaimNotes, which doubles
// v1/v2's steady-state work and adds nothing to v3's).
//
// METRICS. ns/op is overridden with the per-call p50, NOT the sample mean and
// NOT wall/ops. That is deliberate: on a shared machine the mean is dominated
// by descheduling — a run's mean moves several-fold between identical runs
// while its p50 moves by ~2% — so the mean is not a number anyone re-running
// this will reproduce. mean-ns is still reported, for the gap between them.
// max-ns is scheduler noise by construction (a mutex-and-slice-compare
// operation reports tens of milliseconds) and is retained only as evidence of
// that. p99-ns is emitted only when enough samples were kept to make it mean
// something (benchP99MinSamples); samples-n is always reported so a percentile
// is never read without its denominator.
//
// Every sample includes one time.Now/time.Since pair, and the M2 timebase ticks
// at ~41.67ns, so v3's sub-100ns figures are quantised: read them as "one or
// two ticks", not to three significant figures.
// BenchmarkMailProbe_HarnessOverhead measures that floor.

// benchClaimLimit mirrors the per-call delivery cap the tools package applies
// (maxDeliveredPerCall), so the claim statement carries its production LIMIT.
const benchClaimLimit = 3

// benchBackgroundRows is the residue a live mailbox carries: delivered notes to
// other sessions. Small on purpose — a real collab.db holds a handful of
// expiring advisory rows, not a table worth planning around.
const benchBackgroundRows = 64

// benchMaxSamples bounds the retained latency samples per session. Beyond it
// every k-th call is kept; the running mean and max still cover every call.
const benchMaxSamples = 100_000

// benchP99MinSamples is the floor below which p99 is not reported at all. Under
// it the "99th percentile" is the second- or third-worst observation in the run,
// which is a sample of the machine's scheduler rather than of this code — and
// quoting it as a tail latency is how a 7ms figure turns out to be reproducible
// only within a factor of four.
const benchP99MinSamples = 1000

// pendingProbe is the v2 candidate: the cheapest read that answers "is anything
// waiting for me", with the same predicates ClaimNotes matches on so it can
// never say no to a note the claim would have handed over.
const pendingProbe = `SELECT 1 FROM collab_rows
	 WHERE kind = ? AND delivered_at = 0 AND expires_at > ?
	   AND (addressee = ? OR addressee = ?)
	   AND (target_workspace = '' OR target_workspace = ?)
	 LIMIT 1`

// preparedClaim re-declares the statement ClaimNotes builds for a positive
// limit, so it can be driven through a cached *sql.Stmt. The production store
// has no statement cache to borrow, and this is the option nobody had costed.
// It must stay byte-identical in effect to ClaimNotes' own statement;
// TestMailprobePreparedClaim_MirrorsClaimNotes is what enforces that.
const preparedClaim = `UPDATE collab_rows SET delivered_at = ?, delivered_to = ?
			 WHERE id IN (SELECT id FROM collab_rows
				 WHERE kind = ? AND delivered_at = 0 AND expires_at > ?
				   AND (addressee = ? OR addressee = ?) AND (target_workspace = '' OR target_workspace = ?)
				 ORDER BY created_at ASC LIMIT ?)
			 RETURNING ` + rowColumns

// preparedClaimArgs builds the bind list preparedClaim expects, in ClaimNotes'
// order: the SET pair first, then the subquery's predicates.
func preparedClaimArgs(sessionName, workspace string, now time.Time, limit int) []any {
	return []any{
		now.UnixNano(), sessionName,
		string(KindNote), now.UnixNano(), sessionName, AddresseeNext, workspace, limit,
	}
}

// probe is one session's hot-path check plus the peer activity that sets the
// scenario up. arrive and settle both run OUTSIDE the timed region — they are
// the other agent's leave_note, not part of what this session pays.
//
// Concurrency: not safe for concurrent use. One probe belongs to one session
// goroutine; the harness builds a separate probe per session.
type probe struct {
	call   func()
	arrive func() // before the timed call: a peer sends, or takes the writer lock
	settle func() // after it: wait for that peer to be done
}

// benchGate is a copy of (*chatWatch).due from internal/cli, reproduced here
// because that type is unexported in a package that cannot import this one's
// internals. Keep it identical — including lastCheck, which the production gate
// records and nothing reads (see the note in the review follow-up); a copy that
// quietly drops a field is not the fidelity claim this file makes.
//
// Concurrency: safe for concurrent use, like the original — tool calls on one
// connection can overlap.
type benchGate struct {
	mu        sync.Mutex
	keys      []string
	gens      []uint64
	lastFull  time.Time
	lastCheck time.Time
}

const benchFullCheckInterval = 30 * time.Second

func (g *benchGate) due(keys []string, gens []uint64, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	changed := len(g.keys) != len(keys) || len(g.gens) != len(gens)
	if !changed {
		for i := range keys {
			if g.keys[i] != keys[i] || g.gens[i] != gens[i] {
				changed = true
				break
			}
		}
	}
	backstop := now.Sub(g.lastFull) >= benchFullCheckInterval
	g.lastCheck = now
	if !changed && !backstop {
		return false
	}
	g.keys = append(g.keys[:0], keys...)
	g.gens = append(g.gens[:0], gens...)
	g.lastFull = now
	return true
}

// benchEnv is the shared state one benchmark case runs against: a single store
// and notifier, as the cli collabPool shares them, plus the prepared statements
// the cached-statement variants need.
//
// Concurrency: the *sql.DB, *sql.Stmt and *Notifier it holds are all safe for
// concurrent use; the struct itself is built once before any session starts.
type benchEnv struct {
	s          *Store
	ws         string
	n          *Notifier
	claimStmt  *sql.Stmt
	probeStmt  *sql.Stmt
	preparedOK bool
}

func newBenchEnv(b *testing.B, prepared bool) *benchEnv {
	b.Helper()
	ws := b.TempDir()
	s, err := Open(ws)
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	b.Cleanup(func() { _ = s.Close() })
	seedBackground(b, s, benchBackgroundRows)

	e := &benchEnv{s: s, ws: ws, n: NewNotifier(), preparedOK: prepared}
	if prepared {
		e.claimStmt = mustPrepare(b, s, preparedClaim)
		e.probeStmt = mustPrepare(b, s, pendingProbe)
	}
	return e
}

func mustPrepare(b *testing.B, s *Store, q string) *sql.Stmt {
	b.Helper()
	st, err := s.db.PrepareContext(context.Background(), q)
	if err != nil {
		b.Fatalf("prepare: %v", err)
	}
	b.Cleanup(func() { _ = st.Close() })
	return st
}

const benchInsert = `INSERT INTO collab_rows
	 (kind, author_session, author_id, body, path_globs, addressee, created_at,
	  expires_at, conversation_id, delivered_at, delivered_to, origin_workspace, target_workspace)
	 VALUES (?, ?, ?, ?, '', ?, ?, ?, ?, ?, ?, '', '')`

// seedBackground fills the table with already-delivered notes for other
// sessions, so no variant is measured against an empty table where SQLite's
// plans are unrepresentatively cheap.
func seedBackground(b *testing.B, s *Store, rows int) {
	b.Helper()
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		b.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now()
	for i := range rows {
		if _, err := tx.ExecContext(ctx, benchInsert,
			string(KindNote), "peer", "peer-id", "background chatter",
			fmt.Sprintf("other-%d", i%7), now.Add(-time.Hour).UnixNano(),
			now.Add(time.Hour).UnixNano(), fmt.Sprintf("cbg%04d", i),
			now.UnixNano(), "other"); err != nil {
			b.Fatalf("seed background: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatalf("commit background: %v", err)
	}
}

func benchSessionName(i int) string { return fmt.Sprintf("bench-sess-%d", i) }

// --- the variants -----------------------------------------------------------

func newNakedProbe(e *benchEnv, id int) probe {
	ctx := context.Background()
	name := benchSessionName(id)
	return probe{call: func() {
		_, _ = e.s.ClaimNotes(ctx, name, e.ws, time.Now(), benchClaimLimit)
	}}
}

func newNakedPreparedProbe(e *benchEnv, id int) probe {
	ctx := context.Background()
	name := benchSessionName(id)
	return probe{call: func() {
		rows, err := e.claimStmt.QueryContext(ctx,
			preparedClaimArgs(name, e.ws, time.Now(), benchClaimLimit)...)
		if err != nil {
			return
		}
		_, _ = scanRows(rows)
		_ = rows.Close()
	}}
}

func newProbeThenClaim(e *benchEnv, id int) probe {
	ctx := context.Background()
	name := benchSessionName(id)
	return probe{call: func() {
		now := time.Now()
		var one int
		err := e.s.db.QueryRowContext(ctx, pendingProbe,
			string(KindNote), now.UnixNano(), name, AddresseeNext, e.ws).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return
		}
		_, _ = e.s.ClaimNotes(ctx, name, e.ws, now, benchClaimLimit)
	}}
}

func newProbeThenClaimPrepared(e *benchEnv, id int) probe {
	ctx := context.Background()
	name := benchSessionName(id)
	return probe{call: func() {
		now := time.Now()
		var one int
		err := e.probeStmt.QueryRowContext(ctx,
			string(KindNote), now.UnixNano(), name, AddresseeNext, e.ws).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return
		}
		rows, qErr := e.claimStmt.QueryContext(ctx,
			preparedClaimArgs(name, e.ws, now, benchClaimLimit)...)
		if qErr != nil {
			return
		}
		_, _ = scanRows(rows)
		_ = rows.Close()
	}}
}

func newNotifierProbe(e *benchEnv, id int) probe {
	ctx := context.Background()
	name := benchSessionName(id)
	keys := []string{name, NotifyKey(e.ws, AddresseeNext)}
	g := &benchGate{}
	return probe{call: func() {
		now := time.Now()
		if !g.due(keys, e.n.Gens(keys), now) {
			return // provably nothing new since the last look: no query
		}
		_, _ = e.s.ClaimNotes(ctx, name, e.ws, now, benchClaimLimit)
	}}
}

// variant names one candidate design. prepared marks the ones that need the
// env's cached statements built.
type variant struct {
	name     string
	prepared bool
	build    func(e *benchEnv, id int) probe
}

func variants() []variant {
	return []variant{
		{name: "v1-naked", build: newNakedProbe},
		{name: "v1p-naked-prepared", prepared: true, build: newNakedPreparedProbe},
		{name: "v2-probe", build: newProbeThenClaim},
		{name: "v2p-probe-prepared", prepared: true, build: newProbeThenClaimPrepared},
		{name: "v3-notifier", build: newNotifierProbe},
	}
}

// --- harness ----------------------------------------------------------------

// latencies accumulates one session's per-call samples.
//
// Concurrency: not safe for concurrent use. Each session goroutine owns one and
// they are merged only after every goroutine has finished.
type latencies struct {
	kept []time.Duration
	sum  time.Duration
	max  time.Duration
	n    int
}

func (l *latencies) merge(o *latencies) {
	l.kept = append(l.kept, o.kept...)
	l.sum += o.sum
	l.n += o.n
	if o.max > l.max {
		l.max = o.max
	}
}

func (l *latencies) percentile(p float64) float64 {
	if len(l.kept) == 0 {
		return 0
	}
	idx := int(math.Ceil(p/100*float64(len(l.kept)))) - 1
	return float64(l.kept[max(idx, 0)].Nanoseconds())
}

func (l *latencies) report(b *testing.B, wall time.Duration) {
	b.Helper()
	if l.n == 0 {
		return
	}
	slices.Sort(l.kept)
	// ns/op is the p50, not the mean: the mean here is mostly a measurement of
	// how busy the machine was, and is not reproducible run to run.
	b.ReportMetric(l.percentile(50), "ns/op")
	b.ReportMetric(float64(l.sum.Nanoseconds())/float64(l.n), "mean-ns")
	if len(l.kept) >= benchP99MinSamples {
		b.ReportMetric(l.percentile(99), "p99-ns")
	}
	b.ReportMetric(float64(l.max.Nanoseconds()), "max-ns")
	b.ReportMetric(float64(len(l.kept)), "samples-n")
	b.ReportMetric(float64(wall.Nanoseconds())/float64(l.n), "wall-ns/op")
}

// runProbes drives `sessions` goroutines, each calling its own probe b.N times
// against one shared store, and reports the merged latency distribution.
func runProbes(b *testing.B, sessions int, build func(id int) probe) {
	b.Helper()
	probes := make([]probe, sessions)
	for i := range probes {
		probes[i] = build(i)
	}
	// One untimed pass per session so the first measured call is not paying for
	// a cold connection, a cold page cache or a first statement prepare.
	for i := range probes {
		if probes[i].arrive != nil {
			probes[i].arrive()
		}
		probes[i].call()
		if probes[i].settle != nil {
			probes[i].settle()
		}
	}

	// Ceil, not floor: a floor of b.N/benchMaxSamples is 1 for every b.N below
	// twice the cap, so the cap would not bind at all between 100k and 200k.
	step := (b.N + benchMaxSamples - 1) / benchMaxSamples
	if step < 1 {
		step = 1
	}
	per := make([]latencies, sessions)
	var wg sync.WaitGroup
	b.ResetTimer()
	start := time.Now()
	for g := range sessions {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			p := probes[g]
			l := &per[g]
			l.kept = make([]time.Duration, 0, min(b.N, benchMaxSamples)+1)
			for i := range b.N {
				if p.arrive != nil {
					p.arrive()
				}
				t0 := time.Now()
				p.call()
				d := time.Since(t0)
				if p.settle != nil {
					p.settle()
				}
				l.sum += d
				l.n++
				if d > l.max {
					l.max = d
				}
				if i%step == 0 {
					l.kept = append(l.kept, d)
				}
			}
		}(g)
	}
	wg.Wait()
	wall := time.Since(start)
	b.StopTimer()

	var all latencies
	for i := range per {
		all.merge(&per[i])
	}
	all.report(b, wall)
}

// --- benchmarks -------------------------------------------------------------

// BenchmarkMailProbe_Idle is the case that decides the argument: no mail
// waiting, which is ~99.9% of tool calls. The session count is the axis that
// matters — the stated concern is writer-lock contention between the sessions
// sharing a workspace, not mean latency at rest.
//
// READ ns/op (the p50). mean-ns on this benchmark swings several-fold between
// identical runs on a shared machine while the p50 holds to a couple of
// percent; max-ns is pure descheduling.
//
// The session sweep SATURATES the database — millions of checks a second, which
// no agent makes — so its contention figures describe a load that never
// happens, and they exaggerate every variant's problem including v3's (whose
// cost at 8 sessions is contention on the notifier's single mutex).
// BenchmarkMailProbe_LockCollision is the rate-independent one.
//
// v3's figure here also EXCLUDES the 30s backstop, which fires a full claim
// regardless of the gate. At one tool call a second that amortises to roughly
// claim/30 per call; at one call every five seconds it dominates the gate
// entirely. Quote v3 as "the gate is free", never as "the mailbox check is
// free".
func BenchmarkMailProbe_Idle(b *testing.B) {
	for _, v := range variants() {
		for _, sessions := range []int{1, 2, 4, 8} {
			b.Run(fmt.Sprintf("%s/sessions=%d", v.name, sessions), func(b *testing.B) {
				e := newBenchEnv(b, v.prepared)
				runProbes(b, sessions, func(id int) probe { return v.build(e, id) })
			})
		}
	}
}

// THE WITH-MAIL CASE IS DELIBERATELY NOT BENCHMARKED HERE, and that is a
// finding rather than an omission.
//
// Two attempts were made. A three-way comparison produced a strict-ordering
// violation — with mail present every variant runs the identical ClaimNotes, so
// v3 does everything v1 does plus a gate check, yet it measured 2-6x FASTER,
// with the gap tracking b.N. Narrowing it to a single variant did not help: its
// p50 still moved 7.7x across five identical runs (219us to 1.69ms), again
// correlated with b.N. The cause is structural, not statistical. This path is
// fsync-bound (roughly half of all profile samples land in
// _pagerWalFrames -> fsync), and having mail on every iteration requires a write
// on every iteration, which puts WAL auto-checkpoint scheduling inside the
// measurement. What such a benchmark reports is checkpoint phase.
//
// The regression question does not need it. With mail present the claim is
// common to every variant and cancels; the only difference between them is the
// gate or probe run in FRONT of the claim, and BenchmarkMailProbe_Idle prices
// those exactly. A variant cannot regress the delivery path by adding a check
// that costs 83ns or 20us to a claim that costs hundreds of microseconds.
//
// Do not re-add a loop benchmark here without first showing its p50 is stable
// across runs; an unstable number is worse than no number, because it gets
// quoted.

// BenchmarkMailProbe_HarnessOverhead is the floor every number above sits on:
// the time.Now/time.Since pair and the loop around an empty probe. On this
// timebase a tick is ~41.67ns, so the cheap variants are measured in single
// ticks and their low-order digits are quantisation, not signal.
//
// Its own p50 is 0 — over half of all empty-probe calls complete inside one
// tick — and the testing package omits a zero-valued metric, so this benchmark
// reports no ns/op column at all. Read mean-ns as the floor.
func BenchmarkMailProbe_HarnessOverhead(b *testing.B) {
	for _, sessions := range []int{1, 8} {
		b.Run(fmt.Sprintf("sessions=%d", sessions), func(b *testing.B) {
			runProbes(b, sessions, func(int) probe { return probe{call: func() {}} })
		})
	}
}

// benchHoldWriteLock is how long a colliding writer holds collab.db's writer
// lock in the collision benchmark. Deliberately SHORTER than a real leave_note
// insert, so the number measured is the cost of the collision itself and not of
// waiting out the other writer's work.
const benchHoldWriteLock = 500 * time.Microsecond

// lockHolder takes collab.db's writer lock for benchHoldWriteLock, standing in
// for another session's leave_note, share_intent or the reaper's prune. seize
// returns once the lock is genuinely held; release waits for it to be given up.
//
// Concurrency: not safe for concurrent use — one holder serves one session's
// probe, and seize/release must alternate on that session's goroutine.
type lockHolder struct {
	s    *Store
	done chan struct{}
}

func (h *lockHolder) seize() {
	held := make(chan struct{})
	h.done = make(chan struct{})
	go func() {
		defer close(h.done)
		tx, err := h.s.db.BeginTx(context.Background(), nil)
		if err != nil {
			close(held)
			return
		}
		// An INSERT, not a BEGIN IMMEDIATE: this is what a peer's leave_note does,
		// and it is what actually takes the WAL writer lock.
		_, _ = tx.ExecContext(context.Background(), benchInsert,
			string(KindNote), "holder", "holder", "holding", "someone-else",
			time.Now().UnixNano(), time.Now().Add(time.Hour).UnixNano(), "chold", 0, "")
		close(held)
		time.Sleep(benchHoldWriteLock)
		_ = tx.Rollback()
	}()
	<-held
}

func (h *lockHolder) release() { <-h.done }

// BenchmarkMailProbe_LockCollision is the measurement the concurrency sweep
// cannot make honestly: what ONE collision costs, at any call rate.
//
// Real sessions make a few tool calls a second, not hundreds of thousands, so
// the sweep's contention figures describe a load that never happens. This one
// instead arranges exactly one collision per call: a peer holds the writer lock
// (a leave_note, a share_intent, the reaper's prune) while this session runs its
// mailbox check. That does happen, and the question is what it costs when it
// does.
//
// The lock is held for only benchHoldWriteLock, so anything much above that in
// the result is SQLite's busy handler, which backs off in MILLISECONDS. Note
// this is the one place where preparing the statement changes nothing: the
// collision is a lock wait, not parse or execution time.
func BenchmarkMailProbe_LockCollision(b *testing.B) {
	for _, v := range variants() {
		b.Run(v.name, func(b *testing.B) {
			e := newBenchEnv(b, v.prepared)
			h := &lockHolder{s: e.s}
			runProbes(b, 1, func(id int) probe {
				p := v.build(e, id)
				p.arrive, p.settle = h.seize, h.release
				return p
			})
		})
	}
}

// TestMailprobePreparedClaim_MirrorsClaimNotes keeps the prepared variants
// honest. They re-declare ClaimNotes' statement so it can be driven through a
// cached *sql.Stmt, and a benchmark comparing a DIFFERENT query to the
// production one would be worse than no benchmark. Two identically seeded
// stores must hand back the same rows.
func TestMailprobePreparedClaim_MirrorsClaimNotes(t *testing.T) {
	ctx := context.Background()
	const me = "claimant"

	seed := func(t *testing.T) *Store {
		t.Helper()
		ws := t.TempDir()
		s, err := Open(ws)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		now := time.Now()
		for i := range 5 {
			addressee := me
			if i == 3 {
				addressee = AddresseeNext // both shapes the claim accepts
			}
			if _, err := s.db.ExecContext(ctx, benchInsert,
				string(KindNote), "peer", "peer-id", fmt.Sprintf("note %d", i),
				addressee, now.Add(time.Duration(i)*time.Second).UnixNano(),
				now.Add(time.Hour).UnixNano(), fmt.Sprintf("c%d", i), 0, ""); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}
		// Rows that must NOT be claimed: delivered, expired, someone else's.
		if _, err := s.db.ExecContext(ctx, benchInsert,
			string(KindNote), "peer", "peer-id", "already read", me,
			now.UnixNano(), now.Add(time.Hour).UnixNano(), "cd", now.UnixNano(), me); err != nil {
			t.Fatalf("seed delivered: %v", err)
		}
		if _, err := s.db.ExecContext(ctx, benchInsert,
			string(KindNote), "peer", "peer-id", "stale", me,
			now.UnixNano(), now.Add(-time.Hour).UnixNano(), "ce", 0, ""); err != nil {
			t.Fatalf("seed expired: %v", err)
		}
		return s
	}

	now := time.Now().Add(time.Minute)
	a, bStore := seed(t), seed(t)

	want, err := a.ClaimNotes(ctx, me, a.ws, now, benchClaimLimit)
	if err != nil {
		t.Fatalf("ClaimNotes: %v", err)
	}
	if len(want) == 0 {
		t.Fatal("ClaimNotes claimed nothing; the fixture is not exercising the statement")
	}

	st, err := bStore.db.PrepareContext(ctx, preparedClaim)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	defer st.Close()
	rows, err := st.QueryContext(ctx, preparedClaimArgs(me, bStore.ws, now, benchClaimLimit)...)
	if err != nil {
		t.Fatalf("prepared claim: %v", err)
	}
	got, err := scanRows(rows)
	rows.Close()
	if err != nil {
		t.Fatalf("scan: %v", err)
	}

	if len(got) != len(want) {
		t.Fatalf("prepared claim returned %d rows, ClaimNotes returned %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i].ID || got[i].Body != want[i].Body ||
			got[i].Addressee != want[i].Addressee || got[i].DeliveredTo != want[i].DeliveredTo {
			t.Fatalf("row %d differs:\n prepared: %+v\n ClaimNotes: %+v", i, got[i], want[i])
		}
	}
}
