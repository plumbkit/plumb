package cli

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// TestAttachOnly_TakesNoLockNotEvenBriefly pins attachOnly's contract (PLAN-550
// MINOR-1): "it takes no lock, not even briefly". A probe that held the account
// authority or the lifetime lock for an instant would make a concurrent launcher
// see it held and wait out its whole deadline — a diagnostic turned outage
// (lock.go's contract). The review's mutant R5, an acquireAuthority + Close inside
// attachOnly's loop, survived every attach-only test.
//
// While attachOnly waits on a starting daemon, a second open file description
// tries each lock non-blocking, as fast as it can: every attempt must succeed. A
// hold of microseconds on attachOnly's side collides with one of those attempts.
func TestAttachOnly_TakesNoLockNotEvenBriefly(t *testing.T) {
	withCanonicalService(t)
	t.Setenv("PLUMB_DAEMON_START_TIMEOUT", "5s")

	socket := shortSocketPath(t)
	authority, err := authorityPath()
	if err != nil {
		t.Fatalf("authorityPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(authority), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	rec, err := os.OpenFile(authority, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("opening the authority lock: %v", err)
	}
	// A starting daemon's record (this live pid), so attachOnly keeps polling —
	// through attachToHolder, the legacy probe and authorityRecorded — instead of
	// failing at once.
	if err := publishAuthority(rec, authorityMetadata{PID: os.Getpid(), Version: "test", Socket: socket}); err != nil {
		t.Fatalf("publishAuthority: %v", err)
	}
	_ = rec.Close()

	type held struct {
		name string
		f    *os.File
	}
	// Every lock a launch takes: the account authority, the runtime lifetime lock,
	// and the lifetime lock of each legacy runtime location (PLAN-530's fence).
	locks := map[string]string{"account authority": authority, "lifetime": daemonLockPath()}
	for _, loc := range legacyLocations() {
		locks["legacy "+loc.Dir] = loc.Lock
	}
	if len(locks) < 3 {
		t.Fatalf("this isolated account names no legacy location, so the legacy fence is not probed: %v", locks)
	}
	probes := make([]held, 0, len(locks))
	for name, path := range locks {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // G304: a lock under this test's own temp runtime
		if err != nil {
			t.Fatalf("opening the %s lock: %v", name, err)
		}
		t.Cleanup(func() { _ = f.Close() })
		probes = append(probes, held{name, f})
	}

	var attempts, refused atomic.Int64
	var refusedName atomic.Value
	stop := make(chan struct{})
	hammered := make(chan struct{})
	go func() {
		defer close(hammered)
		for {
			select {
			case <-stop:
				return
			default:
			}
			for _, p := range probes {
				attempts.Add(1)
				err := syscall.Flock(int(p.f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
				if errors.Is(err, syscall.EWOULDBLOCK) {
					refused.Add(1)
					refusedName.Store(p.name)
					continue
				}
				if err == nil {
					_ = syscall.Flock(int(p.f.Fd()), syscall.LOCK_UN)
				}
			}
		}
	}()

	// The socket appears after attachOnly has polled for a while: the window in
	// which a lock taken "only to look" would be seen.
	go func() {
		time.Sleep(1500 * time.Millisecond)
		if ln, lerr := net.Listen("unix", socket); lerr == nil {
			t.Cleanup(func() { _ = ln.Close() })
		}
	}()

	conn, err := attachOnly(context.Background(), socket)
	close(stop)
	<-hammered
	if err != nil {
		t.Fatalf("attachOnly: %v", err)
	}
	_ = conn.Close()
	if attempts.Load() < 100 {
		t.Fatalf("only %d lock attempts were made while attachOnly ran: the probe proves nothing", attempts.Load())
	}
	if n := refused.Load(); n > 0 {
		t.Fatalf("the %v lock was held by somebody else %d time(s) while attachOnly ran: attach-only took a lock", refusedName.Load(), n)
	}
}
