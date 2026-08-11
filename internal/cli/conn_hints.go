package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/memory"
	"github.com/plumbkit/plumb/internal/paths"
	"github.com/plumbkit/plumb/internal/textfmt"
)

// hintAllowedTools is the small set of path-bearing tools whose responses are
// enriched with a relevant-memory hint. read_file dominates call volume, so the
// per-connection cache keeps the lookup cheap.
var hintAllowedTools = map[string]bool{
	"read_file": true, "edit_file": true, "write_file": true,
	"read_symbol": true, "file_outline": true,
}

const hintCacheTTL = 10 * time.Second

// memoryHintCache caches the workspace's memory list (frontmatter only) so the
// hot read_file path does not re-read every memory's frontmatter on each call.
// It refreshes when the memories directory's mtime changes or the TTL elapses.
//
// Concurrency: safe for concurrent use.
type memoryHintCache struct {
	mu      sync.Mutex
	ws      string
	mems    []memory.Memory
	builtAt time.Time
	sig     int64
}

func (c *memoryHintCache) memories(ws string) []memory.Memory {
	c.mu.Lock()
	defer c.mu.Unlock()
	sig := memoriesDirSig(ws)
	if c.ws == ws && c.sig == sig && time.Since(c.builtAt) < hintCacheTTL {
		return c.mems
	}
	mems, _ := memory.List(ws) // reads frontmatter only, never bodies
	c.ws, c.mems, c.builtAt, c.sig = ws, mems, time.Now(), sig
	return c.mems
}

func memoriesDirSig(ws string) int64 {
	st, err := os.Stat(memory.Dir(ws))
	if err != nil {
		return 0
	}
	return st.ModTime().UnixNano()
}

// mailboxSilentTools are the tools that must never carry a piggybacked message
// block. The mailbox tools themselves would double-deliver (they already claim
// and render), and session_start renders its own "## Messages" section.
var mailboxSilentTools = map[string]bool{
	"check_messages": true, "leave_note": true, "session_start": true,
	// workspace_sessions lists unread messages itself; appending the claimed
	// bodies underneath would show the same message twice in one response.
	"workspace_sessions": true,
}

// enrichToolOutput appends plumb's advisory blocks to a successful tool result.
//
// Two tiers, deliberately gated differently. Messages addressed to this session
// are appended to EVERY tool's output: a message is about the agent, not about
// the file it happens to be touching, so an agent working only through git or
// run_task must still receive it. The path-derived hints stay restricted to the
// small set of path-bearing tools, where a target file exists to match against.
//
// Cheap and non-blocking, as required of an EnrichToolOutput hook: the message
// check short-circuits on an in-process generation counter and touches the
// database only when a peer has actually written something (see conn_chat.go).
func (s *connSession) enrichToolOutput(ctx context.Context, name string, args json.RawMessage, text string) (out string) {
	// Message delivery runs LAST, and deliberately so. It is the only enrich step
	// that mutates state — claiming marks a row delivered for good — while
	// runHookSafely discards this whole string if a later step panics. Running the
	// read-only path hints first means such a panic costs a hint, not a message
	// that has already been marked as read and can never be offered again.
	//
	// The named return plus defer is what puts it after every early return below,
	// so a message is delivered on EVERY tool call rather than only on the
	// path-bearing ones the hints are restricted to.
	defer func() {
		if !mailboxSilentTools[name] {
			out += s.messageHint(ctx)
		}
	}()
	if !hintAllowedTools[name] {
		return text
	}
	ws := s.view().acquiredRoot
	if ws == "" {
		return text
	}
	// Three independent path-derived signals, each self-gated on its own config:
	// the relevant-memory hint ([memory] inject_hints), the phase-1 peer-activity
	// hint ([collab] peer_awareness, an observed fact), and the phase-2 peer-intent
	// hint ([collab] intents, an unverified claim). All are advisory,
	// byte-budgeted, and appended after the tool's own output.
	text += s.memoryHint(ctx, name, args, ws)
	text += s.peerHint(args, ws)
	text += s.intentHint(args, ws)
	return text
}

// memoryHint returns the relevant-memory "[Hint: …]" block for the tool's target
// path, or "" when memory-hint injection is off, no path applies, or nothing
// matches. Extracted from enrichToolOutput so the peer-activity hint can be
// gated independently of [memory] inject_hints. Candidates are relevance-
// filtered by tool and target class before matching (hintEligibleMemories),
// and generated survivors are labelled at render time (labelGeneratedHints).
func (s *connSession) memoryHint(ctx context.Context, name string, args json.RawMessage, ws string) string {
	mcfg := s.memoryConfig()
	if !mcfg.InjectHints {
		return ""
	}
	rel := hintRelPath(ws, args)
	if rel == "" {
		return ""
	}
	var syms map[string]bool
	if name == "edit_file" || name == "write_file" {
		syms = s.editedFileSymbols(ctx, ws, rel)
	}
	class := hintClassFor(rel)
	mems := hintEligibleMemories(s.hintCache.memories(ws), name, class)
	names := s.unseenHints(matchingMemoryNames(mems, rel, syms), hintMaxHints(mcfg))
	if len(names) == 0 {
		return ""
	}
	return hintBlock(labelGeneratedHints(names, mems), hintEffectiveBudget(mcfg.HintBudgetBytes, class))
}

// hintClass buckets a hint target file by the kind of content it holds, so the
// hint filter can be stricter where auto-generated memories are usually noise.
type hintClass int

const (
	hintClassSource hintClass = iota // code and everything unclassified: full budget, read-skip only
	hintClassProse                   // .md/.txt/.rst: episodic skipped for any tool, half budget
	hintClassConfig                  // .json/.toml/.yaml/.yml/.lock: user-authored memories only
)

// hintClassFor classifies the workspace-relative target path by extension.
// Prose and config/data files attract path-glob matches from episodic session
// summaries (a prior session that wrote the file seeded its paths: globs) that
// carry no task relevance, so they get stricter filtering and — for prose — a
// smaller byte budget.
func hintClassFor(rel string) hintClass {
	switch strings.ToLower(filepath.Ext(rel)) {
	case ".md", ".txt", ".rst":
		return hintClassProse
	case ".json", ".toml", ".yaml", ".yml", ".lock":
		return hintClassConfig
	default:
		return hintClassSource
	}
}

// hintEligibleMemories filters mems down to those worth hinting for this tool
// and target class. Episodic session summaries are skipped on read_file (pure
// reads gain nothing from a past session's activity log) but kept for the
// mutation tools, where prior-session context can matter when editing; on a
// prose target they are skipped for every tool, and on a config/data target
// only user-authored memories survive.
func hintEligibleMemories(mems []memory.Memory, tool string, class hintClass) []memory.Memory {
	var out []memory.Memory
	for _, m := range mems {
		if hintEligible(m, tool, class) {
			out = append(out, m)
		}
	}
	return out
}

func hintEligible(m memory.Memory, tool string, class hintClass) bool {
	switch class {
	case hintClassConfig:
		return m.UserAuthored()
	case hintClassProse:
		return !m.Episodic()
	default:
		return tool != "read_file" || !m.Episodic()
	}
}

// hintEffectiveBudget returns the per-call byte budget for the hint block. The
// configured [memory] hint_budget_bytes is untouched — a prose target simply
// earns half of it, since hints beside prose reads are the noisiest case.
// budget <= 0 means unbounded and stays unbounded.
func hintEffectiveBudget(budget int, class hintClass) int {
	if class != hintClassProse || budget <= 0 {
		return budget
	}
	if half := budget / 2; half > 0 {
		return half
	}
	return budget
}

// labelGeneratedHints prefixes each generated (non-user-authored) memory name
// with "(generated) " so agents can deprioritise it at a glance. Labelling
// happens at render time only — suppression tracking and matching stay on the
// raw names — and the label counts against the byte budget like any other text.
func labelGeneratedHints(names []string, mems []memory.Memory) []string {
	gen := make(map[string]bool, len(mems))
	for _, m := range mems {
		if !m.UserAuthored() {
			gen[m.Name] = true
		}
	}
	out := make([]string, len(names))
	for i, n := range names {
		if gen[n] {
			out[i] = "(generated) " + n
		} else {
			out[i] = n
		}
	}
	return out
}

// unseenHints filters names down to those not yet hinted this session, caps
// the result at limit, and records only the survivors as seen. Suppression runs
// BEFORE the cap so an already-hinted memory frees its slot for the next
// unseen match instead of permanently blocking everything ranked below it.
// Clearing happens on re-pin (clearHintSeen), so a new project starts fresh.
func (s *connSession) unseenHints(names []string, limit int) []string {
	s.hintSeenMu.Lock()
	defer s.hintSeenMu.Unlock()
	if s.hintSeen == nil {
		s.hintSeen = make(map[string]bool)
	}
	var out []string
	for _, n := range names {
		if s.hintSeen[n] {
			continue
		}
		s.hintSeen[n] = true
		out = append(out, n)
		if len(out) == limit {
			break
		}
	}
	return out
}

// clearHintSeen resets the once-per-session hint suppression; called on
// re-pin so memories of the new project hint normally. The message-generation
// cache is reset alongside it: the new project has a different collab.db, and
// the previous project's counters say nothing about what is waiting there.
func (s *connSession) clearHintSeen() {
	s.hintSeenMu.Lock()
	s.hintSeen = nil
	s.hintSeenMu.Unlock()
	if s.chatWatch != nil {
		s.chatWatch.reset()
	}
}

// editedFileSymbols returns the symbol names the topology index records in
// the edited file. Only consulted for mutation tools — read_file dominates
// call volume and stays path-glob-only; one indexed query per edit is cheap.
func (s *connSession) editedFileSymbols(ctx context.Context, ws, rel string) map[string]bool {
	store := s.topologyStoreLive()
	if store == nil {
		return nil
	}
	nodes, err := store.SymbolsInFile(ctx, filepath.Join(ws, rel))
	if err != nil || len(nodes) == 0 {
		return nil
	}
	set := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		set[n.Name] = true
	}
	return set
}

// matchingMemoryNames returns every memory name whose paths globs match rel,
// or whose provenance source_symbols intersect syms (nil syms skips the
// symbol pass), user-authored first. The hint cap is applied by unseenHints
// AFTER suppression filtering — capping here would let seen memories
// permanently block unseen ones ranked below them. User-authored memories
// always come before generated ones — every idle session can mint an
// episodic-* memory attached to the same hot files, and those must never
// crowd a hand-written note out of the capped hint block.
func matchingMemoryNames(mems []memory.Memory, rel string, syms map[string]bool) []string {
	var user, generated []string
	for _, m := range mems {
		if !m.MatchesPath(rel) && !referencesAnySymbol(m, syms) {
			continue
		}
		if m.UserAuthored() {
			user = append(user, m.Name)
		} else {
			generated = append(generated, m.Name)
		}
	}
	return append(user, generated...)
}

// referencesAnySymbol reports whether any of m's provenance source_symbols is
// in syms — comparing both the stored form and its base segment, because
// symbol-query args (the provenance source) may use the dotted
// ReceiverType.MethodName form while topology node names are bare.
func referencesAnySymbol(m memory.Memory, syms map[string]bool) bool {
	for _, sym := range m.SourceSymbols {
		if syms[sym] || syms[memory.SymbolBase(sym)] {
			return true
		}
	}
	return false
}

// hintRelPath extracts the tool's target file path (file_path / path / uri) and
// returns it relative to ws, or "" when there is no in-workspace path argument.
func hintRelPath(ws string, args json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(args, &m) != nil {
		return ""
	}
	var raw string
	for _, key := range []string{"file_path", "path", "uri"} {
		if v, ok := m[key].(string); ok && v != "" {
			raw = v
			break
		}
	}
	if raw == "" {
		return ""
	}
	abs := paths.URIToPath(raw)
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(ws, abs)
	}
	rel, err := filepath.Rel(ws, abs)
	// Reject only a genuine escape (".." or "../…"); an in-workspace dir literally
	// named "..config" must still hint, so don't match on a bare ".." prefix.
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	return filepath.ToSlash(rel)
}

func hintMaxHints(m config.MemoryConfig) int {
	if m.MaxHints > 0 {
		return m.MaxHints
	}
	return 3
}

func hintBlock(names []string, budget int) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = "'" + n + "'"
	}
	noun := "memory"
	if len(names) > 1 {
		noun = "memories"
	}
	block := fmt.Sprintf("\n\n[Hint: relevant %s attached to this path: %s — call read_memory to view.]",
		noun, strings.Join(quoted, ", "))
	return textfmt.ClampBytes(block, budget)
}
