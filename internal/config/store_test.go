package config

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func TestStore_CurrentReturnsInitial(t *testing.T) {
	initial := Defaults()
	initial.LogLevel = "warn"
	s := NewStore(initial)

	if got := s.Current().LogLevel; got != "warn" {
		t.Errorf("Current().LogLevel = %q, want %q", got, "warn")
	}
	if got := s.Generation(); got != 1 {
		t.Errorf("Generation() = %d, want 1 for a fresh store", got)
	}
}

func TestStore_SubscribeFiresOnSet(t *testing.T) {
	s := NewStore(Defaults())

	var got Config
	var calls int
	unsub := s.Subscribe(func(c Config) { got = c; calls++ })
	defer unsub()

	next := Defaults()
	next.LogLevel = "debug"
	s.set(next)

	if calls != 1 {
		t.Fatalf("listener fired %d times, want 1", calls)
	}
	if got.LogLevel != "debug" {
		t.Errorf("listener saw LogLevel %q, want %q", got.LogLevel, "debug")
	}
	if s.Generation() != 2 {
		t.Errorf("Generation() = %d after one set, want 2", s.Generation())
	}
}

func TestStore_UnsubscribeStopsDelivery(t *testing.T) {
	s := NewStore(Defaults())

	var calls int
	unsub := s.Subscribe(func(Config) { calls++ })
	s.set(Defaults())
	unsub()
	s.set(Defaults())

	if calls != 1 {
		t.Errorf("listener fired %d times, want 1 (no delivery after unsubscribe)", calls)
	}
}

func TestStore_MultipleSubscribersAllFire(t *testing.T) {
	s := NewStore(Defaults())

	var a, b int
	defer s.Subscribe(func(Config) { a++ })()
	defer s.Subscribe(func(Config) { b++ })()

	s.set(Defaults())

	if a != 1 || b != 1 {
		t.Errorf("subscriber fire counts a=%d b=%d, want 1,1", a, b)
	}
}

func TestStore_ReloadPublishesFromFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	s := NewStore(Defaults())
	var seen string
	defer s.Subscribe(func(c Config) { seen = c.LogLevel })()

	cfgPath := GlobalConfigPath()
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, []byte("log_level = \"error\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := s.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if s.Current().LogLevel != "error" {
		t.Errorf("Current().LogLevel = %q after reload, want %q", s.Current().LogLevel, "error")
	}
	if seen != "error" {
		t.Errorf("listener saw %q after reload, want %q", seen, "error")
	}
}

// TestStore_ReloadKeepsOldConfigOnParseError asserts a broken config file does
// not clobber the live config and surfaces a wrapped error.
func TestStore_ReloadKeepsOldConfigOnParseError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	good := Defaults()
	good.LogLevel = "warn"
	s := NewStore(good)
	genBefore := s.Generation()

	cfgPath := GlobalConfigPath()
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, []byte("[edits\nstrict = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := s.Reload(); err == nil {
		t.Fatal("Reload succeeded on a broken config; want an error")
	}
	if s.Current().LogLevel != "warn" {
		t.Errorf("Current().LogLevel = %q after failed reload, want preserved %q", s.Current().LogLevel, "warn")
	}
	if s.Generation() != genBefore {
		t.Errorf("Generation moved on a failed reload: %d → %d", genBefore, s.Generation())
	}
}

// TestStore_ConcurrentReadWrite exercises Current/Generation against concurrent
// set calls; run under -race to catch data races on the published pointer.
func TestStore_ConcurrentReadWrite(t *testing.T) {
	s := NewStore(Defaults())

	var wg sync.WaitGroup
	var stop atomic.Bool

	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				_ = s.Current()
				_ = s.Generation()
				_ = s.LastReloaded()
			}
		}()
	}
	for i := range 200 {
		next := Defaults()
		next.Edits.RateLimitPerMinute = i
		s.set(next)
	}
	stop.Store(true)
	wg.Wait()

	if s.Current().Edits.RateLimitPerMinute != 199 {
		t.Errorf("final RateLimitPerMinute = %d, want 199", s.Current().Edits.RateLimitPerMinute)
	}
}

func TestStore_RestartNeeded(t *testing.T) {
	s := NewStore(Defaults())
	if s.RestartNeeded() {
		t.Fatal("a fresh store should not report restart-needed")
	}

	// A live-reloadable change (strict mode) must NOT flag restart-needed.
	live := Defaults()
	live.Edits.Strict = true
	s.set(live)
	if s.RestartNeeded() {
		t.Error("edits.strict change should not require a restart")
	}

	// A restart-bound change (log format) MUST flag restart-needed.
	boundChange := Defaults()
	boundChange.LogFormat = "json"
	s.set(boundChange)
	if !s.RestartNeeded() {
		t.Error("log_format change should require a restart")
	}
}

func TestRestartSensitiveEqual(t *testing.T) {
	a := Defaults()

	if !RestartSensitiveEqual(a, Defaults()) {
		t.Fatal("identical configs should be restart-equal")
	}

	liveOnly := Defaults()
	liveOnly.Edits.Strict = !a.Edits.Strict
	liveOnly.Git.AllowPush = !a.Git.AllowPush
	liveOnly.Topology.Enabled = !a.Topology.Enabled
	if !RestartSensitiveEqual(a, liveOnly) {
		t.Error("edits/git/topology changes must not be restart-sensitive")
	}

	cacheChange := Defaults()
	cacheChange.Cache.MaxSize++
	if RestartSensitiveEqual(a, cacheChange) {
		t.Error("cache change should be restart-sensitive")
	}

	fmtChange := Defaults()
	fmtChange.LogFormat = "json"
	if RestartSensitiveEqual(a, fmtChange) {
		t.Error("log_format change should be restart-sensitive")
	}

	lspChange := Defaults()
	if lspChange.LSP == nil {
		lspChange.LSP = map[string]LSPConfig{}
	}
	lspChange.LSP["go"] = LSPConfig{Command: "some-other-gopls"}
	if RestartSensitiveEqual(a, lspChange) {
		t.Error("LSP server change should be restart-sensitive")
	}
}

// TestSave_AtomicLeavesNoTempFiles asserts the atomic write cleans up its
// staging file and produces a single config.toml.
func TestSave_AtomicLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	if err := Save(func(c *Config) { c.LogLevel = "debug" }); err != nil {
		t.Fatalf("Save: %v", err)
	}

	entries, err := os.ReadDir(filepath.Dir(GlobalConfigPath()))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" || filepath.Base(e.Name()) != "config.toml" {
			t.Errorf("unexpected leftover file in config dir: %q", e.Name())
		}
	}
}

// TestStore_NotifiesInRegistrationOrder pins the deterministic notification
// order the topology reconcile relies on: the daemon-level subscriber, which is
// registered before any connection, must run before every per-session one.
func TestStore_NotifiesInRegistrationOrder(t *testing.T) {
	s := NewStore(Defaults())
	var mu sync.Mutex
	var order []int
	for i := 0; i < 5; i++ {
		idx := i
		s.Subscribe(func(Config) {
			mu.Lock()
			order = append(order, idx)
			mu.Unlock()
		})
	}
	s.set(Defaults())
	if len(order) != 5 {
		t.Fatalf("got %d notifications, want 5", len(order))
	}
	for i, got := range order {
		if got != i {
			t.Errorf("notification %d came from subscriber %d; want registration order", i, got)
		}
	}
}
