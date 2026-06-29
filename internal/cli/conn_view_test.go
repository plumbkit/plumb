package cli

import (
	"context"
	"sync"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
)

// TestSessionView_ConcurrentReadsDuringMutation is the snapshot-model race
// oracle: many readers hammer the lock-free accessors (workspace, gitConfig,
// isStrict, sessionName) while a driver goroutine repeatedly attaches, re-pins,
// and re-applies config — every mutation a full copy-on-write swap. Run under
// -race, it proves readers never block on a mutation and never observe a torn
// view: workspace() must always be one of the known roots (or empty), never a
// half-written string, and gitConfig() must always be a coherent value.
//
// freshTempDir is required (not t.TempDir): the plumb repo sets GOTMPDIR inside
// the source tree, so t.TempDir would resolve to plumb's own go.mod; freshTempDir
// lands under /var|/tmp where the .git marker is the nearest root.
func TestSessionView_ConcurrentReadsDuringMutation(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	pool := detectTestPool()

	rootA := freshTempDir(t)
	rootB := freshTempDir(t)
	mustGitDir(t, rootA)
	mustGitDir(t, rootB)
	allowed := map[string]bool{"": true, rootA: true, rootB: true}

	s := newConnSession(context.Background(), pool, nil, store, nil, nil, newSharedBudgets())
	defer s.close()

	var wg sync.WaitGroup
	done := make(chan struct{})

	// Readers: load the snapshot through the lock-free accessors and assert every
	// observation is internally consistent (never a torn root).
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
					if ws := s.workspace(); !allowed[ws] {
						t.Errorf("torn workspace read: %q", ws)
						return
					}
					_ = s.gitConfig()
					_ = s.isStrict()
					_ = s.sessionName()
				}
			}
		}()
	}

	// Driver: attach → re-pin A/B → reload, each a copy-on-write swap on the lane.
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.attachWorkspace(context.Background(), "file://"+rootA)
		s.applyProjectConfig(s.workspace())
		for i := 0; i < 200; i++ {
			target := rootA
			if i%2 == 0 {
				target = rootB
			}
			if _, err := s.repinWorkspace(context.Background(), target, ""); err != nil {
				t.Errorf("repin: %v", err)
				break
			}
			s.applyProjectConfig(s.workspace())
		}
		close(done)
	}()

	wg.Wait()

	if ws := s.workspace(); !allowed[ws] {
		t.Fatalf("final workspace read torn: %q", ws)
	}
}
