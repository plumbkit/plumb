//go:build !darwin && !linux && !windows

package fswatch

import (
	"fmt"
	"runtime"
)

// startBackend refuses. The only recursive watcher on the BSDs is kqueue,
// which needs an open descriptor per watched file; closing those descriptors
// releases the daemon's fcntl locks on its SQLite files under .plumb (see the
// package comment). Periodic resync is slower but cannot lose data.
func (w *Watcher) startBackend(Options) error {
	return fmt.Errorf("%w (%s): kqueue would hold, and on close release, the daemon's SQLite locks", ErrUnsupported, runtime.GOOS)
}
