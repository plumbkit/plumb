package cli

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/golimpio/plumb/internal/config"
)

func TestCheckAndReloadConfig_DeduplicatesOnMtime(t *testing.T) {
	tmpdir := t.TempDir()
	plumbDir := filepath.Join(tmpdir, ".plumb")
	if err := os.MkdirAll(plumbDir, 0o755); err != nil {
		t.Fatal(err)
	}

	configPath := filepath.Join(plumbDir, "config.toml")
	if err := os.WriteFile(configPath, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := &connSession{ctx: ctx, store: config.NewStore(*getDefaultTestConfig())}

	s.mutate(func(v *sessionView) { v.acquiredRoot = tmpdir })

	s.checkAndReloadConfig()

	mtime1 := s.view().lastCfgMtime

	if mtime1.IsZero() {
		t.Errorf("expected lastCfgMtime to be set after first call")
	}

	s.checkAndReloadConfig()

	mtime2 := s.view().lastCfgMtime

	if !mtime1.Equal(mtime2) {
		t.Errorf("expected lastCfgMtime unchanged on dedup: %v vs %v", mtime1, mtime2)
	}
}

func TestCheckAndReloadConfig_AppliesOnNewMtime(t *testing.T) {
	tmpdir := t.TempDir()
	plumbDir := filepath.Join(tmpdir, ".plumb")
	if err := os.MkdirAll(plumbDir, 0o755); err != nil {
		t.Fatal(err)
	}

	configPath := filepath.Join(plumbDir, "config.toml")
	if err := os.WriteFile(configPath, []byte("[edits]\nstrict = false\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := &connSession{ctx: ctx, store: config.NewStore(*getDefaultTestConfig())}

	s.mutate(func(v *sessionView) { v.acquiredRoot = tmpdir })

	s.checkAndReloadConfig()

	mtime1 := s.view().lastCfgMtime

	time.Sleep(100 * time.Millisecond)
	if err := os.WriteFile(configPath, []byte("[edits]\nstrict = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s.checkAndReloadConfig()

	mtime2 := s.view().lastCfgMtime

	if mtime1.Equal(mtime2) {
		t.Errorf("expected lastCfgMtime to change after file modification")
	}

	if !s.isStrict() {
		t.Errorf("expected strict mode to be true after hot-reload")
	}
}

func TestCheckAndReloadConfig_SkipsWhenWorkspaceUnresolved(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := &connSession{ctx: ctx}

	s.checkAndReloadConfig()

	if !s.view().lastCfgMtime.IsZero() {
		t.Errorf("expected lastCfgMtime to remain zero when workspace unresolved")
	}
}

func TestStartConfigWatcher_StartsOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := &connSession{ctx: ctx}

	s.startConfigWatcher()
	s.startConfigWatcher()
	s.startConfigWatcher()
}

func TestApplyProjectConfig_SeedsLastCfgMtime(t *testing.T) {
	tmpdir := t.TempDir()
	plumbDir := filepath.Join(tmpdir, ".plumb")
	if err := os.MkdirAll(plumbDir, 0o755); err != nil {
		t.Fatal(err)
	}

	configPath := filepath.Join(plumbDir, "config.toml")
	if err := os.WriteFile(configPath, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}

	s := &connSession{store: config.NewStore(*getDefaultTestConfig())}

	if !s.view().lastCfgMtime.IsZero() {
		t.Errorf("expected lastCfgMtime to be zero before apply")
	}

	s.applyProjectConfig(tmpdir)

	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	expectedMtime := info.ModTime()

	actualMtime := s.view().lastCfgMtime

	if !actualMtime.Equal(expectedMtime) {
		t.Errorf("expected lastCfgMtime=%v, got %v", expectedMtime, actualMtime)
	}
}

// TestApplyProjectConfig_UsesLiveGlobalBase asserts that applyProjectConfig
// merges against the *current* global base from the store, so a global change
// is reflected on the next apply even with no project config file present.
func TestApplyProjectConfig_UsesLiveGlobalBase(t *testing.T) {
	ws := t.TempDir() // workspace with no .plumb/config.toml → inherits the global base
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	store := config.NewStore(config.Defaults())
	s := &connSession{store: store}
	s.mutate(func(v *sessionView) { v.acquiredRoot = ws })

	s.applyProjectConfig(ws)
	if s.isStrict() {
		t.Fatal("expected non-strict before the global base changes")
	}

	writeGlobalConfig(t, "[edits]\nstrict = true\n")
	if err := store.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	s.applyProjectConfig(ws)
	if !s.isStrict() {
		t.Error("expected strict after applyProjectConfig re-merged the new global base")
	}
}

// TestGlobalConfigChange_ReappliesSession reproduces the subscription
// newConnSession installs and asserts a published global change automatically
// re-applies the per-session config (no explicit applyProjectConfig call).
func TestGlobalConfigChange_ReappliesSession(t *testing.T) {
	ws := t.TempDir() // no project config → session tracks the global base
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	store := config.NewStore(config.Defaults())
	s := &connSession{store: store}
	s.mutate(func(v *sessionView) { v.acquiredRoot = ws })

	unsub := store.Subscribe(func(config.Config) {
		if w := s.workspace(); w != "" {
			s.applyProjectConfig(w)
		}
	})
	defer unsub()

	s.applyProjectConfig(ws) // seed
	if s.isStrict() {
		t.Fatal("expected non-strict initially")
	}

	writeGlobalConfig(t, "[edits]\nstrict = true\n")
	if err := store.Reload(); err != nil { // fires the subscription → re-applies the session
		t.Fatalf("Reload: %v", err)
	}

	if !s.isStrict() {
		t.Error("expected strict after the global config change propagated via the store")
	}
}

// writeGlobalConfig writes body to the global config path resolved from the
// test's XDG_CONFIG_HOME, creating the parent directory.
func writeGlobalConfig(t *testing.T, body string) {
	t.Helper()
	gp := config.GlobalConfigPath()
	if err := os.MkdirAll(filepath.Dir(gp), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gp, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestLastCfgMtimeThreadSafety hammers the snapshot's lastCfgMtime from several
// readers (lock-free view loads) while a writer drives it through the mutation
// lane — the race detector verifies the snapshot model is data-race free.
func TestLastCfgMtimeThreadSafety(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := &connSession{ctx: ctx}

	var wg sync.WaitGroup
	done := make(chan bool)

	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
					_ = s.view().lastCfgMtime
				}
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			select {
			case <-done:
				return
			default:
				s.mutate(func(v *sessionView) { v.lastCfgMtime = time.Now() })
			}
		}
	}()

	time.Sleep(50 * time.Millisecond)
	close(done)
	wg.Wait()
}

func getDefaultTestConfig() *config.Config {
	return &config.Config{
		LogLevel:  "info",
		LogFormat: "text",
		Edits: config.EditsConfig{
			Strict: false,
		},
	}
}
