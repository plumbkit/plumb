package sqlitex

import (
	"errors"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// IsCorrupt reports whether err is SQLite's own verdict that a file is not a
// database or that its image is malformed (SQLITE_NOTADB, SQLITE_CORRUPT, or
// one of their extended codes). A busy, locked, read-only or unopenable file is
// not corrupt: deleting it would lose data over a transient or environmental
// fault, so callers offering a destructive remedy should gate it on this.
func IsCorrupt(err error) bool {
	var se *sqlite.Error
	return errors.As(err, &se) && isCorruptCode(se.Code())
}

// isCorruptCode classifies a SQLite result code. An extended code carries its
// primary code in the low byte (SQLITE_CORRUPT_INDEX is 779, 11 | 3<<8).
func isCorruptCode(code int) bool {
	switch code & 0xff {
	case sqlite3.SQLITE_CORRUPT, sqlite3.SQLITE_NOTADB:
		return true
	}
	return false
}
