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
// target path matches a memory's paths glob. Names only — never memory bodies.
// Cheap and non-blocking, as required of an EnrichToolOutput hook.
func (s *connSession) enrichToolOutput(_ context.Context, name string, args json.RawMessage, text string) string {
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
	names := matchingMemoryNames(s.hintCache.memories(ws), rel, hintMaxHints(mcfg))
	if len(names) == 0 {
		return text
	}
	return text + hintBlock(names, mcfg.HintBudgetBytes)
}

func matchingMemoryNames(mems []memory.Memory, rel string, max int) []string {
	var names []string
	for _, m := range mems {
		if m.MatchesPath(rel) {
			names = append(names, m.Name)
			if len(names) >= max {
				break
			}
		}
	}
	return names
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
