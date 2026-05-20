package quality

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
)

// RunnerConfig holds the per-workspace quality runner parameters resolved
// from the [quality] config block.
type RunnerConfig struct {
	// Workspace is the project root; passed to analysers so they pick up
	// project-local config (e.g. .golangci.yml).
	Workspace string
	// Analysers is the ordered list of analysers to run.
	Analysers []Analyser
	// Mode is "background" (default) or "sync".
	//   background — enqueue files and return findings on the next request.
	//   sync       — run analysers within Timeout and return findings inline.
	Mode string
	// Timeout caps each sync analyser run (default 2 s).
	Timeout time.Duration
	// MaxFindingsPerFile caps findings appended per file to keep responses
	// bounded.
	MaxFindingsPerFile int
}

func (c *RunnerConfig) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return 2 * time.Second
}

func (c *RunnerConfig) maxPerFile() int {
	if c.MaxFindingsPerFile > 0 {
		return c.MaxFindingsPerFile
	}
	return 5
}

// cachedResult stores the findings for a path keyed by file mtime. Stale
// results (mtime changed) are discarded so agents always see fresh analysis.
type cachedResult struct {
	mtime    time.Time
	findings []Finding
}

// Runner manages quality analysis for one workspace.
// Background mode: a goroutine coalesces rapid writes and runs analysers
// asynchronously. Sync mode: Report blocks until analysis completes or the
// timeout elapses.
//
// Concurrency: all public methods are safe for concurrent use.
type Runner struct {
	cfg   RunnerConfig
	mu    sync.RWMutex
	cache map[string]*cachedResult // path → latest findings
	queue chan string              // paths pending background analysis
	done  chan struct{}
}

// NewRunner creates a Runner. Call Start before using Report.
func NewRunner(cfg RunnerConfig) *Runner {
	return &Runner{
		cfg:   cfg,
		cache: make(map[string]*cachedResult),
		queue: make(chan string, 64),
		done:  make(chan struct{}),
	}
}

// Start launches the background worker goroutine. A no-op in sync mode.
func (r *Runner) Start() {
	if r.cfg.Mode == "sync" {
		return
	}
	go r.backgroundWorker()
}

// Stop shuts down the background worker. Safe to call more than once.
func (r *Runner) Stop() {
	select {
	case <-r.done:
	default:
		close(r.done)
	}
}

// Report is the entry point for write tools. It enqueues path for analysis
// and returns a formatted "code quality" section string. In sync mode the
// function blocks up to the configured timeout. In background mode it
// immediately returns any cached findings for the file (empty string if none).
//
// Returns "" when quality analysis is disabled, no analyser supports the file,
// or no findings exist.
func (r *Runner) Report(ctx context.Context, path string) string {
	if len(r.cfg.Analysers) == 0 {
		return ""
	}
	if !r.anySupports(path) {
		return ""
	}
	if r.cfg.Mode == "sync" {
		return r.syncReport(ctx, path)
	}
	r.enqueue(path)
	return r.cachedReport(path)
}

// Findings returns any cached findings for path, or nil if none are cached.
// Intended for future session_start / TUI visibility.
func (r *Runner) Findings(path string) []Finding {
	r.mu.RLock()
	defer r.mu.RUnlock()
	cr := r.cache[path]
	if cr == nil {
		return nil
	}
	if stale(path, cr.mtime) {
		return nil
	}
	return cr.findings
}

// syncReport runs analysis synchronously within the configured timeout.
func (r *Runner) syncReport(ctx context.Context, path string) string {
	if cached := r.cachedReport(path); cached != "" {
		return cached
	}
	tctx, cancel := context.WithTimeout(ctx, r.cfg.timeout())
	defer cancel()
	findings := r.runAnalysers(tctx, path)
	r.store(path, findings)
	return r.format(findings)
}

// enqueue adds path to the background queue. Drops silently if the queue is full.
func (r *Runner) enqueue(path string) {
	select {
	case r.queue <- path:
	default:
	}
}

// cachedReport returns a formatted section if fresh cached findings exist.
func (r *Runner) cachedReport(path string) string {
	r.mu.RLock()
	cr := r.cache[path]
	r.mu.RUnlock()
	if cr == nil {
		return ""
	}
	if stale(path, cr.mtime) {
		return ""
	}
	return r.format(cr.findings)
}

// runAnalysers runs every configured analyser that supports path.
func (r *Runner) runAnalysers(ctx context.Context, path string) []Finding {
	var all []Finding
	for _, a := range r.cfg.Analysers {
		if !a.Supports(path) {
			continue
		}
		findings, err := a.Analyse(ctx, []string{path})
		if err != nil {
			slog.Warn("quality: analyser error", "analyser", a.Name(), "path", path, "err", err)
			continue
		}
		all = append(all, findings...)
	}
	return all
}

// store saves findings in the cache keyed by the file's current mtime.
func (r *Runner) store(path string, findings []Finding) {
	var mtime time.Time
	if info, err := os.Stat(path); err == nil {
		mtime = info.ModTime()
	}
	r.mu.Lock()
	r.cache[path] = &cachedResult{mtime: mtime, findings: findings}
	r.mu.Unlock()
}

// format renders findings as a compact code-quality section string.
// Returns "" when there are no findings.
func (r *Runner) format(findings []Finding) string {
	capped := cap(findings, r.cfg.maxPerFile())
	if len(capped) == 0 {
		return ""
	}
	names := r.analyserNames()
	var sb strings.Builder
	fmt.Fprintf(&sb, "\ncode quality (%s):\n", names)
	for _, f := range capped {
		if f.Line > 0 {
			fmt.Fprintf(&sb, "  L%d %s: %s\n", f.Line, f.Code, f.Message)
		} else {
			fmt.Fprintf(&sb, "  %s: %s\n", f.Code, f.Message)
		}
	}
	overflow := len(findings) - len(capped)
	if overflow > 0 {
		fmt.Fprintf(&sb, "  … and %d more (see max_findings_per_file)\n", overflow)
	}
	return sb.String()
}

func (r *Runner) anySupports(path string) bool {
	for _, a := range r.cfg.Analysers {
		if a.Supports(path) {
			return true
		}
	}
	return false
}

func (r *Runner) analyserNames() string {
	names := make([]string, 0, len(r.cfg.Analysers))
	for _, a := range r.cfg.Analysers {
		names = append(names, a.Name())
	}
	return strings.Join(names, ", ")
}

// backgroundWorker processes paths from the queue in order, coalescing
// repeated writes to the same path before analysis starts.
func (r *Runner) backgroundWorker() {
	for {
		select {
		case <-r.done:
			return
		case path := <-r.queue:
			path = r.drain(path)
			tctx, cancel := context.WithTimeout(context.Background(), r.cfg.timeout())
			findings := r.runAnalysers(tctx, path)
			cancel()
			r.store(path, findings)
		}
	}
}

// drain reads additional paths from the queue until it is empty, returning
// the last path seen for each unique key. If the same path appears multiple
// times only the most recent enqueue matters; we return the last non-empty
// path consumed (falling back to initial if none follow).
func (r *Runner) drain(initial string) string {
	last := initial
	for {
		select {
		case p := <-r.queue:
			last = p
		default:
			return last
		}
	}
}

// stale reports whether the cached result is outdated because the file's
// mtime has changed since the cache was written.
func stale(path string, cachedAt time.Time) bool {
	if cachedAt.IsZero() {
		return true
	}
	info, err := os.Stat(path)
	if err != nil {
		return true
	}
	return info.ModTime().After(cachedAt)
}

// cap limits findings to max per file.
func cap(findings []Finding, max int) []Finding {
	if max <= 0 || len(findings) <= max {
		return findings
	}
	return findings[:max]
}
