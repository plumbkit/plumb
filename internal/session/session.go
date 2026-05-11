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

// Info describes one active plumb serve instance.
type Info struct {
	ID            string    `json:"id"`
	PID           int       `json:"pid"`
	DaemonVersion string    `json:"daemon_version,omitempty"`
	Language      string    `json:"language"`
	Folder        string    `json:"folder"`
	Adapter       string    `json:"adapter"`
	StartedAt     time.Time `json:"started_at"`
	ClientName    string    `json:"client_name,omitempty"`
	ClientVersion string    `json:"client_version,omitempty"`
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
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return "", fmt.Errorf("writing session file: %w", err)
	}
	return info.ID, nil
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
	_ = os.WriteFile(path, append(out, '\n'), 0o644)
}

// SetClient updates the ClientName and ClientVersion fields of the session
// identified by id. No-ops silently if the session file does not exist.
func SetClient(id, clientName, clientVersion string) {
	Patch(id, func(info *Info) {
		info.ClientName = clientName
		info.ClientVersion = clientVersion
	})
}

// Unregister removes the session file for id. Errors are silently ignored
// so it is safe to use as a deferred call.
func Unregister(id string) {
	dir, err := Dir()
	if err != nil {
		return
	}
	_ = os.Remove(filepath.Join(dir, id+".json"))
}

// List returns all sessions whose processes are still running,
// sorted by StartedAt ascending. Stale files are removed automatically.
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
		if !pidAlive(info.PID) {
			_ = os.Remove(path)
			continue
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
