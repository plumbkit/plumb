package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

// The mailbox already gives a note a byte budget and a TTL. The stats database
// gives it neither and is never pruned, so a raw second copy there would outlive
// every guarantee the first copy was given. These tests pin the two halves of
// that: nothing body-shaped reaches telemetry, and nothing else is disturbed on
// the way.

func TestStatsToolData_DropsCollaborationBodies(t *testing.T) {
	// Shaped like a leaked credential so a partial redaction is visible rather
	// than merely shorter.
	const secret = "ghp_0123456789abcdefghijklmnopqrstuvwxyz1"

	cases := []struct {
		name       string
		tool       string
		args       string
		output     string
		wantKeys   []string // routing metadata that must survive
		wantOutput string
	}{
		{
			name:       "leave_note drops body, keeps routing",
			tool:       "leave_note",
			args:       `{"to":"Bob","conversation_id":"c1","body":"` + secret + `"}`,
			output:     "note delivered to Bob: " + secret,
			wantKeys:   []string{"to", "conversation_id"},
			wantOutput: collaborationStatsOutputOmitted,
		},
		{
			name:       "share_intent drops body, keeps path globs",
			tool:       "share_intent",
			args:       `{"body":"` + secret + `","path_globs":["internal/**"]}`,
			output:     "intent broadcast: " + secret,
			wantKeys:   []string{"path_globs"},
			wantOutput: collaborationStatsOutputOmitted,
		},
		{
			name:       "share_findings drops summary and description",
			tool:       "share_findings",
			args:       `{"summary":"` + secret + `","description":"` + secret + `","paths":["a.go"]}`,
			output:     "finding saved: " + secret,
			wantKeys:   []string{"paths"},
			wantOutput: collaborationStatsOutputOmitted,
		},
		{
			name:       "check_messages keeps args, drops the delivered body",
			tool:       "check_messages",
			args:       `{"wait_seconds":30}`,
			output:     "1 message from Bob: " + secret,
			wantKeys:   []string{"wait_seconds"},
			wantOutput: collaborationStatsOutputOmitted,
		},
		{
			name:       "session_start keeps args, drops the delivered body",
			tool:       "session_start",
			args:       `{"purpose":"audit"}`,
			output:     "## Messages\n" + secret,
			wantKeys:   []string{"purpose"},
			wantOutput: collaborationStatsOutputOmitted,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotArgs, gotOutput := statsToolData(tc.tool, json.RawMessage(tc.args), tc.output)

			if strings.Contains(gotArgs, secret) {
				t.Errorf("secret survived in args: %q", gotArgs)
			}
			if strings.Contains(gotOutput, secret) {
				t.Errorf("secret survived in output: %q", gotOutput)
			}
			if gotOutput != tc.wantOutput {
				t.Errorf("output = %q, want %q", gotOutput, tc.wantOutput)
			}

			var fields map[string]json.RawMessage
			if err := json.Unmarshal([]byte(gotArgs), &fields); err != nil {
				t.Fatalf("redacted args are not valid JSON: %v (%q)", err, gotArgs)
			}
			for _, k := range tc.wantKeys {
				if _, ok := fields[k]; !ok {
					t.Errorf("routing key %q was dropped; args = %q", k, gotArgs)
				}
			}
			for _, k := range []string{"body", "summary", "description"} {
				if _, ok := fields[k]; ok {
					t.Errorf("sensitive key %q survived; args = %q", k, gotArgs)
				}
			}
		})
	}
}

// An empty output must stay empty rather than becoming the placeholder — a row
// that recorded nothing should not read back as though something was withheld.
func TestStatsToolData_EmptyOutputStaysEmpty(t *testing.T) {
	_, gotOutput := statsToolData("leave_note", json.RawMessage(`{"to":"Bob","body":"x"}`), "")
	if gotOutput != "" {
		t.Errorf("output = %q, want empty", gotOutput)
	}
}

// Every other tool must pass through byte-identical. git especially: its
// output_text is read back for commit attribution in workspace_sessions, so a
// redaction that overreached here would silently break that feature.
func TestStatsToolData_LeavesOtherToolsAlone(t *testing.T) {
	for _, tool := range []string{"git", "read_file", "edit_file", "workspace_sessions", "find_replace"} {
		t.Run(tool, func(t *testing.T) {
			args := `{"file_path":"/tmp/a.go","body":"this is not a note"}`
			output := "abc1234 some commit subject"

			gotArgs, gotOutput := statsToolData(tool, json.RawMessage(args), output)

			if gotArgs != args {
				t.Errorf("args = %q, want unchanged %q", gotArgs, args)
			}
			if gotOutput != output {
				t.Errorf("output = %q, want unchanged %q", gotOutput, output)
			}
		})
	}
}

// Unparseable arguments for a body-bearing tool are dropped, not stored. This is
// the case where we can least prove what the blob contains, so it is also the
// case where storing it is least defensible.
func TestStatsToolData_UnparseableBodyToolArgsAreDropped(t *testing.T) {
	gotArgs, gotOutput := statsToolData("leave_note", json.RawMessage(`{not json`), "delivered")

	if gotArgs != "{}" {
		t.Errorf("args = %q, want {}", gotArgs)
	}
	if gotOutput != collaborationStatsOutputOmitted {
		t.Errorf("output = %q, want %q", gotOutput, collaborationStatsOutputOmitted)
	}
}
