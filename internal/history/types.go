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

// Side is one side (before or after) of a change. SHA and Size are set whenever
// Exists; Content is nil when the side is not carried (too large, or stripped
// by Prepare). A Side shares Content with its source slice: callers must not
// mutate it afterwards.
type Side struct {
	Exists  bool
	Content []byte
	SHA     []byte
	Size    int64
}

// SideFromBytes describes existing content b.
func SideFromBytes(b []byte) Side {
	sum := sha256.Sum256(b)
	s := Side{Exists: true, SHA: sum[:], Size: int64(len(b))}
	if len(b) <= HardMaxContentBytes {
		s.Content = b
	}
	return s
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
	return Side{Exists: true, SHA: h.Sum(nil), Size: n}, nil
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

//nolint:unused // consumed by writer in Task 7
func (it Item) contentBytes() int64 {
	return int64(len(it.Before.Content) + len(it.After.Content))
}
