package cli

// serve_resume_store_test.go — the proxy's credential store: private, atomic, bounded and
// bound to its daemon scope.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestResumeStore_RoundTripAndGeneration(t *testing.T) {
	t.Parallel()
	s := rcStore(t, t.TempDir(), "scope-a")
	if _, ok := s.load("conv"); ok {
		t.Fatal("an empty store returned an entry")
	}
	e1, err := s.put("conv", rcSecret(1))
	if err != nil || e1.Generation != 1 {
		t.Fatalf("first put = %+v, %v; want generation 1", e1, err)
	}
	if again, _ := s.put("conv", rcSecret(1)); again.Generation != 1 {
		t.Errorf("storing the same secret again moved the generation to %d", again.Generation)
	}
	e2, _ := s.put("conv", rcSecret(2))
	got, ok := s.load("conv")
	if !ok || got.Secret != rcSecret(2) || got.Generation != 2 || e2.Generation != 2 || got.Conversation != "conv" {
		t.Fatalf("load = %+v (found %v); want conv at generation 2 holding the second secret", got, ok)
	}
	if _, ok := s.load("other"); ok {
		t.Error("another conversation resolved to conv's entry")
	}
}

// The credential is a bearer secret: the file is private to the user, and so is the
// directory it lives in. A file someone left looser is tightened before the secret goes in.
func TestResumeStore_FilesArePrivate(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits")
	}
	dir := filepath.Join(t.TempDir(), "state", "resume")
	s := rcStore(t, dir, "scope-a")
	if _, err := s.put("conv", rcSecret(1)); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("store directory mode = %v (%v), want 0700", fi.Mode().Perm(), err)
	}
	path := s.path("conv")
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("entry mode = %v (%v), want 0600", fi.Mode().Perm(), err)
	}

	// fsync.AtomicWrite keeps an existing file's mode; for a secret that would keep a
	// world-readable one world-readable.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.put("conv", rcSecret(2)); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("a rewrite left the entry at mode %v, want 0600", fi.Mode().Perm())
	}
}

// N3. A store directory that already exists is not trusted for its mode, and one that is
// a symbolic link is not used at all: the secrets would go wherever the link points.
func TestResumeStore_TightensAnExistingDirectoryAndRefusesASymlink(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits")
	}
	loose := filepath.Join(t.TempDir(), "resume")
	if err := os.Mkdir(loose, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(loose, 0o755); err != nil { // not subject to the umask
		t.Fatal(err)
	}
	if s := newResumeStore(loose, "scope-a"); s == nil {
		t.Fatal("an existing directory was refused")
	}
	if fi, _ := os.Stat(loose); fi.Mode().Perm() != 0o700 {
		t.Errorf("an existing 0755 store directory was left at %v, want 0700", fi.Mode().Perm())
	}

	elsewhere := t.TempDir()
	link := filepath.Join(t.TempDir(), "resume")
	if err := os.Symlink(elsewhere, link); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	if s := newResumeStore(link, "scope-a"); s != nil {
		t.Fatal("a symbolic-linked store directory was used")
	}
	if ents, _ := os.ReadDir(elsewhere); len(ents) != 0 {
		t.Errorf("the link target holds %d entries", len(ents))
	}
}

// A conversation id is a client-supplied claim. It never becomes a path.
func TestResumeStore_ConversationIDNeverBecomesAPath(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	s := rcStore(t, filepath.Join(root, "store"), "scope-a")
	for _, conv := range []string{"../../escape", "a/b", `a\b`, "..", strings.Repeat("x", 4096), "with\x00nul"} {
		if _, err := s.put(conv, rcSecret(1)); err != nil {
			t.Fatalf("put(%q): %v", conv, err)
		}
		if p := s.path(conv); filepath.Dir(p) != s.dir {
			t.Errorf("path(%q) = %q escaped the store directory", conv, p)
		}
		if e, ok := s.load(conv); !ok || e.Conversation != conv {
			t.Errorf("load(%q) = %+v (found %v)", conv, e, ok)
		}
	}
	if matches, _ := filepath.Glob(filepath.Join(root, "*")); len(matches) != 1 {
		t.Errorf("the store wrote outside its directory: %v", matches)
	}
}

// An entry is bound to the daemon whose store issued it.
func TestResumeStore_BoundToItsDaemonScope(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a, b := rcStore(t, dir, "scope-a"), rcStore(t, dir, "scope-b")
	if _, err := a.put("conv", rcSecret(1)); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.load("conv"); ok {
		t.Fatal("an entry written for one daemon scope resolved under another")
	}
	if _, ok := a.load("conv"); !ok {
		t.Fatal("the entry vanished from its own scope")
	}
	// Even a file moved to where the other scope would look is read for what it says.
	if err := os.Rename(a.path("conv"), b.path("conv")); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.load("conv"); ok {
		t.Error("an entry whose scope field names another daemon was trusted because of where it was found")
	}
}

// A lost, truncated or hand-edited entry is "absent": the replacement resumes by name, as
// it did before the credential existed.
func TestResumeStore_DamagedEntriesAreAbsent(t *testing.T) {
	t.Parallel()
	s := rcStore(t, t.TempDir(), "scope-a")
	good, _ := s.put("conv", rcSecret(1))
	for name, body := range map[string]string{
		"empty":            ``,
		"truncated":        `{"v":1,"conversation":"conv","scope":"scope-a","secret":"rsk1-0000`,
		"not json":         `rsk1-0000000000000000000001`,
		"wrong version":    strings.Replace(mustJSON(t, good), `"v":1`, `"v":2`, 1),
		"malformed secret": strings.Replace(mustJSON(t, good), good.Secret, "hunter2", 1),
		"other conv":       strings.Replace(mustJSON(t, good), `"conv"`, `"elsewhere"`, 1),
	} {
		if err := os.WriteFile(s.path("conv"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if e, ok := s.load("conv"); ok {
			t.Errorf("%s: a damaged entry was trusted: %+v", name, e)
		}
	}
	if _, err := s.put("conv", "hunter2"); err == nil {
		t.Error("put accepted a value that is not a credential")
	}
}

// Writers and readers on one entry never see a partial file: a read is absent or whole.
func TestResumeStore_WritesAreAtomic(t *testing.T) {
	t.Parallel()
	s := rcStore(t, t.TempDir(), "scope-a")
	if _, err := s.put("conv", rcSecret(0)); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	var torn sync.Map
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				data, err := os.ReadFile(s.path("conv"))
				if err != nil {
					continue // briefly absent is allowed; torn is not
				}
				var e resumeEntry
				if json.Unmarshal(data, &e) != nil || !resumeTokenShape.MatchString(e.Secret) {
					torn.Store(string(data), true)
				}
			}
		}()
	}
	for i := 1; i <= 200; i++ {
		if _, err := s.put("conv", rcSecret(i)); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
	torn.Range(func(k, _ any) bool {
		t.Errorf("a reader saw a partial entry: %q", k)
		return true
	})
	if e, ok := s.load("conv"); !ok || e.Secret != rcSecret(200) || e.Generation != 201 {
		t.Errorf("final entry = %+v (found %v), want the last write", e, ok)
	}
	// No staging file is left behind.
	if ents, _ := os.ReadDir(s.dir); len(ents) != 1 {
		names := make([]string, 0, len(ents))
		for _, de := range ents {
			names = append(names, de.Name())
		}
		t.Errorf("store directory holds %v, want exactly one entry", names)
	}
}

// The store is capped. It evicts the oldest entry, never the one just written.
func TestResumeStore_IsCappedAndEvictsTheOldest(t *testing.T) {
	t.Parallel()
	s := rcStore(t, t.TempDir(), "scope-a")
	s.max = 5
	base := time.Now().Add(-time.Hour)
	for i := range 8 {
		conv := fmt.Sprintf("conv-%d", i)
		if _, err := s.put(conv, rcSecret(i)); err != nil {
			t.Fatal(err)
		}
		// Age each file so "oldest" is unambiguous whatever the filesystem's timestamp
		// resolution: entry i is i minutes after base.
		if err := os.Chtimes(s.path(conv), base.Add(time.Duration(i)*time.Minute), base.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if n := s.count(); n != 5 {
		t.Fatalf("store holds %d entries, want the cap of 5", n)
	}
	for i := range 3 {
		if _, ok := s.load(fmt.Sprintf("conv-%d", i)); ok {
			t.Errorf("conv-%d, among the oldest, survived the cap", i)
		}
	}
	for i := 3; i < 8; i++ {
		if _, ok := s.load(fmt.Sprintf("conv-%d", i)); !ok {
			t.Errorf("conv-%d, among the newest, was evicted", i)
		}
	}

	// The entry just written is never the one evicted, even when the clock says it is oldest.
	if _, err := s.put("conv-new", rcSecret(99)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(s.path("conv-new"), base.Add(-time.Hour), base.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.put("conv-new", rcSecret(100)); err != nil {
		t.Fatal(err)
	}
	if e, ok := s.load("conv-new"); !ok || e.Secret != rcSecret(100) {
		t.Errorf("the credential just written was evicted: %+v (found %v)", e, ok)
	}
}

// A nil store holds nothing and is safe to ask.
func TestResumeStore_NilIsInert(t *testing.T) {
	t.Parallel()
	var s *resumeStore
	if _, ok := s.load("conv"); ok {
		t.Error("a nil store returned an entry")
	}
	if _, err := s.put("conv", rcSecret(1)); err == nil {
		t.Error("a nil store accepted a write")
	}
	if newResumeStore("", "scope") != nil || newResumeStore(t.TempDir(), "") != nil {
		t.Error("a store was built with no directory or no scope")
	}
}

// The scope is derived from where the daemon's session-state database lives: two paths,
// two scopes; the same path, the same scope; and the scope reveals no path.
func TestResumeDaemonScope(t *testing.T) {
	t.Parallel()
	a, b := resumeDaemonScope("/data/a/session_state.db"), resumeDaemonScope("/data/b/session_state.db")
	if a == b || a != resumeDaemonScope("/data/a/session_state.db") {
		t.Errorf("scopes %q and %q: want distinct for distinct databases and stable for one", a, b)
	}
	if strings.Contains(a, "data") {
		t.Errorf("scope %q reveals a path", a)
	}
}

func mustJSON(t *testing.T, e resumeEntry) string {
	t.Helper()
	return fmt.Sprintf(`{"v":%d,"conversation":%q,"scope":%q,"secret":%q,"generation":%d,"updated":%q}`,
		e.Version, e.Conversation, e.Scope, e.Secret, e.Generation, e.Updated.Format(time.RFC3339Nano))
}
