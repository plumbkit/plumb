package session

import (
	"encoding/json"
	"path/filepath"
	"strings"
)

// RecordUnended reports whether a session record for id exists and has NOT been
// marked ended. Read-only on purpose: List marks crashed sessions ended and
// prunes expired files, side effects a mail receipt must not cause.
//
// It answers "disconnected, not (yet) gone", never "will come back": a session
// whose client simply quit keeps EndedAt zero until something calls Unregister or
// List, so a caller must word this as a possibility. An unknown or unreadable
// record is false.
//
// It sits beside session.go rather than inside it: that file is at the
// repository's 600-line production cap, and this probe is a self-contained read.
func RecordUnended(id string) bool {
	// The id becomes a file name, so refuse anything that could climb out of the
	// session directory. Callers pass daemon-set ids — never a model's — and this
	// costs one line to keep true even if that changes.
	if id == "" || id == ".." || strings.ContainsAny(id, `/\`) {
		return false
	}
	dir, err := Dir()
	if err != nil {
		return false
	}
	data, err := readSessionFile(filepath.Join(dir, id+".json"))
	if err != nil {
		return false
	}
	var info Info
	if err := json.Unmarshal(data, &info); err != nil {
		return false
	}
	return info.EndedAt.IsZero()
}
