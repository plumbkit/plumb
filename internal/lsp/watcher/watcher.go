// Package watcher tracks file-watcher glob patterns registered by a language
// server via client/registerCapability and filters DidChangeWatchedFiles events
// to only those the server actually asked to watch.
package watcher

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"

	"github.com/golimpio/plumb/internal/lsp/protocol"
)

// Filter is a thread-safe store of file-watcher glob patterns registered by
// the language server. Register and Unregister are called from the server-
// request handler; FilterEvents is called inside DidChangeWatchedFiles.
//
// Concurrency: all methods are safe for concurrent use.
type Filter struct {
	mu   sync.RWMutex
	byID map[string][]string // registration id → glob patterns
}

// Register parses a client/registerCapability params blob and stores any
// workspace/didChangeWatchedFiles watcher patterns, keyed by registration ID.
func (f *Filter) Register(raw json.RawMessage) {
	var params struct {
		Registrations []struct {
			ID              string          `json:"id"`
			Method          string          `json:"method"`
			RegisterOptions json.RawMessage `json:"registerOptions"`
		} `json:"registrations"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return
	}
	toAdd := make(map[string][]string)
	for _, reg := range params.Registrations {
		if reg.Method != protocol.MethodDidChangeWatchedFiles {
			continue
		}
		var opts struct {
			Watchers []struct {
				GlobPattern string `json:"globPattern"`
			} `json:"watchers"`
		}
		if err := json.Unmarshal(reg.RegisterOptions, &opts); err != nil {
			continue
		}
		for _, w := range opts.Watchers {
			if w.GlobPattern != "" {
				toAdd[reg.ID] = append(toAdd[reg.ID], w.GlobPattern)
			}
		}
	}
	if len(toAdd) == 0 {
		return
	}
	f.mu.Lock()
	if f.byID == nil {
		f.byID = make(map[string][]string)
	}
	for id, patterns := range toAdd {
		f.byID[id] = append(f.byID[id], patterns...)
	}
	f.mu.Unlock()
}

// Unregister parses a client/unregisterCapability params blob and removes the
// named registrations.
func (f *Filter) Unregister(raw json.RawMessage) {
	var params struct {
		Unregistrations []struct {
			ID string `json:"id"`
		} `json:"unregistrations"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return
	}
	f.mu.Lock()
	for _, u := range params.Unregistrations {
		delete(f.byID, u.ID)
	}
	f.mu.Unlock()
}

// FilterEvents returns only the events whose file URI matches at least one
// registered watcher pattern. When no patterns have been registered yet,
// all events are returned unchanged (preserving the pre-registration
// behaviour of sending everything).
func (f *Filter) FilterEvents(events []protocol.FileEvent) []protocol.FileEvent {
	f.mu.RLock()
	patterns := allPatterns(f.byID)
	f.mu.RUnlock()
	if len(patterns) == 0 {
		return events
	}
	out := make([]protocol.FileEvent, 0, len(events))
	for _, ev := range events {
		path := strings.TrimPrefix(ev.URI, "file://")
		if matchesAny(patterns, path) {
			out = append(out, ev)
		}
	}
	return out
}

func allPatterns(byID map[string][]string) []string {
	var out []string
	for _, ps := range byID {
		out = append(out, ps...)
	}
	return out
}

// matchesAny reports whether filePath matches any of the given glob patterns.
func matchesAny(patterns []string, filePath string) bool {
	base := filepath.Base(filePath)
	for _, p := range patterns {
		if matchGlob(p, filePath, base) {
			return true
		}
	}
	return false
}

// matchGlob matches an LSP glob pattern against a file path.
//
// LSP glob patterns differ from filepath.Match in two ways:
//   - `{a,b,c}` means alternation: matches a, b, or c.
//   - `**` matches zero or more path segments (not just one).
//
// We handle these by: (1) expanding alternation before matching, and
// (2) splitting on `**` and matching prefix + suffix independently.
func matchGlob(pattern, filePath, base string) bool {
	// Expand {a,b,c} alternation — recurse on each alternative.
	if alts := expandAlternation(pattern); len(alts) > 1 {
		for _, alt := range alts {
			if matchGlob(alt, filePath, base) {
				return true
			}
		}
		return false
	}

	if after, ok := strings.CutPrefix(pattern, "**/"); ok {
		// **/<suffix>: match suffix against base name (fast path).
		if matched, _ := filepath.Match(after, base); matched {
			return true
		}
		// Also try against every path suffix (handles **/subdir/*.go).
		parts := strings.SplitAfter(filePath, "/")
		for i := range parts {
			if matched, _ := filepath.Match(after, strings.Join(parts[i:], "")); matched {
				return true
			}
		}
		return false
	}

	// Pattern with ** in the middle (e.g. /abs/root/**/*.go).
	if idx := strings.Index(pattern, "/**/"); idx >= 0 {
		prefix := pattern[:idx+1] // e.g. "/abs/root/"
		suffix := pattern[idx+4:] // e.g. "*.go"
		if strings.HasPrefix(filePath, prefix) {
			rest := filePath[len(prefix):]
			// suffix must match the file's base or a relative sub-path.
			if matched, _ := filepath.Match(suffix, filepath.Base(rest)); matched {
				return true
			}
			if matched, _ := filepath.Match(suffix, rest); matched {
				return true
			}
		}
		return false
	}

	matched, _ := filepath.Match(pattern, filePath)
	return matched
}

// expandAlternation expands the first {a,b,c} group in pattern into multiple
// patterns. Returns a single-element slice when no braces are present.
func expandAlternation(pattern string) []string {
	start := strings.IndexByte(pattern, '{')
	if start < 0 {
		return []string{pattern}
	}
	end := strings.IndexByte(pattern[start:], '}')
	if end < 0 {
		return []string{pattern} // unbalanced — leave unchanged
	}
	end += start
	prefix := pattern[:start]
	suffix := pattern[end+1:]
	alts := strings.Split(pattern[start+1:end], ",")
	result := make([]string, 0, len(alts))
	for _, alt := range alts {
		result = append(result, prefix+strings.TrimSpace(alt)+suffix)
	}
	return result
}
