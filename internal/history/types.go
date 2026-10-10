// Package history records every file change plumb makes as a timestamped,
// compressed diff in a global SQLite database (history.db), linked by call_id
// to the tool call that made it. Design: private spec
// 2026-10-01-write-diff-history-design.md.
package history

import (
	"crypto/sha256"
	"errors"
	"io"
	"io/fs"
	"os"
	"time"
)

// Op is what a change did to its path.
type Op string

// Change operations (the changes.op CHECK set).
const (
	OpCreate Op = "create"
	OpUpdate Op = "update"
	OpDelete Op = "delete"
	OpRename Op = "rename"
	OpCopy   Op = "copy"
	OpRevert Op = "revert"
)

// Kind distinguishes a file change from an empty-directory removal.
type Kind string

// Path kinds.
const (
	KindFile Kind = "file"
	KindDir  Kind = "dir"
)

// Content says what the row's diff column holds.
type Content string

// Content markers (the changes.content CHECK set).
const (
	ContentDiff      Content = "diff"
	ContentNone      Content = "none"
	ContentSensitive Content = "withheld:sensitive"
	ContentBinary    Content = "withheld:binary"
	ContentTooLarge  Content = "withheld:too_large"
	ContentOverflow  Content = "withheld:overflow"
)

// HardMaxContentBytes caps the content a Side ever carries; [history]
// max_content_bytes can lower it, never raise it.
const HardMaxContentBytes = 8 << 20

// Side is one side (before or after) of a change. Size is set whenever Exists;
// Content is nil when the side is not carried (too large, or stripped by
// Prepare). A Side shares its bytes with its source slice: callers must not
// mutate them afterwards.
//
// The hash is LAZY. Every write site builds its Sides whether or not history
// is on, and a disabled call must not pay for hashing (spec §6.1); so
// SideFromBytes only records the bytes, SHA computes on demand, and settled —
// applied by Prepare, Store.Enqueue and marker, which only run when history is
// on — fixes the hash before anything strips the content it is computed from.
type Side struct {
	Exists  bool
	Content []byte
	Size    int64
	sha     []byte // fixed by settled or SideFromFile; computed on demand until then
	raw     []byte // an uncarried (too large) side's bytes, kept only until settled
	// unhashed marks a side built from a stat alone (SideStat): it exists and has a
	// size, and the call that built it deliberately did not read the file. SHA
	// returns nil for it rather than the empty input's digest, which would be a hash
	// nobody computed.
	unhashed bool
}

// SHA returns the side's sha256, or nil when the side does not exist or was never
// read (SideStat).
func (s Side) SHA() []byte {
	if !s.Exists || s.sha != nil || s.unhashed {
		return s.sha
	}
	src := s.Content
	if src == nil {
		src = s.raw
	}
	sum := sha256.Sum256(src)
	return sum[:]
}

// settled returns s with its hash fixed and an uncarried side's bytes released.
// Idempotent. Anything that strips a Side's content must settle it first, or
// the hash would be computed from nothing.
func settled(s Side) Side {
	if s.Exists && s.sha == nil {
		s.sha = s.SHA()
	}
	s.raw = nil
	return s
}

// SideFromBytes describes existing content b, without hashing it (see Side).
func SideFromBytes(b []byte) Side {
	s := Side{Exists: true, Size: int64(len(b))}
	if len(b) <= HardMaxContentBytes {
		s.Content = b
	} else {
		s.raw = b
	}
	return s
}

// SideStat describes a file by its stat alone: it exists and is this large, and
// the call that asked did not read it. It is the side a RESPONSE needs when it has
// already decided not to render the bytes — a file over the response diff cap,
// with history off — and paying an 8 MiB read plus a full hash only to print "diff
// withheld: file too large" is the waste PLAN-457 removed.
//
// SHA returns nil: the side carries no hash, and reporting the empty input's digest
// would claim a digest nobody computed.
func SideStat(size int64) Side {
	return Side{Exists: true, Size: size, unhashed: true}
}

// SideFromFile describes path's current content: Exists false when it is
// absent, content carried up to HardMaxContentBytes, hashed by streaming above
// that so a huge file never sits in memory.
func SideFromFile(path string) (Side, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Side{}, nil
	}
	if err != nil {
		return Side{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return Side{}, err
	}
	if info.Size() <= HardMaxContentBytes {
		b, err := io.ReadAll(f)
		if err != nil {
			return Side{}, err
		}
		return SideFromBytes(b), nil
	}
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return Side{}, err
	}
	return Side{Exists: true, sha: h.Sum(nil), Size: n}, nil
}

// Change is what a write site reports, under its per-path lock, after the write
// succeeded. At is stamped by the recorder.
type Change struct {
	At             time.Time
	Op             Op
	Kind           Kind
	Tool           string
	Path, From     string // absolute; From for rename/copy
	Before, After  Side
	RevertsOwnCall bool   // revert of this call's latest row for Path
	RevertsSHA     []byte // undo_edit: sha of what plumb wrote
	Reason         string // revert reason
}

// Item is a Change enriched with the connection's identity — the queue element.
type Item struct {
	Change
	CallID, SessionID, SessionName, LogicalAgent, ClientName string
	Workspace                                                string
	// Content is "" when the writer classifies; Prepare pre-sets it for
	// sensitive, too-large and no-diff-needed items, and Enqueue for overflow.
	Content        Content
	Added, Removed int // pre-computed for sensitive items
}

func (it Item) contentBytes() int64 {
	return int64(len(it.Before.Content) + len(it.After.Content))
}
