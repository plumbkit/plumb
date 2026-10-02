package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/history"
)

// A copy from project A into project B is one change with two paths. The store
// and the response gate must take ONE decision for it, and it must honour a
// glob declared in either project: a glob only in the source's project used to
// store cleartext while the response withheld it, and a glob only in the
// destination's project used to show in the transcript what the store withheld.
func TestCrossProjectCopyStoreAndResponseAgree(t *testing.T) {
	cases := []struct {
		name          string
		srcGlob       bool // *.vault declared in the source's project
		dstGlob       bool // *.vault declared in the destination's project
		wantSensitive bool
	}{
		{"glob in the source project only", true, false, true},
		{"glob in the destination project only", false, true, true},
		{"control: no glob anywhere", false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_DATA_HOME", t.TempDir())
			store, ss := newOriginStore(t)
			a, b := freshTempDir(t), freshTempDir(t)
			mustGitDir(t, a)
			mustGitDir(t, b)
			for root, on := range map[string]bool{a: tc.srcGlob, b: tc.dstGlob} {
				if !on {
					continue
				}
				if err := os.MkdirAll(filepath.Join(root, ".plumb"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, ".plumb", "config.toml"),
					[]byte("[history]\nsensitive_globs = [\"*.vault\"]\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			s := newPersistSession(t, store, ss, "proxy-cross-project-copy")
			s.historyStore = newHistoryStore(nil)
			defer s.historyStore.Close()
			if _, err := s.repinWorkspace(context.Background(), "file://"+a, "", false, false); err != nil {
				t.Fatalf("pin: %v", err)
			}
			src, dst := filepath.Join(a, "db.vault"), filepath.Join(b, "notes.txt")
			ctx := context.Background()

			if got := s.buildWriteDeps().SensitivePathFn(ctx, dst, src); got != tc.wantSensitive {
				t.Errorf("response gate = %v, want %v", got, tc.wantSensitive)
			}

			s.recordHistory(ctx, history.Change{
				Op: history.OpCopy, Kind: history.KindFile, At: time.Now(), Path: dst, From: src,
				After: history.SideFromBytes([]byte("SECRET=hunter2\n")),
			})
			if err := s.historyStore.store().Sync(ctx); err != nil {
				t.Fatal(err)
			}
			r, err := history.OpenReadOnlyAt(history.DBPath())
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			es, err := r.List(history.Filter{All: true})
			if err != nil || len(es) != 1 {
				t.Fatalf("entries = %+v, %v", es, err)
			}
			if stored := es[0].Content == history.ContentSensitive; stored != tc.wantSensitive {
				t.Errorf("store withheld = %v (content %q), want %v", stored, es[0].Content, tc.wantSensitive)
			}
		})
	}
}
