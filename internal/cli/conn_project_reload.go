package cli

// conn_project_reload.go — the session half of project-config hot reload
// (PLAN-414): keeping the daemon-owned per-workspace watcher set in step with
// this session's pin, the bounded poll fallback, and the agent-facing notice
// for a collaboration-policy change. Split from conn_config.go by
// responsibility: that file owns the apply path; this one owns everything
// that decides WHEN the apply path runs again.

import (
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/paths"
)

// trackProjectWatch keeps the daemon-owned project-config watcher set in step
// with this session's pin: acquire the canonical workspace root, release the
// previous one. No-op when no manager is wired (unit tests) or the workspace
// is already the watched one. applyProjectConfig calls it on every apply, so
// the watcher set survives every path that can move the pin.
func (s *connSession) trackProjectWatch(workspace string) {
	if s.projectWatches == nil || workspace == "" {
		return
	}
	// A closing session must not re-acquire: a dispatch that captured this
	// session's reload hook just before registry removal can fire after close
	// has run, and an acquire here would leak a watcher reference nobody
	// releases. Releases stay unguarded — close cancels s.ctx BEFORE
	// releaseProjectWatch runs.
	if s.ctx != nil && s.ctx.Err() != nil {
		return
	}
	canonical := paths.Canonical(workspace)
	var prev string
	s.mutate(func(v *sessionView) {
		prev = v.projectWatchRoot
		v.projectWatchRoot = canonical
	})
	if prev == canonical {
		return
	}
	s.projectWatches.acquire(canonical)
	if prev != "" {
		s.projectWatches.release(prev)
	}
}

// releaseProjectWatch drops this session's watcher reference on close.
func (s *connSession) releaseProjectWatch() {
	if s.projectWatches == nil {
		return
	}
	var root string
	s.mutate(func(v *sessionView) {
		root = v.projectWatchRoot
		v.projectWatchRoot = ""
	})
	if root != "" {
		s.projectWatches.release(root)
	}
}

// startConfigWatcher launches the bounded POLL FALLBACK for project-config
// hot reload (PLAN-414): the daemon-owned per-workspace fsnotify watcher
// (projectConfigWatchManager) is the correctness mechanism, and this
// 30-second tick only reconciles a workspace whose watcher is absent (no
// manager wired) or has failed — it never silently replaces the watcher. The
// goroutine runs until s.ctx is cancelled (on session disconnect or daemon
// shutdown). Invoked exactly once per session via sync.Once.
func (s *connSession) startConfigWatcher() {
	s.watcherOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-s.ctx.Done():
					return
				case <-ticker.C:
					s.reconcileProjectConfig()
				}
			}
		}()
	})
}

// reconcileProjectConfig is one tick of the fallback poll. A workspace with a
// healthy watcher is skipped outright — the watcher dispatches through
// connRegistry.reloadProject promptly and the poll would be a second, hidden
// mechanism for the same job. When the watcher failed (or none is wired), the
// poll owns the workspace again and says so once, in the log, rather than
// flapping a warning every 30 seconds.
func (s *connSession) reconcileProjectConfig() {
	ws := s.workspace()
	if ws == "" {
		return
	}
	if s.projectWatches != nil && s.projectWatches.healthy(ws) {
		s.mutate(func(v *sessionView) { v.fallbackWarned = false })
		return
	}
	if s.projectWatches != nil {
		warned := false
		s.mutate(func(v *sessionView) {
			warned = v.fallbackWarned
			v.fallbackWarned = true
		})
		if !warned {
			s.log().Warn("daemon: project config watcher unhealthy — 30s poll fallback reconciling this session", "workspace", ws)
		}
	}
	s.checkAndReloadConfig()
}

// collabChangeNotice renders the one-line agent-facing notice for a change in
// the [collab] capability switches between the previously applied block and
// the new one ("" when nothing the agent can act on changed). Only the
// capability switches are named — the tuning knobs (budgets, TTLs, ceilings)
// change behaviour at the margins and would make the notice noise.
func collabChangeNotice(prev, next config.CollabConfig) string {
	switches := []struct {
		name     string
		old, new bool
	}{
		{"mailbox", prev.Mailbox, next.Mailbox},
		{"cross_project", prev.CrossProject, next.CrossProject},
		{"intents", prev.Intents, next.Intents},
		{"knowledge_handoff", prev.KnowledgeHandoff, next.KnowledgeHandoff},
		{"peer_awareness", prev.PeerAwareness, next.PeerAwareness},
	}
	var enabled, revoked []string
	for _, sw := range switches {
		switch {
		case !sw.old && sw.new:
			enabled = append(enabled, sw.name)
		case sw.old && !sw.new:
			revoked = append(revoked, sw.name)
		}
	}
	if len(enabled) == 0 && len(revoked) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("[Policy: collaboration settings changed for this session —")
	if len(enabled) > 0 {
		b.WriteString(" now enabled: ")
		b.WriteString(strings.Join(enabled, ", "))
	}
	if len(revoked) > 0 {
		if len(enabled) > 0 {
			b.WriteString(";")
		}
		b.WriteString(" now revoked: ")
		b.WriteString(strings.Join(revoked, ", "))
	}
	b.WriteString(". Takes effect immediately; see `plumb config show`.]")
	return b.String()
}

// collabPolicyNotice returns and clears the pending collaboration-policy
// notice, so it is surfaced exactly once — on the next tool result or
// session_start after the change (enrichToolOutput).
func (s *connSession) collabPolicyNotice() string {
	var n string
	s.mutate(func(v *sessionView) {
		n = v.collabNotice
		v.collabNotice = ""
	})
	return n
}
