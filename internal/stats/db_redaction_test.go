package stats

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// TestMigrationScrubsHistoricalCollaborationBodies proves the v17 step removes
// message content that older builds recorded, and removes ONLY that: a row for
// an ordinary tool must come through byte-identical, or the migration is a data
// loss bug rather than a privacy fix.
//
// Seeded at the full current schema and migrated from 16, so the only step that
// runs is v17 — this test cannot pass by accident through some earlier ALTER.
func TestMigrationScrubsHistoricalCollaborationBodies(t *testing.T) {
	raw, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "v16.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer raw.Close()
	if _, err := raw.Exec(schema); err != nil {
		t.Fatalf("seed schema: %v", err)
	}

	// A shape a leaked credential would have, so a partial scrub is visible.
	const secret = "ghp_0123456789abcdefghijklmnopqrstuvwxyz1"
	inputs := map[string]string{
		"leave_note":     `{"to":"Bob","body":"` + secret + `"}`,
		"share_intent":   `{"body":"` + secret + `"}`,
		"share_findings": `{"summary":"` + secret + `"}`,
		"check_messages": `{"wait_seconds":5}`,
		"session_start":  `{"purpose":"audit"}`,
		"read_file":      `{"file_path":"safe.go"}`,
	}
	for tool, input := range inputs {
		if _, err := raw.Exec(
			`INSERT INTO tool_calls (tool, called_at, input_json, output_text) VALUES (?, 1, ?, ?)`,
			tool, input, "delivered "+secret); err != nil {
			t.Fatalf("seed %s: %v", tool, err)
		}
	}

	if err := migrate(raw, 16); err != nil {
		t.Fatalf("migrate 16→%d: %v", SchemaVersion, err)
	}

	for tool, seeded := range inputs {
		var input, output string
		if err := raw.QueryRow(
			`SELECT input_json, output_text FROM tool_calls WHERE tool = ?`, tool,
		).Scan(&input, &output); err != nil {
			t.Fatalf("read %s: %v", tool, err)
		}

		if tool == "read_file" {
			if input != seeded || output != "delivered "+secret {
				t.Errorf("ordinary telemetry was altered: input=%q output=%q", input, output)
			}
			continue
		}

		if strings.Contains(input, secret) || strings.Contains(output, secret) {
			t.Errorf("%s: content survived the scrub: input=%q output=%q", tool, input, output)
		}
		if output != "" {
			t.Errorf("%s: output_text = %q, want empty", tool, output)
		}
		// The two readers carry no body in their arguments, so their input is
		// ordinary telemetry and must survive.
		switch tool {
		case "check_messages", "session_start":
			if input != seeded {
				t.Errorf("%s: input_json = %q, want it preserved as %q", tool, input, seeded)
			}
		default:
			if input != "{}" {
				t.Errorf("%s: input_json = %q, want {}", tool, input)
			}
		}
	}

	// Rows that never held a body must not have been touched at all.
	var untouched int
	if err := raw.QueryRow(
		`SELECT COUNT(*) FROM tool_calls WHERE tool = 'read_file' AND output_text != ''`,
	).Scan(&untouched); err != nil {
		t.Fatalf("count: %v", err)
	}
	if untouched != 1 {
		t.Errorf("read_file rows with output = %d, want 1", untouched)
	}
}
