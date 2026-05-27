// Package session manages the registry of active plumb serve processes.
//
// Each plumb serve instance writes a JSON file to
// $XDG_DATA_HOME/plumb/sessions/<id>.json on startup and removes it on exit.
// Stale files left by crashed processes are cleaned up automatically by List.
//
// Concurrency: Register / Unregister are safe to call from any goroutine.
// List reads from the filesystem and is safe to call concurrently.
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
	ID            string    `json:"id"`
	Name          string    `json:"name,omitempty"`
	PID           int       `json:"pid"`
	DaemonVersion string    `json:"daemon_version,omitempty"`
	Language      string    `json:"language"`
	Folder        string    `json:"folder"`
	Adapter       string    `json:"adapter"`
	StartedAt     time.Time `json:"started_at"`
	// LastSeenAt is populated by List from the session file's mtime.
	// It is not stored in the JSON; Touch updates the mtime instead.
	LastSeenAt time.Time `json:"-"`
	// ExternalID is an opaque string set by the caller via session_start's
	// session_id parameter. It is persisted so FindEnded can match a
	// reconnecting agent to its previous session across plumb restarts.
	ExternalID string `json:"external_id,omitempty"`
	// EndedAt is set by Unregister instead of deleting the file. A non-zero
	// value means the session has ended; zero means it is still active.
	EndedAt       time.Time `json:"ended_at,omitempty"`
	ClientName    string    `json:"client_name,omitempty"`
	ClientVersion string    `json:"client_version,omitempty"`
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
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, info.ID+".json")
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return "", fmt.Errorf("writing session file: %w", err)
	}
	return info.ID, nil
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
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading session file: %w", err)
	}
	var info Info
	if err := json.Unmarshal(data, &info); err != nil {
		return "", fmt.Errorf("decoding session file: %w", err)
	}
	info.Name = name
	out, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encoding session file: %w", err)
	}
	if err := os.WriteFile(path, append(out, '\n'), 0o600); err != nil {
		return "", fmt.Errorf("writing session file: %w", err)
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
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var info Info
	if err := json.Unmarshal(data, &info); err != nil {
		return
	}
	fn(&info)
	out, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(path, append(out, '\n'), 0o600)
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
			Patch(info.ID, func(i *Info) { i.EndedAt = time.Now() })
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

// Dir returns the path to the session file directory.
func Dir() (string, error) {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "plumb", "sessions"), nil
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
