package cli

import (
	"fmt"
	"log/slog"
	"math"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
)

// defaultDaemonMemLimit is the soft heap ceiling applied when PLUMB_MEMORY_LIMIT
// is unset. Go's GC works harder as the heap approaches it and never hard-fails —
// a genuine spike is bounded rather than allowed to exhaust the machine. The
// daemon idles at tens of MB, so a tight-but-comfortable 1 GiB keeps the GC
// reclaiming transient parse arenas promptly (so consecutive large parses don't
// stack into the heap high-water and pin RSS) while leaving ample headroom.
// Override with PLUMB_MEMORY_LIMIT (e.g. "2GiB"), or disable with "0"/"off".
const defaultDaemonMemLimit int64 = 1 << 30 // 1 GiB

// defaultParseMemoryBudgetMB bounds a SINGLE tree-sitter parse's backing-storage
// growth (applied by gotreesitter to the node arena and the GLR scratch
// independently). GLR-heavy structural grammars (Markdown most of all, at ~200
// nodes/byte) can otherwise balloon one parse of a few-hundred-KB file to
// hundreds of MB — back-to-back during a resync that becomes the multi-GB heap
// high-water. 128 MB completes ordinary code parses with headroom while capping
// the pathological case. Honoured only if the operator has not already set
// GOT_PARSE_MEMORY_BUDGET_MB. Disable plumb's default by exporting that env to
// "0" (gotreesitter treats 0 as "no budget").
const defaultParseMemoryBudgetMB = "128"

// applyParseMemoryBudget sets a default per-parse memory budget for the bundled
// tree-sitter engine unless the operator already configured one. It must run
// before the first parse: gotreesitter memoises the env value with a sync.Once,
// so a later change would not take effect.
func applyParseMemoryBudget() {
	const envKey = "GOT_PARSE_MEMORY_BUDGET_MB"
	if _, ok := os.LookupEnv(envKey); ok {
		slog.Info("daemon: per-parse memory budget (operator-configured)", "budget_mb", os.Getenv(envKey))
		return
	}
	_ = os.Setenv(envKey, defaultParseMemoryBudgetMB)
	slog.Info("daemon: per-parse memory budget applied (default)", "budget_mb", defaultParseMemoryBudgetMB)
}

// applyMemoryLimit sets the Go runtime soft memory limit from PLUMB_MEMORY_LIMIT,
// falling back to defaultDaemonMemLimit when unset and to no limit when the value
// is "0"/"off". A malformed value keeps the default rather than failing startup.
// The chosen limit is logged so it is visible in daemon.log.
func applyMemoryLimit(raw string) {
	raw = strings.TrimSpace(raw)
	switch strings.ToLower(raw) {
	case "":
		debug.SetMemoryLimit(defaultDaemonMemLimit)
		slog.Info("daemon: soft memory limit applied (default)", "limit", formatLimit(defaultDaemonMemLimit), "source", "default")
		return
	case "0", "off", "none", "unlimited":
		debug.SetMemoryLimit(math.MaxInt64)
		slog.Info("daemon: soft memory limit disabled", "source", "PLUMB_MEMORY_LIMIT")
		return
	}

	limit, err := parseByteSize(raw)
	if err != nil || limit <= 0 {
		debug.SetMemoryLimit(defaultDaemonMemLimit)
		slog.Warn("daemon: invalid PLUMB_MEMORY_LIMIT; keeping default soft memory limit",
			"value", raw, "limit", formatLimit(defaultDaemonMemLimit))
		return
	}
	debug.SetMemoryLimit(limit)
	slog.Info("daemon: soft memory limit applied", "limit", formatLimit(limit), "source", "PLUMB_MEMORY_LIMIT")
}

func formatLimit(n int64) string {
	if n < 0 {
		return "unlimited"
	}
	return fmt.Sprintf("%d", n)
}

// parseByteSize parses a human byte size: a bare number is bytes, or a number
// with a unit suffix (B, KB/KiB, MB/MiB, GB/GiB, TB/TiB — case-insensitive).
// Both the decimal (KB) and binary (KiB) spellings map to 1024-based multiples,
// which is the convention agents expect from a "512KB" cap.
func parseByteSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	upper := strings.ToUpper(s)
	mult := int64(1)
	for _, u := range []struct {
		suffix string
		factor int64
	}{
		{"TIB", 1 << 40},
		{"TB", 1 << 40},
		{"GIB", 1 << 30},
		{"GB", 1 << 30},
		{"MIB", 1 << 20},
		{"MB", 1 << 20},
		{"KIB", 1 << 10},
		{"KB", 1 << 10},
		{"B", 1},
	} {
		if strings.HasSuffix(upper, u.suffix) {
			mult = u.factor
			upper = strings.TrimSpace(strings.TrimSuffix(upper, u.suffix))
			break
		}
	}
	n, err := strconv.ParseInt(upper, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing byte size %q: %w", s, err)
	}
	if n < 0 || n > math.MaxInt64/mult {
		return 0, fmt.Errorf("byte size %q out of range", s)
	}
	return n * mult, nil
}
