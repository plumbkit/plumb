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
	// unavailable. For a workspace root that IS the home directory — where
	// language discovery is deliberately skipped — it instead carries the
	// skip note naming that cause (issue #316).
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
	// ProtocolOffered is the MCP protocol revision the client offered at
	// initialize; ProtocolAnswered is the revision plumb negotiated back. Both
	// empty for a client that sent no protocolVersion (or a record that
	// predates negotiation tracking).
	ProtocolOffered  string `json:"protocol_offered,omitempty"`
	ProtocolAnswered string `json:"protocol_answered,omitempty"`
	// ClientCapabilities is the raw capabilities JSON the client advertised at
	// initialize, retained so feature support can be inspected instead of
	// assumed. Empty when the client advertised none.
	ClientCapabilities string `json:"client_capabilities,omitempty"`
	Health             string `json:"health,omitempty"`
	HealthMessage      string `json:"health_message,omitempty"`
	// Synthetic is true when the workspace root was inferred by the
	// auto-attach fallback (git root or seed directory) rather than discovered
	// via a standard project marker (.plumb/, go.mod, etc.).
	Synthetic bool `json:"synthetic,omitempty"`
}

// ErrNameTaken is returned by Register and Rename when the requested name is
// not free — either a LIVE session already answers to it, or a disconnected but
// recoverable identity has it RESERVED (see Reserved). Match it with errors.Is.
//
// The message names both, and deliberately so: it used to say "by a live
// session", which became false the moment reservations existed. An agent told a
// live session holds a name it can see is not live has no way to reason about
// the refusal at all.
var ErrNameTaken = errors.New("session name is already in use by a live session, or reserved by a session that can still reconnect")

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
	return register(info, nil)
}

// register is Register's body, parameterised by the reservations that names
// held by absent-but-recoverable sessions add to the live-uniqueness check.
// Register passes nil (no reservations, exactly the previous behaviour);
// RegisterReserved passes the caller's set. One body, so the two can never
// enforce different rules.
func register(info Info, reserved Reserved) (Info, error) {
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
	// Validate before the lock. A caller-supplied name has to clear the same
	// rules Rename enforces, or Register becomes the one door into the registry
	// that bypasses them — it would happily store the reserved "next", or a name
	// carrying a newline or colon, which internal/tools/git.go relies on being
	// impossible to keep the Plumb-Session commit trailer single-line.
	requested := info.Name
	if requested != "" {
		norm, err := NormaliseName(requested)
		if err != nil {
			return Info{}, err
		}
		requested, info.Name = norm, norm
	}
	path := filepath.Join(dir, info.ID+".json")
	if err := withSessionDirLock(dir, func() error {
		live, err := listLocked(dir)
		if err != nil {
			return err
		}
		switch {
		case requested == "":
			info.Name = freeName(live, info.ID, reserved)
		case nameTaken(live, requested, info.ID) || reserved.taken(requested, info.ID):
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
//
// It is also the single choke point every writer of HealthMessage passes
// through (Register directly, Patch — and so markBoundaryViolation — via its
// own call below), so sanitizeHealthMessage is applied here rather than in
// any one reader. HealthMessage embeds client-supplied text (a boundary
// violation quotes the offending path), and ESC is a legal byte in a POSIX
// path; a prior attempt (issue #358) stripped escapes inside one TUI renderer
// only, so the raw text still reached the terminal through the session detail
// pane and the web API, which read the same field without going through that
// renderer.
func writeSessionFileAtomic(path string, info Info) error {
	info.HealthMessage = sanitizeHealthMessage(info.HealthMessage)
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
// name, or — via RenameReserved — when a disconnected but recoverable identity
// has it reserved. Session names are mailbox addresses — collab_rows.addressee
// holds the name string — so two sessions under one name make delivery
// ambiguous: ClaimNotes' atomic claim hands the message to whichever asks first
// and the intended recipient never sees it. Renaming to the name you already
// hold is allowed. An ended session reserves its name only while its durable
// identity is still recoverable; plain Rename passes no reservations and so
// checks live sessions alone, exactly as it always did.
//
// The check runs under the same flock as the write, so it cannot race a
// concurrent Rename or Register.
func Rename(id, name string) (string, error) {
	return rename(id, name, nil)
}

// rename is Rename's body, parameterised by reservations. See register.
func rename(id, name string, reserved Reserved) (string, error) {
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
		// Read this session's file BEFORE listLocked. listLocked prunes ended
		// files past the grace window, so scanning first can delete the very file
		// this rename is about to read and turn the call into a self-inflicted
		// ENOENT reported as "reading session file".
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("reading session file: %w", err)
		}
		var info Info
		if err := json.Unmarshal(data, &info); err != nil {
			return fmt.Errorf("decoding session file: %w", err)
		}
		live, err := listLocked(dir)
		if err != nil {
			return err
		}
		if nameTaken(live, name, id) || reserved.taken(name, id) {
			return fmt.Errorf("%w: %q", ErrNameTaken, name)
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
//
// Name is not patchable. Rename owns it, because it is the only path that
// enforces validation and live-session uniqueness — and a name is a mailbox
// address, so a write primitive that could set it freely would be a door around
// the guard rather than a second guard. A callback that changes Name has that
// one field discarded; every other mutation still applies.
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
		name := info.Name
		fn(&info)
		info.Name = name
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

// SetProtocol records the initialize-time protocol negotiation on the session
// identified by id: the revision the client offered, the revision plumb
// answered, and the client's advertised capabilities as raw JSON. No-ops
// silently if the session file does not exist.
func SetProtocol(id, offered, answered, capabilities string) {
	Patch(id, func(info *Info) {
		info.ProtocolOffered = offered
		info.ProtocolAnswered = answered
		info.ClientCapabilities = capabilities
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

// ExternalIDOf returns the external-conversation ID linked to the session at id,
// or "" when the session has none, has no file, or the file cannot be read.
//
// It reads the session file rather than any cached copy on purpose: SetExternalID
// writes there, so the file is the single source of truth and a second copy is a
// second thing that can go stale. Failure is indistinguishable from "none" by
// design — every caller treats an empty result as "unknown, carry nothing",
// which is the only safe reading of an absent linkage.
func ExternalIDOf(id string) string {
	if id == "" {
		return ""
	}
	info, err := readInfo(id)
	if err != nil {
		return ""
	}
	return info.ExternalID
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
