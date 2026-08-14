package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestStatsToolDataOmitsCollaborationBodies(t *testing.T) {
	secret := "ghp_0123456789abcdefghijklmnopqrstuvwxyz1"
	oversized := strings.Repeat("x", 128*1024) + secret
	tests := []struct {
		tool string
		args string
	}{
		{"leave_note", `{"to":"Bob","conversation_id":"c1","body":"` + oversized + `"}`},
		{"share_intent", `{"path_globs":["internal/**"],"ttl_minutes":30,"body":"` + oversized + `"}`},
		{"share_findings", `{"paths":["internal/**"],"summary":"` + secret + `","description":"` + oversized + `"}`},
		{"check_messages", `{"wait_seconds":5}`},
		{"session_start", `{"purpose":"audit"}`},
	}
	for _, tc := range tests {
		input, output := statsToolData(tc.tool, json.RawMessage(tc.args), "delivered "+oversized)
		if strings.Contains(input, secret) || strings.Contains(input, oversized) {
			t.Errorf("%s stats input retained collaboration content", tc.tool)
		}
		if len(input) > 512 {
			t.Errorf("%s stats input remained oversized: %d bytes", tc.tool, len(input))
		}
		if output != collaborationStatsOutputOmitted {
			t.Errorf("%s stats output retained collaboration content: %q", tc.tool, output)
		}
	}
	input, _ := statsToolData("leave_note", json.RawMessage(`{"to":"Bob","conversation_id":"c1","body":"secret"}`), "queued")
	if !strings.Contains(input, `"to":"Bob"`) || !strings.Contains(input, `"conversation_id":"c1"`) {
		t.Fatalf("safe routing metadata was lost: %s", input)
	}
}

func TestStatsToolDataLeavesOrdinaryToolsUnchanged(t *testing.T) {
	args := json.RawMessage(`{"file_path":"x.go"}`)
	input, output := statsToolData("read_file", args, "contents")
	if input != string(args) || output != "contents" {
		t.Fatalf("ordinary stats changed: input=%q output=%q", input, output)
	}
}

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
