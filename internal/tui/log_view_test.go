package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseLogLine_JSONFormat(t *testing.T) {
	line := `{"time":"2026-05-18T15:04:05.123456789Z","level":"INFO","msg":"daemon: ready","socket":"/tmp/plumb.sock","pid":"12345"}`
	e := parseLogLine(line)

	if e.Msg != "daemon: ready" {
		t.Errorf("Msg = %q, want %q", e.Msg, "daemon: ready")
	}
	if e.Level != "INFO" {
		t.Errorf("Level = %q, want INFO", e.Level)
	}
	if e.Time.IsZero() {
		t.Error("Time should be parsed, got zero")
	}
	if e.Attrs["socket"] != "/tmp/plumb.sock" {
		t.Errorf("socket attr = %q, want /tmp/plumb.sock", e.Attrs["socket"])
	}
	if e.Raw != line {
		t.Error("Raw should equal the original line")
	}
}

func TestParseLogLine_PlainText(t *testing.T) {
	line := "2026/05/18 15:04:05 daemon started"
	e := parseLogLine(line)

	if e.Raw != line {
		t.Errorf("Raw = %q, want %q", e.Raw, line)
	}
	if e.Msg != "" {
		t.Errorf("Msg should be empty for plain text, got %q", e.Msg)
	}
	if !e.Time.IsZero() {
		t.Error("Time should be zero for plain text")
	}
}

func TestParseLogLine_Empty(t *testing.T) {
	cases := []string{"", "\r\n", "\n"}
	for _, c := range cases {
		e := parseLogLine(c)
		if !e.empty() {
			t.Errorf("parseLogLine(%q).empty() = false, want true", c)
		}
	}
}

func TestParseLogLine_InvalidJSON(t *testing.T) {
	line := `{not valid json}`
	e := parseLogLine(line)
	if e.Raw != line {
		t.Errorf("Raw = %q, want %q", e.Raw, line)
	}
	if e.Msg != "" {
		t.Error("Msg should be empty for invalid JSON")
	}
}

func TestParseLogLine_WARNLevel(t *testing.T) {
	line := `{"time":"2026-05-18T15:04:05Z","level":"WARN","msg":"rate limit approaching"}`
	e := parseLogLine(line)
	if e.Level != "WARN" {
		t.Errorf("Level = %q, want WARN", e.Level)
	}
	if e.Msg != "rate limit approaching" {
		t.Errorf("Msg = %q, want rate limit approaching", e.Msg)
	}
}

func TestReadNewLogLines_ReadsFromOffset(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")

	first := `{"time":"2026-05-18T10:00:00Z","level":"INFO","msg":"old entry"}` + "\n"
	second := `{"time":"2026-05-18T10:00:01Z","level":"INFO","msg":"new entry"}` + "\n"

	if err := os.WriteFile(path, []byte(first+second), 0o644); err != nil {
		t.Fatal(err)
	}

	// Read only from the second entry onward.
	offset := int64(len(first))
	entries, newOffset := readNewLogLines(path, offset)

	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Msg != "new entry" {
		t.Errorf("Msg = %q, want new entry", entries[0].Msg)
	}
	if newOffset != int64(len(first)+len(second)) {
		t.Errorf("newOffset = %d, want %d", newOffset, len(first)+len(second))
	}
}

func TestReadNewLogLines_FileNotFound(t *testing.T) {
	const from int64 = 42
	entries, newOffset := readNewLogLines("/nonexistent/daemon.log", from)
	if entries != nil {
		t.Error("expected nil entries for missing file")
	}
	if newOffset != from {
		t.Errorf("newOffset = %d, want unchanged %d", newOffset, from)
	}
}

func TestReadNewLogLines_MixedFormats(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")
	lines := strings.Join([]string{
		`{"time":"2026-05-18T10:00:00Z","level":"INFO","msg":"structured"}`,
		`plain text line`,
		`{"time":"2026-05-18T10:00:01Z","level":"WARN","msg":"warning"}`,
	}, "\n") + "\n"

	if err := os.WriteFile(path, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, _ := readNewLogLines(path, 0)
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}
	if entries[1].Msg != "" {
		t.Error("plain text entry should have empty Msg")
	}
	if entries[1].Raw != "plain text line" {
		t.Errorf("plain text Raw = %q", entries[1].Raw)
	}
}

func TestInitLogTail_ReadsLastChunk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")

	// Write more than logInitBytes worth of content.
	var sb strings.Builder
	for i := range logInitBytes/64 + 10 {
		fmt.Fprintf(&sb, `{"time":"2026-05-18T10:00:00Z","level":"INFO","msg":"entry %d"}`+"\n", i)
	}
	content := sb.String()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	entries, offset := initLogTail(path)
	if len(entries) == 0 {
		t.Fatal("expected entries, got none")
	}
	// The last entry should be present (recent end of the file).
	last := entries[len(entries)-1]
	if !strings.Contains(last.Raw, "entry") {
		t.Errorf("last entry Raw = %q, expected to contain 'entry'", last.Raw)
	}
	// Offset should point to end of file.
	fi, _ := os.Stat(path)
	if offset != fi.Size() {
		t.Errorf("offset = %d, want file size %d", offset, fi.Size())
	}
}

func TestInitLogTail_SmallFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")
	content := `{"time":"2026-05-18T10:00:00Z","level":"INFO","msg":"only entry"}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, offset := initLogTail(path)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Msg != "only entry" {
		t.Errorf("Msg = %q, want only entry", entries[0].Msg)
	}
	if offset != int64(len(content)) {
		t.Errorf("offset = %d, want %d", offset, len(content))
	}
}

func TestInitLogTail_FileNotFound(t *testing.T) {
	entries, offset := initLogTail("/nonexistent/daemon.log")
	if entries != nil {
		t.Error("expected nil entries")
	}
	if offset != 0 {
		t.Errorf("offset = %d, want 0", offset)
	}
}

func TestParseLogLine_TimeFormats(t *testing.T) {
	// slog uses RFC3339Nano
	ts := time.Date(2026, 5, 18, 15, 4, 5, 123456789, time.UTC)
	line := fmt.Sprintf(`{"time":%q,"level":"DEBUG","msg":"test"}`, ts.Format(time.RFC3339Nano))
	e := parseLogLine(line)
	if e.Time.IsZero() {
		t.Error("expected non-zero time")
	}
	if !e.Time.Equal(ts) {
		t.Errorf("time = %v, want %v", e.Time, ts)
	}
}
