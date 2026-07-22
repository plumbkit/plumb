package config

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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

// TestStore_ListenerPanicDoesNotStopOtherListeners asserts that a panicking
// subscriber cannot abort delivery to the subscribers registered after it —
// a broken listener degrades to "missed one notification", never "broke
// notification for everyone downstream". The panicking listener A is
// registered first (index 0) so the test also proves the notify loop's
// iteration state survives a mid-loop panic, not merely that unrelated
// listeners fire independently. A captureHandler asserts the panic is
// actually logged, matching the internal/cli precedent for this pattern
// (see daemon_test.go's captureHandler).
func TestStore_ListenerPanicDoesNotStopOtherListeners(t *testing.T) {
	var records []string
	var mu sync.Mutex
	h := &captureHandler{fn: func(msg string) {
		mu.Lock()
		records = append(records, msg)
		mu.Unlock()
	}}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	defer slog.SetDefault(prev)

	s := NewStore(Defaults())

	var bCalls, cCalls int
	s.Subscribe(func(Config) { panic("boom") }) // A
	s.Subscribe(func(Config) { bCalls++ })      // B
	s.Subscribe(func(Config) { cCalls++ })      // C

	s.set(Defaults())

	if bCalls != 1 {
		t.Errorf("listener B fired %d times, want 1 (a panic upstream must not stop it)", bCalls)
	}
	if cCalls != 1 {
		t.Errorf("listener C fired %d times, want 1 (a panic upstream must not stop it)", cCalls)
	}

	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, r := range records {
		if strings.Contains(r, "listener panic") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("listener panic was not logged; records = %v", records)
	}
}

// TestStore_PanickingListenerLeavesStoreUsable asserts the Store survives a
// panicking listener and remains fully usable for a subsequent set/notify
// cycle: the normal listener keeps firing, Current/Generation keep advancing,
// and neither set call propagates the panic out to the caller.
func TestStore_PanickingListenerLeavesStoreUsable(t *testing.T) {
	s := NewStore(Defaults())

	var calls int
	s.Subscribe(func(Config) { panic("boom") })
	s.Subscribe(func(Config) { calls++ })

	first := Defaults()
	first.LogLevel = "warn"
	s.set(first) // panics internally; must not propagate here

	second := Defaults()
	second.LogLevel = "debug"
	s.set(second) // a second cycle must still work cleanly

	if calls != 2 {
		t.Errorf("normal listener fired %d times across two set cycles, want 2", calls)
	}
	if got := s.Current().LogLevel; got != "debug" {
		t.Errorf("Current().LogLevel = %q after two cycles, want %q", got, "debug")
	}
	if got := s.Generation(); got != 3 {
		t.Errorf("Generation() = %d after two set cycles from a fresh store, want 3", got)
	}
}

// captureHandler is a minimal slog.Handler that calls fn with the message of
// every record; mirrors the equivalent helper in internal/cli/daemon_test.go
// (unexported there, so duplicated here rather than shared across packages).
type captureHandler struct {
	fn func(msg string)
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.fn(r.Message)
	return nil
}
func (h *captureHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(_ string) slog.Handler      { return h }
