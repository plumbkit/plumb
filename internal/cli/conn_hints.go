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

// enrichToolOutput appends a "[Hint: relevant memory …]" block when the tool's
// target path matches a memory's paths glob — and, for mutation tools, when a
// memory's provenance references a symbol the topology index records in the
// edited file. Names only — never memory bodies. Each memory is hinted at most
// once per session (cleared on re-pin): after the first pointer, repeats on
// every read of the same path are noise. Cheap and non-blocking, as required
// of an EnrichToolOutput hook.
func (s *connSession) enrichToolOutput(ctx context.Context, name string, args json.RawMessage, text string) string {
	if !hintAllowedTools[name] {
		return text
	}
	ws := s.view().acquiredRoot
	if ws == "" {
		return text
	}
	mcfg := s.memoryConfig()
	if !mcfg.InjectHints {
		return text
	}
	rel := hintRelPath(ws, args)
	if rel == "" {
		return text
	}
	var syms map[string]bool
	if name == "edit_file" || name == "write_file" {
		syms = s.editedFileSymbols(ctx, ws, rel)
	}
	names := s.unseenHints(matchingMemoryNames(s.hintCache.memories(ws), rel, syms), hintMaxHints(mcfg))
	if len(names) == 0 {
		return text
	}
	return text + hintBlock(names, mcfg.HintBudgetBytes)
}

// unseenHints filters names down to those not yet hinted this session, caps
// the result at max, and records only the survivors as seen. Suppression runs
// BEFORE the cap so an already-hinted memory frees its slot for the next
// unseen match instead of permanently blocking everything ranked below it.
// Clearing happens on re-pin (clearHintSeen), so a new project starts fresh.
func (s *connSession) unseenHints(names []string, max int) []string {
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
		if len(out) == max {
			break
		}
	}
	return out
}

// clearHintSeen resets the once-per-session hint suppression; called on
// re-pin so memories of the new project hint normally.
func (s *connSession) clearHintSeen() {
	s.hintSeenMu.Lock()
	s.hintSeen = nil
	s.hintSeenMu.Unlock()
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
	abs := strings.TrimPrefix(raw, "file://")
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
	return clampBytes(block, budget)
}
