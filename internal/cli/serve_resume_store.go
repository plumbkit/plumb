package cli

// serve_resume_store.go — where a `plumb serve` proxy keeps the resume credentials it
// was disclosed, so that a REPLACEMENT serve (a client relaunch, a reboot) can present
// one (docs/identity-resume-credential-design.md §3, "Proxy-side storage").
//
// The store lives under the proxy's own state directory, never in a workspace and never
// anywhere a tool can address: the credential is a bearer secret, and a workspace path
// is exactly what the path policy lets a model read. One entry is one conversation:
//
//	{conversation, scope, secret, generation}
//
// Each entry is its own file, named by a hash of its (scope, conversation), written
// atomically with mode 0600. One file per entry is what lets several serve processes on
// one host (several agents, one client) write the store at the same time without a
// shared file to lock: two processes never read-modify-write the same bytes unless they
// carry the same conversation, and then the later disclosure is the newer credential.
//
// The scope binds an entry to the daemon whose store issued it. A credential is a hash
// in the daemon's session-state database; presenting it to a daemon with a different
// database cannot match anything and, worse, can count toward a conversation's
// failed-ownership revocation there. The scope is derived from where that database
// lives, which is the one fact a proxy and its daemon share without being told.
//
// Retention. The design leaves it open (open question 7). The store is capped at
// resumeStoreMaxEntries and evicts the oldest entry by modification time, so it cannot
// grow without bound across conversations that never return. An entry is replaced by the
// next credential disclosed for its conversation; the daemon does not (yet) tell a proxy
// that it refused a credential as superseded or revoked, so a refused entry stays until
// the cap evicts it. A lost, truncated or foreign-scope entry is read as "absent", which
// is the name-only continuity that shipped before the credential existed.
//
// Nothing here logs a secret.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/fsync"
	"github.com/plumbkit/plumb/internal/paths"
	"github.com/plumbkit/plumb/internal/sessionstate"
)

const (
	// resumeStoreMaxEntries caps the store. Each entry is one conversation this host
	// served; a few hundred bytes each, so the cap is about unbounded retention, not
	// space. Generous enough that a busy host never evicts a live conversation.
	resumeStoreMaxEntries = 256

	resumeStoreVersion = 1
	resumeEntrySuffix  = ".json"
)

// resumeEntry is one stored credential.
type resumeEntry struct {
	Version      int    `json:"v"`
	Conversation string `json:"conversation"`
	Scope        string `json:"scope"`
	// Secret is the bearer secret. It is never logged.
	Secret string `json:"secret"`
	// Generation counts the credentials this store has held for the conversation. It
	// orders this store's own writes; it is not the daemon's generation number, which
	// the daemon never discloses.
	Generation int       `json:"generation"`
	Updated    time.Time `json:"updated"`
}

// resumeStore is a directory of resumeEntry files for one daemon scope. The zero value
// is not usable; a nil *resumeStore is, and holds nothing.
type resumeStore struct {
	dir   string
	scope string
	max   int
}

// newResumeStore opens (creating, and tightening to 0700) a store in dir for the daemon
// scope. It returns nil, and says so without any secret, when the directory cannot be made
// or is a symbolic link: the proxy then still strips the credential but cannot persist
// it, and the feature is inert.
func newResumeStore(dir, scope string) *resumeStore {
	if dir == "" || scope == "" {
		return nil
	}
	if err := ensurePrivateDir(dir); err != nil {
		slog.Warn("serve: resume credentials cannot be stored; serve-replacement continuity falls back to the name", "err", err)
		return nil
	}
	return &resumeStore{dir: dir, scope: scope, max: resumeStoreMaxEntries}
}

// ensurePrivateDir makes dir (0700) and returns it private. A directory that already
// exists is not trusted for its mode: MkdirAll leaves one alone, so an older plumb, a
// manual mkdir or a loose umask could have left it open to other users, and the files
// inside carry bearer secrets. It is tightened to 0700, and a symbolic link is refused,
// because a link can point the secrets anywhere, including inside a workspace.
func ensurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symbolic link; refusing to keep credentials behind one", dir)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if fi.Mode().Perm() != 0o700 {
		return os.Chmod(dir, 0o700)
	}
	return nil
}

// openDefaultResumeStore is the production store: under the proxy's own state
// directory, scoped to the daemon whose session-state database this host resolves.
func openDefaultResumeStore() *resumeStore {
	return newResumeStore(
		filepath.Join(paths.StateDir(), "serve", "resume-credentials"),
		resumeDaemonScope(sessionstate.DBPath()),
	)
}

// resumeDaemonScope names the daemon store a credential belongs to: a hash of the
// canonical path of its session-state database. Two daemons that share a database share
// a scope, which is right (they evaluate the same hashes); two that do not, do not.
func resumeDaemonScope(dbPath string) string {
	sum := sha256.Sum256([]byte("plumb-resume-scope-v1\x00" + paths.Canonical(dbPath)))
	return hex.EncodeToString(sum[:16])
}

// path is the entry's file. The conversation id is a client-supplied claim and may hold
// anything, so it never becomes a path component: the name is a hash of it and the scope.
func (s *resumeStore) path(conversation string) string {
	sum := sha256.Sum256([]byte(s.scope + "\x00" + conversation))
	return filepath.Join(s.dir, hex.EncodeToString(sum[:16])+resumeEntrySuffix)
}

// load returns the conversation's stored credential. ok is false when there is none, or
// when what is there is unreadable, from another scope or another conversation, or does
// not hold a credential-shaped secret: the entry is only ever trusted for what it says
// about itself, never for where it was found.
func (s *resumeStore) load(conversation string) (resumeEntry, bool) {
	if s == nil || conversation == "" {
		return resumeEntry{}, false
	}
	data, err := os.ReadFile(s.path(conversation))
	if err != nil {
		return resumeEntry{}, false
	}
	var e resumeEntry
	if json.Unmarshal(data, &e) != nil || e.Version != resumeStoreVersion ||
		e.Scope != s.scope || e.Conversation != conversation || !resumeTokenShape.MatchString(e.Secret) {
		return resumeEntry{}, false
	}
	return e, true
}

// put records secret as the conversation's credential and returns the entry written. A
// secret equal to the one already stored is a no-op. Atomic and 0600 by construction.
func (s *resumeStore) put(conversation, secret string) (resumeEntry, error) {
	if s == nil {
		return resumeEntry{}, errors.New("serve: no resume credential store")
	}
	if conversation == "" || !resumeTokenShape.MatchString(secret) {
		return resumeEntry{}, errors.New("serve: refusing to store a malformed resume credential entry")
	}
	prev, had := s.load(conversation)
	if had && prev.Secret == secret {
		return prev, nil
	}
	e := resumeEntry{
		Version:      resumeStoreVersion,
		Conversation: conversation,
		Scope:        s.scope,
		Secret:       secret,
		Generation:   prev.Generation + 1, // 1 when there was no previous entry
		Updated:      time.Now().UTC(),
	}
	// The entry IS the credential store, written 0600 under the proxy's private state directory.
	data, err := json.Marshal(e) //nolint:gosec // G117: persisting the credential is this file's whole purpose
	if err != nil {
		return resumeEntry{}, err
	}
	path := s.path(conversation)
	// AtomicWrite keeps an existing file's mode, which is right for config and wrong for
	// a secret: tighten a file someone else made looser before the secret goes in it.
	if _, statErr := os.Stat(path); statErr == nil {
		_ = os.Chmod(path, 0o600)
	}
	if err := fsync.AtomicWrite(path, data, fsync.Options{Mode: 0o600, Label: "serve-resume"}); err != nil {
		return resumeEntry{}, fmt.Errorf("serve: storing a resume credential: %w", err)
	}
	s.prune(path)
	return e, nil
}

// prune evicts the oldest entries beyond the cap. keep is never evicted: it is the entry
// just written, and a clock that puts it first must not delete the credential in hand.
func (s *resumeStore) prune(keep string) {
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	type aged struct {
		path string
		mod  time.Time
	}
	var all []aged
	for _, de := range ents {
		if de.IsDir() || !strings.HasSuffix(de.Name(), resumeEntrySuffix) {
			continue
		}
		info, err := de.Info()
		if err != nil {
			continue
		}
		all = append(all, aged{filepath.Join(s.dir, de.Name()), info.ModTime()})
	}
	if len(all) <= s.max {
		return
	}
	sort.Slice(all, func(i, j int) bool { return all[i].mod.Before(all[j].mod) })
	for _, a := range all[:len(all)-s.max] {
		if a.path != keep {
			_ = os.Remove(a.path)
		}
	}
}

// count is the number of entries on disk. For tests and diagnostics.
func (s *resumeStore) count() int {
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, de := range ents {
		if !de.IsDir() && strings.HasSuffix(de.Name(), resumeEntrySuffix) {
			n++
		}
	}
	return n
}
