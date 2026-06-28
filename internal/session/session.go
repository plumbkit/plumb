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
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

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

// Register writes a session file for this process.
// Missing fields (ID, PID, StartedAt) are filled automatically.
// Returns the session ID; call Unregister(id) (via defer) to clean up on exit.
func Register(info Info) (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating session dir: %w", err)
	}
	if info.ID == "" {
		info.ID = newID()
	}
	if info.Name == "" {
		info.Name = GenerateName()
	}
	if info.PID == 0 {
		info.PID = os.Getpid()
	}
	if info.StartedAt.IsZero() {
		info.StartedAt = time.Now()
	}
	path := filepath.Join(dir, info.ID+".json")
	if err := withSessionDirLock(dir, func() error {
		return writeSessionFileAtomic(path, info)
	}); err != nil {
		return "", fmt.Errorf("writing session file: %w", err)
	}
	return info.ID, nil
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
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) //nolint:errcheck
	return fn()
}

// writeSessionFileAtomic marshals info and writes it to path atomically (temp
// file + rename) so a concurrent reader — in this or another process — never
// observes a partially-written file. The temp file is dotfile-prefixed and ends
// in .tmp, so List and FindEnded (which match *.json) ignore it.
func writeSessionFileAtomic(path string, info Info) error {
	out, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".session-*.json.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(out); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

// Rename validates and writes a new session name for id, returning the
// normalised name that was stored.
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
			copy := info
			best = &copy
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
		entries, err := os.ReadDir(dir)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading session dir: %w", err)
		}

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
		return nil
	}); err != nil {
		return nil, err
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
