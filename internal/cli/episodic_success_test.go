package cli

import (
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/stats"
)

// TestBuildEpisodicDetail_ExcludesFailedWrite verifies a failed mutation is not
// reported as a modification (write count / touched file) in the episodic
// summary. Regression test for cli-1.
func TestBuildEpisodicDetail_ExcludesFailedWrite(t *testing.T) {
	calls := []stats.Call{
		{Tool: "edit_file", InputJSON: `{"file_path":"/ws/ok.go"}`, Success: true},
		{Tool: "edit_file", InputJSON: `{"file_path":"/ws/failed.go"}`, Success: false},
	}
	d := buildEpisodicDetail(calls, "/ws")
	if d.WriteN != 1 {
		t.Errorf("WriteN = %d, want 1 (the failed write must be excluded)", d.WriteN)
	}
	for _, p := range d.Touched {
		if strings.Contains(p, "failed.go") {
			t.Errorf("a failed write was reported as a touched file: %v", d.Touched)
		}
	}
}
