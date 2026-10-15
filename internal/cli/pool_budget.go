package cli

import (
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/monitor"
	"github.com/plumbkit/plumb/internal/textfmt"
)

// PLAN-534: admission and budgets for the language-server pool.
//
// A configured per-language max_workspaces is a cap with hibernate-LRU
// semantics. Admission is SYNCHRONOUS: the victim's state flips to hibernating
// under the same p.mu section that admits the new or woken server, so a
// concurrent admission counts the slot correctly; only the process teardown
// runs afterwards, outside the lock. A server a call is using is never the
// victim, and an idle unpinned server goes before an idle pinned one. When every
// server of the language is busy the new one is admitted anyway — a call is
// never refused — and the language is reported over its cap until it is back
// under.
//
// Beyond the per-language caps there is a global ADVISORY soft target: the
// janitor samples the active servers and an estimated RSS (the daemon plus each
// server's own process) and warns when either is over. It is reported, never
// enforced.

const (
	// hibernatedTrimAfter is how long a server may stay hibernated before the
	// janitor releases what its entry still holds: an entry no session leases
	// is removed, a leased one drops its warm cache (the lease keeps the slot).
	hibernatedTrimAfter = 4 * time.Hour
	// softTargetActive and softTargetRSS are the advisory global target.
	softTargetActive = 12
	softTargetRSS    = 6 << 30
)

// poolBudget is the janitor's last sample of the advisory global target.
type poolBudget struct {
	active    int
	rss       uint64
	sampledAt time.Time
}

func (b poolBudget) over() bool {
	return b.active > softTargetActive || b.rss > softTargetRSS
}

// admitLocked makes room for one more running server of language under its
// per-language cap. exclude is the entry being woken (it does not count against
// the cap it is asking to join). It returns the victim's teardown, to run after
// the caller releases p.mu, or nil. Caller holds p.mu.
func (p *workspacePool) admitLocked(language string, maxRunning int, exclude *poolEntry) func() {
	if maxRunning <= 0 {
		p.setOverCapLocked(language, false, 0)
		return nil
	}
	if victim := p.overBudgetVictimLocked(language, maxRunning, exclude); victim != nil {
		slog.Info("pool: max_workspaces reached — hibernating the least recently used idle server", "language", language, "victim_root", victim.root, "max", maxRunning)
		td := p.beginHibernateLocked(victim)
		p.setOverCapLocked(language, false, maxRunning)
		return td
	}
	p.setOverCapLocked(language, p.runningCountLocked(language, exclude) >= maxRunning, maxRunning)
	return nil
}

// setOverCapLocked records whether language is admitted beyond its cap, logging
// each transition. Caller holds p.mu.
func (p *workspacePool) setOverCapLocked(language string, over bool, maxRunning int) {
	if over == p.overCap[language] {
		return
	}
	if over {
		if p.overCap == nil {
			p.overCap = make(map[string]bool)
		}
		p.overCap[language] = true
		slog.Warn("pool: every server of this language is busy — admitting one over max_workspaces", "language", language, "max", maxRunning)
		return
	}
	delete(p.overCap, language)
}

// runningCountLocked counts language's running servers, less exclude. Caller
// holds p.mu.
func (p *workspacePool) runningCountLocked(language string, exclude *poolEntry) int {
	n := 0
	for k, e := range p.entries {
		if k.language == language && e != exclude && e.state.running() {
			n++
		}
	}
	return n
}

// trimLongHibernated releases what an entry hibernated for hibernatedTrimAfter
// still holds: one no session leases is removed outright; a leased one keeps
// its slot (the lease is a session's claim on it) but drops its warm cache.
func (p *workspacePool) trimLongHibernated(now time.Time) {
	var reap []*poolEntry
	p.mu.Lock()
	for _, e := range p.entries {
		if e.state != poolHibernated || e.hibernatedAt.IsZero() || now.Sub(e.hibernatedAt) < hibernatedTrimAfter {
			continue
		}
		if e.refs == 0 {
			reap = append(reap, e)
			continue
		}
		if !e.trimmed && e.cache != nil {
			e.cache.Clear()
			e.trimmed = true
		}
	}
	p.mu.Unlock()
	for _, e := range reap {
		p.reapEntry(e)
	}
}

// sampleBudget measures the advisory global target: the active servers, and an
// estimated RSS of the daemon plus each active server's process. RSS is read
// outside p.mu (a syscall per process). Warns when the target is first exceeded.
func (p *workspacePool) sampleBudget(now time.Time) {
	var pids []int
	active := 0
	p.mu.Lock()
	for _, e := range p.entries {
		if !e.state.running() {
			continue
		}
		active++ // a server still spawning counts; it has no process to measure yet
		if e.sup != nil {
			if pid := e.sup.PID(); pid > 0 {
				pids = append(pids, pid)
			}
		}
	}
	p.mu.Unlock()
	b := poolBudget{active: active, sampledAt: now}
	for _, pid := range append(pids, os.Getpid()) {
		if rss, ok := monitor.ProcessRSS(pid); ok {
			b.rss += rss
		}
	}
	p.mu.Lock()
	was := p.budget.over()
	p.budget = b
	p.mu.Unlock()
	if b.over() && !was {
		slog.Warn("pool: over the advisory language-server target (not enforced)",
			"active", b.active, "target_active", softTargetActive,
			"est_rss", textfmt.HumanBytesCompact(b.rss), "target_rss", textfmt.HumanBytesCompact(softTargetRSS))
	}
}

// budgetSummary is daemon_info's one-line view of the pool: the active and
// hibernated servers against the advisory target, the last RSS estimate, and
// any language admitted over its cap. Per-entry detail is `plumb debug lsp`.
func (p *workspacePool) budgetSummary() string {
	p.mu.Lock()
	active, hibernated := 0, 0
	for _, e := range p.entries {
		switch {
		case e.state.running():
			active++
		case e.state == poolHibernated:
			hibernated++
		}
	}
	b := p.budget
	over := make([]string, 0, len(p.overCap))
	for lang := range p.overCap {
		over = append(over, lang)
	}
	p.mu.Unlock()
	slices.Sort(over)
	s := fmt.Sprintf("%d active, %d hibernated (advisory target %d active", active, hibernated, softTargetActive)
	if !b.sampledAt.IsZero() {
		s += fmt.Sprintf(", est. RSS %s of %s", textfmt.HumanBytesCompact(b.rss), textfmt.HumanBytesCompact(softTargetRSS))
	}
	s += ")"
	if b.over() {
		s += " — OVER the advisory target"
	}
	if len(over) > 0 {
		s += "; over max_workspaces: " + strings.Join(over, ", ")
	}
	return s
}

// lspPoolSummary is daemon_info's pool row; "" when no pool is wired.
func (s *connSession) lspPoolSummary() string {
	if s.pool == nil {
		return ""
	}
	return s.pool.budgetSummary()
}
