package protocol

import (
	"encoding/json"
	"testing"
)

func TestDecodeLocations(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		wantLen  int
		wantURI  string // URI of the first result, when wantLen > 0
		wantLine uint32 // start line of the first result
	}{
		{"null", `null`, 0, "", 0},
		{"empty string", ``, 0, "", 0},
		{"empty array", `[]`, 0, "", 0},
		{
			"single Location object",
			`{"uri":"file:///a.zig","range":{"start":{"line":3,"character":4},"end":{"line":3,"character":9}}}`,
			1, "file:///a.zig", 3,
		},
		{
			"Location array",
			`[{"uri":"file:///a.swift","range":{"start":{"line":1,"character":0},"end":{"line":1,"character":2}}},` +
				`{"uri":"file:///b.swift","range":{"start":{"line":7,"character":0},"end":{"line":7,"character":2}}}]`,
			2, "file:///a.swift", 1,
		},
		{
			"LocationLink array uses targetSelectionRange",
			`[{"targetUri":"file:///c.swift","targetRange":{"start":{"line":10,"character":0},"end":{"line":20,"character":0}},` +
				`"targetSelectionRange":{"start":{"line":11,"character":6},"end":{"line":11,"character":12}}}]`,
			1, "file:///c.swift", 11,
		},
		{
			"single LocationLink object falls back to targetRange",
			`{"targetUri":"file:///d.zig","targetRange":{"start":{"line":5,"character":0},"end":{"line":9,"character":0}}}`,
			1, "file:///d.zig", 5,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			locs, err := DecodeLocations(json.RawMessage(tc.raw))
			if err != nil {
				t.Fatalf("DecodeLocations: %v", err)
			}
			if len(locs) != tc.wantLen {
				t.Fatalf("got %d locations, want %d (%v)", len(locs), tc.wantLen, locs)
			}
			if tc.wantLen == 0 {
				return
			}
			if locs[0].URI != tc.wantURI {
				t.Errorf("URI = %q, want %q", locs[0].URI, tc.wantURI)
			}
			if locs[0].Range.Start.Line != tc.wantLine {
				t.Errorf("start line = %d, want %d", locs[0].Range.Start.Line, tc.wantLine)
			}
		})
	}
}

func TestDecodeLocations_InvalidJSON(t *testing.T) {
	if _, err := DecodeLocations(json.RawMessage(`{not json`)); err == nil {
		t.Fatal("expected an error for malformed JSON")
	}
}
