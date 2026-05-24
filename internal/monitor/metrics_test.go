package monitor

import (
	"path/filepath"
	"testing"
	"time"
)

func TestCPUPercent(t *testing.T) {
	start := time.Unix(1_000, 0)
	got, ok := cpuPercent(start, 100*time.Millisecond, start.Add(2*time.Second), 600*time.Millisecond, true)
	if !ok {
		t.Fatal("cpuPercent returned unavailable")
	}
	if got != 25 {
		t.Fatalf("cpuPercent = %.1f, want 25.0", got)
	}
	got, ok = cpuPercent(start, 0, start.Add(time.Second), 2*time.Second, true)
	if !ok {
		t.Fatal("cpuPercent returned unavailable for saturated sample")
	}
	if got != 100 {
		t.Fatalf("cpuPercent saturated = %.1f, want 100.0", got)
	}

	for name, tt := range map[string]struct {
		previousAt      time.Time
		previousCPUTime time.Duration
		now             time.Time
		cpuTime         time.Duration
		available       bool
	}{
		"not available": {previousAt: start, now: start.Add(time.Second), available: false},
		"first sample":  {now: start.Add(time.Second), available: true},
		"clock reset":   {previousAt: start, now: start.Add(time.Second), previousCPUTime: time.Second, cpuTime: 500 * time.Millisecond, available: true},
		"zero wall":     {previousAt: start, now: start, available: true},
	} {
		t.Run(name, func(t *testing.T) {
			if _, ok := cpuPercent(tt.previousAt, tt.previousCPUTime, tt.now, tt.cpuTime, tt.available); ok {
				t.Fatal("cpuPercent returned available")
			}
		})
	}
}

func TestFormatBytes(t *testing.T) {
	for _, tt := range []struct {
		n    uint64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1 KB"},
		{42 * 1024 * 1024, "42 MB"},
		{1536 * 1024 * 1024, "1.5 GB"},
	} {
		if got := FormatBytes(tt.n); got != tt.want {
			t.Fatalf("FormatBytes(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}

func TestFormatCPU(t *testing.T) {
	for _, tt := range []struct {
		percent float64
		want    string
	}{
		{-1, "0.0%"},
		{1.25, "1.2%"},
		{9.99, "10.0%"},
		{10, "10%"},
		{123.4, "100%"},
	} {
		if got := FormatCPU(tt.percent); got != tt.want {
			t.Fatalf("FormatCPU(%f) = %q, want %q", tt.percent, got, tt.want)
		}
	}
}

func TestSnapshotRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.metrics.json")
	want := DaemonMetrics{
		SampledAt:      time.Unix(1_000, 0).UTC(),
		StartedAt:      time.Unix(500, 0).UTC(),
		PID:            123,
		RSSBytes:       42,
		RSSAvailable:   true,
		CPUPercent:     1.5,
		CPUAvailable:   true,
		HeapAllocBytes: 9,
		Goroutines:     7,
	}
	if err := WriteSnapshot(path, want); err != nil {
		t.Fatalf("WriteSnapshot: %v", err)
	}
	got, err := ReadSnapshot(path)
	if err != nil {
		t.Fatalf("ReadSnapshot: %v", err)
	}
	if got.PID != want.PID || got.RSSBytes != want.RSSBytes || got.CPUPercent != want.CPUPercent || got.Goroutines != want.Goroutines {
		t.Fatalf("ReadSnapshot = %+v, want %+v", got, want)
	}
	if !got.StartedAt.Equal(want.StartedAt) {
		t.Fatalf("ReadSnapshot StartedAt = %v, want %v", got.StartedAt, want.StartedAt)
	}
}
