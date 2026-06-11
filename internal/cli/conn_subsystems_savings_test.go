package cli

import (
	"encoding/json"
	"testing"
)

func TestBatchSizeFor(t *testing.T) {
	tests := []struct {
		tool string
		args string
		want int
	}{
		{"read_multiple_files", `{"paths":["a","b","c"]}`, 3},
		{"transaction_apply", `{"operations":[{"op":1},{"op":2}]}`, 2},
		{"read_file", `{"file_path":"x"}`, 1},      // non-batching tool → 1
		{"read_multiple_files", `not json`, 1},     // unparseable args → 1
		{"read_multiple_files", `{"paths":[]}`, 0}, // empty batch
	}
	for _, tc := range tests {
		if got := batchSizeFor(tc.tool, json.RawMessage(tc.args)); got != tc.want {
			t.Errorf("batchSizeFor(%s, %s) = %d, want %d", tc.tool, tc.args, got, tc.want)
		}
	}
}
