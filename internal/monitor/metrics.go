package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/golimpio/plumb/internal/config"
)

const SnapshotFileName = "daemon.metrics.json"

// DaemonMetrics is a daemon-process-only resource snapshot.
// It deliberately excludes child language-server processes.
//
// Concurrency: values are immutable after Sample or ReadSnapshot returns.
type DaemonMetrics struct {
	SampledAt         time.Time `json:"sampled_at"`
	StartedAt         time.Time `json:"started_at,omitempty"`
	PID               int       `json:"pid"`
	RSSBytes          uint64    `json:"rss_bytes,omitempty"`
	RSSAvailable      bool      `json:"rss_available"`
	VMSBytes          uint64    `json:"vms_bytes,omitempty"`
	VMSAvailable      bool      `json:"vms_available"`
	CPUPercent        float64   `json:"cpu_percent,omitempty"`
	CPUAvailable      bool      `json:"cpu_available"`
	HeapAllocBytes    uint64    `json:"heap_alloc_bytes"`
	HeapInuseBytes    uint64    `json:"heap_inuse_bytes"`
	HeapSysBytes      uint64    `json:"heap_sys_bytes"`
	HeapReleasedBytes uint64    `json:"heap_released_bytes"`
	NumGC             uint32    `json:"num_gc"`
	Goroutines        int       `json:"goroutines"`
}

// Sampler samples resource usage for one process.
// CPUPercent is computed from the previous sample, so the first sample reports
// CPUAvailable=false.
//
// Concurrency: a Sampler is not safe for concurrent use.
type Sampler struct {
	pid         int
	lastAt      time.Time
	lastCPUTime time.Duration
}

type processMetrics struct {
	RSSBytes         uint64
	RSSAvailable     bool
	VMSBytes         uint64
	VMSAvailable     bool
	CPUTime          time.Duration
	CPUTimeAvailable bool
}

func NewSampler(pid int) *Sampler {
	return &Sampler{pid: pid}
}

func SnapshotPath() string {
	return filepath.Join(config.CacheDir(), SnapshotFileName)
}

func ReadSnapshot(path string) (DaemonMetrics, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return DaemonMetrics{}, err
	}
	var m DaemonMetrics
	if err := json.Unmarshal(data, &m); err != nil {
		return DaemonMetrics{}, fmt.Errorf("decode daemon metrics: %w", err)
	}
	return m, nil
}

func (s *Sampler) Sample(_ context.Context) (DaemonMetrics, error) {
	now := time.Now()
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	out := DaemonMetrics{
		SampledAt:         now,
		PID:               s.pid,
		HeapAllocBytes:    mem.HeapAlloc,
		HeapInuseBytes:    mem.HeapInuse,
		HeapSysBytes:      mem.HeapSys,
		HeapReleasedBytes: mem.HeapReleased,
		NumGC:             mem.NumGC,
		Goroutines:        runtime.NumGoroutine(),
	}

	if pm, err := readProcessMetrics(s.pid); err == nil {
		out.RSSBytes = pm.RSSBytes
		out.RSSAvailable = pm.RSSAvailable
		out.VMSBytes = pm.VMSBytes
		out.VMSAvailable = pm.VMSAvailable
		if percent, ok := cpuPercent(s.lastAt, s.lastCPUTime, now, pm.CPUTime, pm.CPUTimeAvailable); ok {
			out.CPUPercent = percent
			out.CPUAvailable = true
		}
		if pm.CPUTimeAvailable {
			s.lastAt = now
			s.lastCPUTime = pm.CPUTime
		}
	}
	return out, nil
}

func cpuPercent(previousAt time.Time, previousCPUTime time.Duration, now time.Time, cpuTime time.Duration, available bool) (float64, bool) {
	if !available || previousAt.IsZero() {
		return 0, false
	}
	wall := now.Sub(previousAt)
	cpu := cpuTime - previousCPUTime
	if wall <= 0 || cpu < 0 {
		return 0, false
	}
	percent := cpu.Seconds() / wall.Seconds() * 100
	if percent > 100 {
		percent = 100
	}
	return percent, true
}

func WriteSnapshot(path string, metrics DaemonMetrics) error {
	data, err := json.Marshal(metrics)
	if err != nil {
		return fmt.Errorf("encode daemon metrics: %w", err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create metrics dir: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".daemon.metrics-*.tmp")
	if err != nil {
		return fmt.Errorf("create metrics temp file: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("write metrics temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close metrics temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("publish metrics snapshot: %w", err)
	}
	return nil
}

func StartSnapshotWriter(ctx context.Context, path string, interval time.Duration, startedAt time.Time) {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	sampler := NewSampler(os.Getpid())
	write := func() {
		metrics, err := sampler.Sample(ctx)
		if err == nil {
			metrics.StartedAt = startedAt
			_ = WriteSnapshot(path, metrics)
		}
	}
	write()
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		defer os.Remove(path)
		for {
			select {
			case <-ticker.C:
				write()
			case <-ctx.Done():
				return
			}
		}
	}()
}

func FormatBytes(n uint64) string {
	const unit = 1024
	switch {
	case n >= unit*unit*unit:
		return fmt.Sprintf("%.1f GB", float64(n)/(unit*unit*unit))
	case n >= unit*unit:
		return fmt.Sprintf("%.0f MB", float64(n)/(unit*unit))
	case n >= unit:
		return fmt.Sprintf("%.0f KB", float64(n)/unit)
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func FormatCPU(percent float64) string {
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	if percent >= 10 {
		return fmt.Sprintf("%.0f%%", percent)
	}
	return fmt.Sprintf("%.1f%%", percent)
}
