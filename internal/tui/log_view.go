package tui

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"time"
)

const (
	// maxLogEntries caps the in-memory log buffer so large log files don't
	// exhaust RAM on a long-running daemon.
	maxLogEntries = 2000

	// logInitBytes is the amount of the log file read from the end on initial
	// section entry, keeping startup fast regardless of log file size.
	logInitBytes = 64 * 1024 // 64 KiB
)

// logEntry holds one parsed line from daemon.log.
//
// For slog JSON lines all structured fields are populated. For plain-text
// lines only Raw is set, and Msg is empty — callers use empty Msg to detect
// the unstructured case.
type logEntry struct {
	Time  time.Time
	Level string
	Msg   string
	Attrs map[string]string
	Raw   string // original line, kept for display and substring filtering
}

func (e logEntry) empty() bool { return e.Raw == "" }

// parseLogLine interprets line as either a slog JSON record or plain text.
// It never returns an error; unrecognised formats produce an entry with only
// Raw set.
func parseLogLine(line string) logEntry {
	line = strings.TrimRight(line, "\r\n")
	e := logEntry{Raw: line}
	if line == "" {
		return logEntry{}
	}
	if line[0] != '{' {
		return e // plain text — keep raw, skip JSON path
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		return e
	}
	if v, ok := m["time"]; ok {
		var ts string
		if json.Unmarshal(v, &ts) == nil {
			e.Time, _ = time.Parse(time.RFC3339Nano, ts)
		}
	}
	if v, ok := m["level"]; ok {
		json.Unmarshal(v, &e.Level) //nolint:errcheck
	}
	if v, ok := m["msg"]; ok {
		json.Unmarshal(v, &e.Msg) //nolint:errcheck
	}
	e.Attrs = make(map[string]string)
	for k, v := range m {
		switch k {
		case "time", "level", "msg":
		default:
			var s string
			if json.Unmarshal(v, &s) == nil {
				e.Attrs[k] = s
			} else {
				e.Attrs[k] = string(v)
			}
		}
	}
	return e
}

// readNewLogLines reads any log lines written to logPath after fromOffset and
// returns the parsed entries along with the new read position. When the file
// cannot be opened (daemon not running) it returns the original offset.
func readNewLogLines(logPath string, fromOffset int64) ([]logEntry, int64) {
	f, err := os.Open(logPath)
	if err != nil {
		return nil, fromOffset
	}
	defer f.Close()
	if fromOffset > 0 {
		if _, err := f.Seek(fromOffset, io.SeekStart); err != nil {
			return nil, fromOffset
		}
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fromOffset
	}
	newOffset := fromOffset + int64(len(data))
	var entries []logEntry
	for line := range strings.SplitSeq(string(data), "\n") {
		if e := parseLogLine(line); !e.empty() {
			entries = append(entries, e)
		}
	}
	return entries, newOffset
}

// initLogTail reads the last logInitBytes of logPath so the initial Logs view
// contains recent activity without loading the entire file. The first entry
// is discarded when we start mid-file because it is likely truncated.
func initLogTail(logPath string) ([]logEntry, int64) {
	fi, err := os.Stat(logPath)
	if err != nil {
		return nil, 0
	}
	size := fi.Size()
	fromOffset := size - logInitBytes
	fromOffset = max(fromOffset, 0)
	entries, newOffset := readNewLogLines(logPath, fromOffset)
	if fromOffset > 0 && len(entries) > 0 {
		entries = entries[1:] // discard potentially truncated first line
	}
	return entries, newOffset
}
