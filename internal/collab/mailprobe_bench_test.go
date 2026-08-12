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

// mailprobe_bench_test.go measures the three candidate answers to the question
// the mailbox asks on EVERY successful tool call: "is there mail for me?".
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
//   - v3-notifier — today's design: an atomic-ish map read behind a mutex, and
//     a claim only when a generation has moved (or the 30s backstop is due).
//
// WHAT IS FAITHFUL HERE: a real on-disk collab.db opened through the production
// Open() (so the real sqlitex pragmas, WAL, busy timeout and schema apply), the
// real ClaimNotes statement, the real Notifier, one Store shared by every
// session exactly as the cli collabPool shares it, and a table carrying the
// residue a live mailbox has rather than being empty.
//
// WHAT IS NOT: the gate is a copy of (*chatWatch).due, because that type lives
// in internal/cli which cannot reach this package's unexported *sql.DB — the
// copy is line-for-line and its cost is separately pinned by
// BenchmarkChatWatchGate_Idle in internal/cli. The per-call config lookup,
// Inbox construction and RenderMessages that messageHint also performs are
// outside the measurement for all three variants alike, as is the cross-project
// store (a second ClaimNotes, which doubles v1/v2's steady-state work and adds
// nothing to v3's).
//
// METRICS. The default ns/op is overridden with the SAMPLE MEAN per-call
// latency, which is the quantity the argument is about — under N concurrent
// sessions, wall/ops measures throughput, not what a tool call waits for.
// Alongside it: p50-ns, p99-ns, max-ns (contention shows up in the tail, not
// the mean) and wall-ns/op for aggregate throughput. Every sample includes one
// time.Now/time.Since pair; BenchmarkMailProbe_HarnessOverhead measures that
// floor so the cheap variants can be read net of it.

// benchClaimLimit mirrors the per-call delivery cap the tools package applies
// (maxDeliveredPerCall), so the claim statement carries its production LIMIT.
const benchClaimLimit = 3

// benchPendingAuthor tags the rows a with-mail benchmark re-arms between timed
// regions, so the reset touches only those and never the background residue.
const benchPendingAuthor = "mailbench-pending"

// benchBackgroundRows is the residue a live mailbox carries: delivered notes to
// other sessions. Small on purpose — a real collab.db holds a handful of
// expiring advisory rows, not a table worth planning around.
const benchBackgroundRows = 64

// benchMaxSamples bounds the retained latency samples per session. Beyond it
// every k-th call is kept; the running mean and max still cover every call.
const benchMaxSamples = 100_000

// pendingProbe is the v2 candidate: the cheapest read that answers "is anything
// waiting for me", with the same predicates ClaimNotes matches on so it can
// never say no to a note the claim would have handed over.
const pendingProbe = `SELECT 1 FROM collab_rows
	 WHERE kind = ? AND delivered_at = 0 AND expires_at > ?
	   AND (addressee = ? OR addressee = ?)
	   AND (target_workspace = '' OR target_workspace = ?)
	 LIMIT 1`

// probe is one session's hot-path check plus the peer activity that sets the
// scenario up. arrive and settle both run OUTSIDE the timed region — they are
// the other agent's leave_note, not part of what this session pays.
type probe struct {
	call   func()
	arrive func() // before the timed call: a peer sends, or takes the writer lock
	settle func() // after it: wait for that peer to be done
}

// benchGate is a copy of (*chatWatch).due from internal/cli, reproduced here
// because that type is unexported in a package that cannot import this one's
// internals. Keep it identical; internal/cli's own gate benchmark is what
// proves the copy still costs what the original does.
type benchGate struct {
	mu       sync.Mutex
	keys     []string
	gens     []uint64
	lastFull time.Time
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
	if !changed && !backstop {
		return false
	}
	g.keys = append(g.keys[:0], keys...)
	g.gens = append(g.gens[:0], gens...)
	g.lastFull = now
	return true
}

func benchStore(b *testing.B) (*Store, string) {
	b.Helper()
	ws := b.TempDir()
	s, err := Open(ws)
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	b.Cleanup(func() { _ = s.Close() })
	return s, ws
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

// seedPending gives a session n unread notes to find.
func seedPending(b *testing.B, s *Store, addressee string, n int) {
	b.Helper()
	ctx := context.Background()
	now := time.Now()
	for i := range n {
		if _, err := s.db.ExecContext(ctx, benchInsert,
			string(KindNote), "peer", benchPendingAuthor, "you have mail",
			addressee, now.UnixNano(), now.Add(time.Hour).UnixNano(),
			fmt.Sprintf("c%s-%d", addressee, i), 0, ""); err != nil {
			b.Fatalf("seed pending: %v", err)
		}
	}
}

// rearmPending puts a session's seeded notes back on the unread side of the
// watermark, standing in for the peer that sent them. Cheaper and more stable
// than inserting fresh rows, which would make the table — and so the claim's
// cost — drift over the run.
func rearmPending(s *Store, addressee string) {
	_, _ = s.db.ExecContext(context.Background(),
		`UPDATE collab_rows SET delivered_at = 0, delivered_to = ''
		 WHERE author_id = ? AND addressee = ?`, benchPendingAuthor, addressee)
}

func benchSessionName(i int) string { return fmt.Sprintf("bench-sess-%d", i) }

// --- the three variants -----------------------------------------------------

func newNakedProbe(s *Store, ws string, id int) probe {
	ctx := context.Background()
	name := benchSessionName(id)
	return probe{call: func() {
		_, _ = s.ClaimNotes(ctx, name, ws, time.Now(), benchClaimLimit)
	}}
}

func newProbeThenClaim(s *Store, ws string, id int) probe {
	ctx := context.Background()
	name := benchSessionName(id)
	return probe{call: func() {
		now := time.Now()
		var one int
		err := s.db.QueryRowContext(ctx, pendingProbe,
			string(KindNote), now.UnixNano(), name, AddresseeNext, ws).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return
		}
		_, _ = s.ClaimNotes(ctx, name, ws, now, benchClaimLimit)
	}}
}

func newNotifierProbe(n *Notifier, s *Store, ws string, id int) probe {
	ctx := context.Background()
	name := benchSessionName(id)
	keys := []string{name, NotifyKey(ws, AddresseeNext)}
	g := &benchGate{}
	return probe{call: func() {
		now := time.Now()
		if !g.due(keys, n.Gens(keys), now) {
			return // provably nothing new since the last look: no query
		}
		_, _ = s.ClaimNotes(ctx, name, ws, now, benchClaimLimit)
	}}
}

// variant names one candidate design and builds a per-session probe against a
// store the benchmark owns. withMail asks for the arrive hook.
type variant struct {
	name string
	// build returns a probe for session id. n is the shared notifier; only
	// v3-notifier consults it, but every variant is handed one so a with-mail
	// arrival can bump it uniformly.
	build func(n *Notifier, s *Store, ws string, id int) probe
}

func variants() []variant {
	return []variant{
		{"v1-naked", func(_ *Notifier, s *Store, ws string, id int) probe {
			return newNakedProbe(s, ws, id)
		}},
		{"v2-probe", func(_ *Notifier, s *Store, ws string, id int) probe {
			return newProbeThenClaim(s, ws, id)
		}},
		{"v3-notifier", func(n *Notifier, s *Store, ws string, id int) probe {
			return newNotifierProbe(n, s, ws, id)
		}},
	}
}

// --- harness ----------------------------------------------------------------

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
	slices.Sort(l.kept)
	if l.n == 0 {
		return
	}
	// Overrides the framework's ns/op: under N sessions wall/ops is throughput,
	// and the design argument is about what one tool call waits for.
	b.ReportMetric(float64(l.sum.Nanoseconds())/float64(l.n), "ns/op")
	b.ReportMetric(l.percentile(50), "p50-ns")
	b.ReportMetric(l.percentile(99), "p99-ns")
	b.ReportMetric(float64(l.max.Nanoseconds()), "max-ns")
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

	step := 1
	if b.N > benchMaxSamples {
		step = b.N / benchMaxSamples
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
func BenchmarkMailProbe_Idle(b *testing.B) {
	for _, v := range variants() {
		for _, sessions := range []int{1, 2, 4, 8} {
			b.Run(fmt.Sprintf("%s/sessions=%d", v.name, sessions), func(b *testing.B) {
				s, ws := benchStore(b)
				seedBackground(b, s, benchBackgroundRows)
				n := NewNotifier()
				runProbes(b, sessions, func(id int) probe {
					return v.build(n, s, ws, id)
				})
			})
		}
	}
}

// BenchmarkMailProbe_WithMail checks the path that must not regress: a peer has
// written, and the check has to find and claim the notes. The re-arm that makes
// each iteration find mail again runs outside the timed region — it is the
// sender's cost, not the recipient's — so wall-ns/op is meaningless here.
//
// READ p50 HERE, NOT THE MEAN. Unlike the idle case, both the re-arm and the
// claim write WAL frames, so a run of a few thousand iterations crosses SQLite's
// auto-checkpoint threshold repeatedly; a checkpoint stalls whichever call it
// lands on for tens to hundreds of milliseconds, which swamps mean, p99 and max
// while leaving p50 stable to within a few percent. Those stalls are an artefact
// of driving a year's worth of mailbox traffic through one second, not something
// a delivery path meets in production.
//
// Session counts stay low for the same reason: at higher concurrency the untimed
// re-arm writes contend with the timed claims and the number stops measuring the
// claim.
func BenchmarkMailProbe_WithMail(b *testing.B) {
	for _, v := range variants() {
		for _, sessions := range []int{1, 4} {
			b.Run(fmt.Sprintf("%s/sessions=%d", v.name, sessions), func(b *testing.B) {
				s, ws := benchStore(b)
				seedBackground(b, s, benchBackgroundRows)
				n := NewNotifier()
				runProbes(b, sessions, func(id int) probe {
					name := benchSessionName(id)
					seedPending(b, s, name, benchClaimLimit)
					p := v.build(n, s, ws, id)
					keys := []string{name, NotifyKey(ws, AddresseeNext)}
					p.arrive = func() {
						rearmPending(s, name)
						n.Bump(keys...) // what leave_note does after its insert
					}
					return p
				})
			})
		}
	}
}

// BenchmarkMailProbe_HarnessOverhead is the floor every number above sits on:
// the time.Now/time.Since pair and the loop around an empty probe. Subtract it
// before quoting an absolute cost for the cheap variants.
func BenchmarkMailProbe_HarnessOverhead(b *testing.B) {
	for _, sessions := range []int{1, 8} {
		b.Run(fmt.Sprintf("sessions=%d", sessions), func(b *testing.B) {
			runProbes(b, sessions, func(int) probe { return probe{call: func() {}} })
		})
	}
}

// BenchmarkMailProbe_ClaimLockContention isolates the claim itself — no gate,
// no probe, every session claiming at once — so the shape of the writer-lock
// cost is visible without the variant logic in front of it. It is the same work
// v1-naked does; the point is the p99 against the session count.
func BenchmarkMailProbe_ClaimLockContention(b *testing.B) {
	for _, sessions := range []int{1, 8} {
		b.Run(fmt.Sprintf("sessions=%d", sessions), func(b *testing.B) {
			s, ws := benchStore(b)
			seedBackground(b, s, benchBackgroundRows)
			runProbes(b, sessions, func(id int) probe {
				return newNakedProbe(s, ws, id)
			})
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
// The sweep above saturates the database — real sessions make a few tool calls
// a second, not hundreds of thousands — so its contention figures describe a
// load that never happens. This one instead arranges exactly one collision per
// call: a peer holds the writer lock (a leave_note, a share_intent, the
// reaper's prune) while this session runs its mailbox check. That does happen,
// and the question is what it costs when it does.
//
// The lock is held for only benchHoldWriteLock, so anything much above that in
// the result is SQLite's busy handler, which backs off in MILLISECONDS.
func BenchmarkMailProbe_LockCollision(b *testing.B) {
	for _, v := range variants() {
		b.Run(v.name, func(b *testing.B) {
			s, ws := benchStore(b)
			seedBackground(b, s, benchBackgroundRows)
			n := NewNotifier()
			h := &lockHolder{s: s}
			runProbes(b, 1, func(id int) probe {
				p := v.build(n, s, ws, id)
				p.arrive, p.settle = h.seize, h.release
				return p
			})
		})
	}
}
