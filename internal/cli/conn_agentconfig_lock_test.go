package cli

import (
	"context"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/tools"
)

// agent_config must take config.toml's path lock, the one edit_file takes:
// otherwise an edit_file landing between its Before read and its write is
// overwritten, and the history row pairs sides of two different writes.
func TestAgentConfigWaitsForTheConfigPathLock(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store, ss := newOriginStore(t)
	root := freshTempDir(t)
	mustGitDir(t, root)
	s := newPersistSession(t, store, ss, "proxy-agent-config-lock")
	s.historyStore = newHistoryStore(nil)
	defer s.historyStore.Close()
	if _, err := s.repinWorkspace(context.Background(), "file://"+root, "", false, false); err != nil {
		t.Fatalf("pin: %v", err)
	}
	s.mutate(func(v *sessionView) { v.agentConfigWrites = true })

	unlock := tools.LockPath(config.ProjectConfigPath(root))
	done := make(chan error, 1)
	go func() {
		_, err := s.applyAgentConfig(context.Background(), map[string]any{"tasks.go.build": "go build ./..."})
		done <- err
	}()
	select {
	case err := <-done:
		unlock()
		t.Fatalf("agent_config wrote while another writer held config.toml's lock (err: %v)", err)
	case <-time.After(200 * time.Millisecond):
	}
	unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
