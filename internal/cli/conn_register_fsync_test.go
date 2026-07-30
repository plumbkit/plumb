package cli

import (
	"testing"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/fsync"
	"github.com/plumbkit/plumb/internal/tools"
)

// TestBuildWriteDeps_DoesNotInstallPerConnectionFsyncKnob pins the scope of the
// [edits] fsync knob. It gates free functions (safeWrite and friends) shared by
// every session, so it is DAEMON-GLOBAL and installed once in runDaemon from the
// global config store. Installing it per connection — from the connection's
// PROJECT-resolved edits config — made it last-writer-wins: a workspace with
// `[edits] fsync = false` silently disabled crash durability for every other
// live session on every other workspace.
func TestBuildWriteDeps_DoesNotInstallPerConnectionFsyncKnob(t *testing.T) {
	t.Cleanup(func() { tools.SetFsyncFunc(nil) })

	// A session whose project-resolved config opts out of fsync.
	s := &connSession{}
	s.state.Store(&sessionView{edits: config.EditsConfig{Fsync: false}})

	_ = s.buildWriteDeps()

	if !fsync.Enabled() {
		t.Error("buildWriteDeps installed a per-connection fsync knob: one workspace's " +
			"[edits] fsync = false now disables fsync process-wide (the knob is daemon-global)")
	}
}
