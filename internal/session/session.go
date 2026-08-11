// Package session manages the registry of active plumb serve processes.
//
// Each plumb serve instance writes a JSON file to
// $XDG_DATA_HOME/plumb/sessions/<id>.json on startup. On exit the session is
// marked ended (EndedAt) rather than deleted, so a reconnecting agent can
// inherit its previous name via FindEnded; List removes ended files after
// endedSessionGrace and marks crashed sessions (dead PID) ended. List therefore
// has filesystem write side effects — it is not a pure read.
//
// Concurrency: Register / Unregister / Patch / List are safe to call from any
// goroutine and from multiple processes at once (the daemon reaper and the TUI
// refresh both call List). Mutating operations take a session-directory flock
// before writing; every JSON write then goes through writeSessionFileAtomic
// (temp file + rename), so concurrent writers do not lose read-modify-write
// updates and concurrent readers never observe a torn file. Touch and FindEnded
// are intentionally lock-free: Touch sets only an mtime (no read-modify-write)
// and FindEnded tolerates torn reads, so neither needs the writer flock and
// both stay off the per-tool-call hot path's contention.
package session

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/plumbkit/plumb/internal/fsync"
	"github.com/plumbkit/plumb/internal/paths"
)

// endedSessionGrace is how long ended-session files are kept on disk so that
// a reconnecting agent can inherit its previous session's name via FindEnded.
const endedSessionGrace = 24 * time.Hour

// idleSessionThreshold is the duration after the last tool call at which a
// session is considered idle in the TUI. The session stays open but is shown
// with a visual marker.
const IdleSessionThreshold = 30 * time.Minute

// Info describes one active plumb serve instance.
type Info struct {
	ID            string `json:"id"`
	Name          string `json:"name,omitempty"`
	PID           int    `json:"pid"`
	DaemonVersion string `json:"daemon_version,omitempty"`
	Language      string `json:"language"`
	// DetectedLanguage is the project language inferred from root markers,
	// independent of whether an LSP adapter is attached. Language remains the
	// attached LSP language and may be "none" when the adapter is disabled or
	// unavailable.
	DetectedLanguage string `json:"detected_language,omitempty"`
	Folder           string `json:"folder"`
	Adapter          string `json:"adapter"`
	// Adapters lists every language server currently active for this session's
	// root, primary first. One root may drive several (e.g. gopls +
	// vscode-html-language-server for a Go web app); secondaries are appended as
	// they start lazily on the first file of their language. Adapter remains the
	// primary for backward compatibility.
	Adapters  []string  `json:"adapters,omitempty"`
	StartedAt time.Time `json:"started_at"`
	// LastSeenAt is populated by List from the session file's mtime.
	// It is not stored in the JSON; Touch updates the mtime instead.
	LastSeenAt time.Time `json:"-"`
	// ExternalID is an opaque string set by the caller via session_start's
	// session_id parameter. It is persisted so FindEnded can match a
	// reconnecting agent to its previous session across plumb restarts.
	ExternalID string `json:"external_id,omitempty"`
	// Purpose is an optional human-readable tag set by the caller via
	// session_start's purpose parameter (e.g. "deploy-fix"). It is purely
	// descriptive, surfaced in the TUI, daemon_info, and workspace_sessions so an
	// operator can tell concurrent sessions apart at a glance. Validated to
	// alphanumeric + hyphen, max 32 characters; empty when unset.
	Purpose string `json:"purpose,omitempty"`
	// EndedAt is set by Unregister instead of deleting the file. A non-zero
	// value means the session has ended; zero means it is still active.
	EndedAt       time.Time `json:"ended_at,omitempty"`
	ClientName    string    `json:"client_name,omitempty"`
	ClientVersion string    `json:"client_version,omitempty"`
	Health        string    `json:"health,omitempty"`
	HealthMessage string    `json:"health_message,omitempty"`
	// Synthetic is true when the workspace root was inferred by the
	// auto-attach fallback (git root or seed directory) rather than discovered
	// via a standard project marker (.plumb/, go.mod, etc.).
	Synthetic bool `json:"synthetic,omitempty"`
}

// ErrNameTaken is returned by Register and Rename when another LIVE session
// already answers to the requested name. Match it with errors.Is.
var ErrNameTaken = errors.New("session name is already in use by a live session")

// Register writes a session file for this process.
//
// Missing fields (ID, PID, StartedAt) are filled automatically, and an empty
// Name is assigned one no other live session holds. Returns the completed
// record; call Unregister(info.ID) (via defer) to clean up on exit.
//
// Name selection happens INSIDE the session-directory flock, so "is this name
// free?" and the write that claims it cannot interleave with a concurrent
// Register or Rename in this or another process. A caller-supplied Name that a
// live session already holds is refused with ErrNameTaken rather than silently
// changed — only a generated name gets disambiguated, because no caller asked
// for that particular one.
func Register(info Info) (Info, error) {
	dir, err := Dir()
	if err != nil {
		return Info{}, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Info{}, fmt.Errorf("creating session dir: %w", err)
	}
	if info.ID == "" {
		info.ID = newID()
	}
	if info.PID == 0 {
		info.PID = os.Getpid()
	}
	if info.StartedAt.IsZero() {
		info.StartedAt = time.Now()
	}
	requested := info.Name
	path := filepath.Join(dir, info.ID+".json")
	if err := withSessionDirLock(dir, func() error {
		live, err := listLocked(dir)
		if err != nil {
			return err
		}
		switch {
		case requested == "":
			info.Name = freeName(live, info.ID)
		case nameTaken(live, requested, info.ID):
			return fmt.Errorf("%w: %q", ErrNameTaken, requested)
		}
		return writeSessionFileAtomic(path, info)
	}); err != nil {
		if errors.Is(err, ErrNameTaken) {
			return Info{}, err
		}
		return Info{}, fmt.Errorf("writing session file: %w", err)
	}
	return info, nil
}

// nameDrawAttempts is how many random draws freeName makes before falling back
// to a numeric suffix. The pool is ~6k names, so against any realistic number
// of live sessions the first draw lands; the retries cost one slice scan each
// and only run in the rare collision case.
const nameDrawAttempts = 8

// freeName returns a generated name that no live session other than selfID
// holds.
func freeName(live []Info, selfID string) string {
	for range nameDrawAttempts {
		if n := generateName(); !nameTaken(live, n, selfID) {
			return n
		}
	}
	// Pigeonhole: only a live session can occupy a suffix, so at most len(live)
	// of them are taken and this loop always returns.
	base := generateName()
	for i := 2; ; i++ {
		if n := withSuffix(base, i); !nameTaken(live, n, selfID) {
			return n
		}
	}
}

// withSuffix appends "-n" to base, trimming base so the result fits
// MaxNameLength and never ends in a hyphen — NormaliseName rejects both, and a
// generated name has to survive being passed back through it.
func withSuffix(base string, n int) string {
	suffix := "-" + strconv.Itoa(n)
	if room := MaxNameLength - len(suffix); len(base) > room {
		base = strings.TrimRight(base[:room], "-")
	}
	return base + suffix
}

// nameTaken reports whether a live session other than selfID answers to name.
//
// The comparison is case-INSENSITIVE even though the mailbox matches addressees
// with SQLite's case-sensitive '='. That is deliberate: being stricter than
// delivery can only reject confusable names, never admit an ambiguous address.
func nameTaken(live []Info, name, selfID string) bool {
	for _, info := range live {
		if info.ID != selfID && strings.EqualFold(info.Name, name) {
			return true
		}
	}
	return false
}

// NameTaken reports whether a live session other than selfID already answers to
// name. It takes the session-directory flock, so it must NOT be called from
// inside withSessionDirLock — check against a listLocked result there instead.
//
// Advisory only: the answer is stale the moment it returns. Register and Rename
// re-check under the lock that performs the write, and that check is the
// authoritative one. This exists for callers that want a clearer log line, or
// to skip a write attempt they know will fail.
func NameTaken(name, selfID string) (bool, error) {
	live, err := List()
	if err != nil {
		return false, err
	}
	return nameTaken(live, name, selfID), nil
}

func withSessionDirLock(dir string, fn func() error) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(dir, ".sessions.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) //nolint:errcheck // the fd is closed on return either way, which releases the lock
	return fn()
}

// writeSessionFileAtomic marshals info and writes it to path atomically (temp
// file + rename) so a concurrent reader — in this or another process — never
// observes a partially-written file. The temp file is dotfile-prefixed and ends
// in .tmp, so List and FindEnded (which match *.json) ignore it. The temp file
// is fsynced before the rename and the directory after it, so a crash cannot
// resurrect a stale session file (the directory fsync is best-effort).
func writeSessionFileAtomic(path string, info Info) error {
	out, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	// The trailing ".tmp" is load-bearing: List and FindEnded scan this
	// directory for a ".json" suffix, so a staging file named "<x>.json" would
	// be read mid-write.
	return fsync.AtomicWrite(path, out, fsync.Options{
		TempPattern: ".session-*.json.tmp",
		Label:       "session",
	})
}

// Rename validates and writes a new session name for id, returning the
// normalised name that was stored.
//
// Refuses with ErrNameTaken when another LIVE session already answers to the
// name. Session names are mailbox addresses — collab_rows.addressee holds the
// name string — so two live sessions under one name make delivery ambiguous:
// ClaimNotes' atomic claim hands the message to whichever asks first and the
// intended recipient never sees it. Renaming to the name you already hold is
// allowed, and an ended session does not reserve its name.
//
// The check runs under the same flock as the write, so it cannot race a
// concurrent Rename or Register.
func Rename(id, name string) (string, error) {
	name, err := NormaliseName(name)
	if err != nil {
		return "", err
	}
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, id+".json")
	if err := withSessionDirLock(dir, func() error {
		live, err := listLocked(dir)
		if err != nil {
			return err
		}
		if nameTaken(live, name, id) {
			return fmt.Errorf("%w: %q", ErrNameTaken, name)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("reading session file: %w", err)
		}
		var info Info
		if err := json.Unmarshal(data, &info); err != nil {
			return fmt.Errorf("decoding session file: %w", err)
		}
		info.Name = name
		if err := writeSessionFileAtomic(path, info); err != nil {
			return fmt.Errorf("writing session file: %w", err)
		}
		return nil
	}); err != nil {
		return "", err
	}
	return name, nil
}

// Patch reads the session file for id, calls fn with a pointer to the parsed
// Info, then writes the modified Info back. No-ops silently on any error.
func Patch(id string, fn func(*Info)) {
	dir, err := Dir()
	if err != nil {
		return
	}
	path := filepath.Join(dir, id+".json")
	_ = withSessionDirLock(dir, func() error {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var info Info
		if err := json.Unmarshal(data, &info); err != nil {
			return err
		}
		fn(&info)
		return writeSessionFileAtomic(path, info)
	})
}

// SetClient updates the ClientName and ClientVersion fields of the session
// identified by id. No-ops silently if the session file does not exist.
func SetClient(id, clientName, clientVersion string) {
	Patch(id, func(info *Info) {
		info.ClientName = clientName
		info.ClientVersion = clientVersion
	})
}

// Touch updates the last-activity timestamp for a session by setting the
// mtime of its session file to now. List derives LastSeenAt from this mtime,
// so callers do not need to read the JSON to check session freshness.
//
// Touch is deliberately lock-free. It runs on the response path of every
// completed tool call, so taking the directory-wide writer flock here would
// serialise every tool call across every session and process — and queue them
// behind List's long read-parse-stat scan, which shares that lock. It is safe
// without the lock because it does no read-modify-write: it only sets the mtime
// of one file by absolute path. writeSessionFileAtomic's temp+rename means
// Chtimes can only ever observe a whole inode, never a torn file, so the worst
// case is a lost mtime bump (re-applied by the next tool call) or a transient
// ENOENT against an inode mid-rename — both harmless, and the error is already
// discarded. FindEnded likewise reads lock-free against the same writers.
func Touch(id string) {
	dir, err := Dir()
	if err != nil {
		return
	}
	now := time.Now()
	_ = os.Chtimes(filepath.Join(dir, id+".json"), now, now)
}

// SetExternalID persists an opaque external identifier (e.g. an agent
// conversation ID) on the session file. It is used by FindEnded so a
// reconnecting agent can inherit its previous session's name.
func SetExternalID(id, externalID string) {
	Patch(id, func(info *Info) {
		info.ExternalID = externalID
	})
}

// SetPurpose persists the human-readable purpose tag on the session file.
// The caller is expected to pass an already-validated value (see
// NormalisePurpose); SetPurpose itself does no validation.
func SetPurpose(id, purpose string) {
	Patch(id, func(info *Info) {
		info.Purpose = purpose
	})
}

// FindEnded looks for a recently-ended session with the given externalID.
// It scans session files for entries where ExternalID matches and either
// EndedAt is within grace, or the recorded PID is dead (crash without Unregister).
// Returns the most-recently-ended match, or nil when none is found.
func FindEnded(externalID string, grace time.Duration) *Info {
	if externalID == "" {
		return nil
	}
	dir, err := Dir()
	if err != nil {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var best *Info
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var info Info
		if err := json.Unmarshal(data, &info); err != nil {
			continue
		}
		if info.ExternalID != externalID {
			continue
		}
		// Match: ended via Unregister within grace, or daemon crashed (PID dead).
		endedAt := info.EndedAt
		if endedAt.IsZero() {
			if pidAlive(info.PID) {
				continue // still active, skip
			}
			endedAt = info.StartedAt // use start as proxy; recency check is lenient
		}
		if time.Since(endedAt) > grace {
			continue
		}
		if best == nil || endedAt.After(best.EndedAt) {
			snapshot := info
			best = &snapshot
		}
	}
	return best
}

// Unregister marks the session as ended by writing EndedAt to the session file
// instead of deleting it. The file is retained for endedSessionGrace so that
// FindEnded can match a reconnecting agent. Errors are silently ignored.
func Unregister(id string) {
	Patch(id, func(info *Info) {
		info.EndedAt = time.Now()
	})
}

// List returns all active sessions (those not yet ended), sorted by StartedAt
// ascending. LastSeenAt is populated from each file's mtime.
// Stale entries (dead PID without EndedAt) are marked ended.
// Ended sessions older than endedSessionGrace are deleted automatically.
func List() ([]Info, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	var infos []Info
	if err := withSessionDirLock(dir, func() error {
		var lerr error
		infos, lerr = listLocked(dir)
		return lerr
	}); err != nil {
		return nil, err
	}
	return infos, nil
}

// listLocked is List's body with the session-directory flock already held by
// the CALLER. It must never be called without it.
//
// The split exists because withSessionDirLock is not reentrant: it opens a
// fresh fd and takes syscall.Flock(LOCK_EX) on each call, so a nested
// acquisition from the same process blocks forever. Anything needing the live
// session list while holding the lock — the uniqueness checks in Register and
// Rename, which have to read and write under one lock to be race-free — comes
// through here rather than through List.
func listLocked(dir string) ([]Info, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading session dir: %w", err)
	}

	var infos []Info
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var info Info
		if err := json.Unmarshal(data, &info); err != nil {
			continue
		}
		if !info.EndedAt.IsZero() {
			// Ended session — keep for grace period, then remove.
			if time.Since(info.EndedAt) > endedSessionGrace {
				_ = os.Remove(path)
			}
			continue
		}
		if !pidAlive(info.PID) {
			// Daemon crashed without calling Unregister — mark ended now.
			info.EndedAt = time.Now()
			_ = writeSessionFileAtomic(path, info)
			continue
		}
		// Populate LastSeenAt from the file's mtime (Touch uses os.Chtimes).
		if fi, err := os.Stat(path); err == nil {
			info.LastSeenAt = fi.ModTime()
		}
		infos = append(infos, info)
	}

	sort.Slice(infos, func(i, j int) bool {
		return infos[i].StartedAt.Before(infos[j].StartedAt)
	})
	return infos, nil
}

// Dir returns the path to the session file directory, under plumb's data dir
// resolved by internal/paths (adrg/xdg). The error return is retained for API
// compatibility with callers; resolution no longer fails.
func Dir() (string, error) {
	return filepath.Join(paths.DataDir(), "sessions"), nil
}

// pidAlive returns true if the process with the given PID is running.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

func newID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x-%s", time.Now().UnixNano()&0xffffffffffff, hex.EncodeToString(b))
}
