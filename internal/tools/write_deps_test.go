package tools

import (
	"testing"
	"time"
)

func TestWriteDepsDynamicEditConfig(t *testing.T) {
	diagWindow := 25 * time.Millisecond
	skew := 75 * time.Millisecond
	showDiff := false
	deps := WriteDeps{
		PostWriteDiagWindowFn: func() time.Duration { return diagWindow },
		ConcurrentWriteSkewFn: func() time.Duration { return skew },
		ShowWriteDiffFn:       func() bool { return showDiff },
	}

	if got := deps.postWriteDiagWindow(); got != diagWindow {
		t.Fatalf("postWriteDiagWindow = %v, want %v", got, diagWindow)
	}
	if got := deps.concurrentWriteSkew(); got != skew {
		t.Fatalf("concurrentWriteSkew = %v, want %v", got, skew)
	}
	if got := deps.showWriteDiff(); got != showDiff {
		t.Fatalf("showWriteDiff = %v, want %v", got, showDiff)
	}

	diagWindow = -1
	skew = 150 * time.Millisecond
	showDiff = true

	if got := deps.postWriteDiagWindow(); got != diagWindow {
		t.Fatalf("postWriteDiagWindow after update = %v, want %v", got, diagWindow)
	}
	if got := deps.concurrentWriteSkew(); got != skew {
		t.Fatalf("concurrentWriteSkew after update = %v, want %v", got, skew)
	}
	if got := deps.showWriteDiff(); got != showDiff {
		t.Fatalf("showWriteDiff after update = %v, want %v", got, showDiff)
	}
}
