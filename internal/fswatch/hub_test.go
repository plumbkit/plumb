package fswatch

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeWatcher is a Watcher whose channels a test drives.
func fakeWatcher() *Watcher {
	return &Watcher{
		events: make(chan Event, eventBuffer),
		lost:   make(chan struct{}, 1),
		failed: make(chan struct{}),
		done:   make(chan struct{}),
	}
}

// countingHub opens fake watchers, counting opens and recording each.
type countingHub struct {
	*Hub
	opens   atomic.Int32
	mu      sync.Mutex
	opened  []*Watcher
	failNow atomic.Bool
}

func newCountingHub() *countingHub {
	c := &countingHub{Hub: NewHub()}
	c.open = func(string, Options) (*Watcher, error) {
		if c.failNow.Load() {
			return nil, errors.New("seam: open failed")
		}
		c.opens.Add(1)
		w := fakeWatcher()
		c.mu.Lock()
		c.opened = append(c.opened, w)
		c.mu.Unlock()
		return w, nil
	}
	return c
}

func (c *countingHub) last() *Watcher {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.opened[len(c.opened)-1]
}

func recv(t *testing.T, ch <-chan Event, what string) Event {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(2 * time.Second):
		t.Fatalf("%s: no event", what)
		return Event{}
	}
}

func signalled(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	case <-time.After(2 * time.Second):
		return false
	}
}

func TestHub_SharesOneWatcherPerRootAndFiltersPerSubscriber(t *testing.T) {
	h := newCountingHub()
	opts := Options{Cooldown: time.Millisecond}
	topo, err := h.Subscribe("/ws", opts, "")
	if err != nil {
		t.Fatal(err)
	}
	defer topo.Close()
	lsp, err := h.Subscribe("/ws/", opts, ExcludeDirsRegex("/ws", "target"))
	if err != nil {
		t.Fatal(err)
	}
	defer lsp.Close()
	if n := h.opens.Load(); n != 1 || h.Watches() != 1 {
		t.Fatalf("opens = %d, watches = %d; want one shared OS watcher for one root", n, h.Watches())
	}
	other, err := h.Subscribe("/elsewhere", opts, "")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if h.opens.Load() != 2 {
		t.Fatal("another root did not get its own watcher")
	}
	w := h.opened[0]
	w.events <- Event{Path: "/ws/target/x.o", Op: Write}
	w.events <- Event{Path: "/ws/main.go", Op: Write}
	if ev := recv(t, topo.Events(), "topology"); ev.Path != "/ws/target/x.o" {
		t.Fatalf("topology got %s first, want the target/ event it does not exclude", ev.Path)
	}
	recv(t, topo.Events(), "topology second")
	if ev := recv(t, lsp.Events(), "lsp"); ev.Path != "/ws/main.go" {
		t.Fatalf("lsp got %s, want main.go (its own exclusion drops target/)", ev.Path)
	}
}

// TestHub_ASlowSubscriberLosesAloneAndBlocksNobody: a subscriber that never
// reads fills its own buffer and gets Lost; the other keeps receiving.
func TestHub_ASlowSubscriberLosesAloneAndBlocksNobody(t *testing.T) {
	h := newCountingHub()
	slow, _ := h.Subscribe("/ws", Options{}, "")
	defer slow.Close()
	fast, _ := h.Subscribe("/ws", Options{}, "")
	defer fast.Close()
	w := h.last()
	// Lockstep: each event is read by the fast subscriber before the next is
	// sent, so only the slow one (which never reads) can overflow — under load
	// a free-running producer could overflow the fast one too.
	for i := range eventBuffer + 10 {
		w.events <- Event{Path: fmt.Sprintf("/ws/f%d", i), Op: Write}
		recv(t, fast.Events(), fmt.Sprintf("fast event %d", i))
	}
	if !signalled(slow.Lost()) {
		t.Fatal("the slow subscriber's overflow was not reported on its Lost")
	}
	select {
	case <-fast.Lost():
		t.Fatal("the fast subscriber was told it lost events")
	default:
	}
}

func TestHub_LostAndFailedReachEverySubscriber(t *testing.T) {
	h := newCountingHub()
	a, _ := h.Subscribe("/ws", Options{}, "")
	defer a.Close()
	b, _ := h.Subscribe("/ws", Options{}, "")
	defer b.Close()
	w := h.last()
	w.lost <- struct{}{}
	if !signalled(a.Lost()) || !signalled(b.Lost()) {
		t.Fatal("an OS-level Lost did not reach every subscriber")
	}
	close(w.failed)
	if !signalled(a.Failed()) || !signalled(b.Failed()) {
		t.Fatal("Failed did not reach every subscriber")
	}
	// A subscriber arriving after Failed gets a fresh watcher instead
	// (TestHub_AFailedWatcherIsReplacedForANewSubscriber).
}

func TestHub_TheLastCloseClosesTheOSWatcher(t *testing.T) {
	h := newCountingHub()
	a, _ := h.Subscribe("/ws", Options{}, "")
	b, _ := h.Subscribe("/ws", Options{}, "")
	w := h.last()
	a.Close()
	select {
	case <-w.done:
		t.Fatal("the OS watcher closed while a subscriber remained")
	default:
	}
	b.Close()
	select {
	case <-w.done:
	case <-time.After(2 * time.Second):
		t.Fatal("the last Close did not close the OS watcher")
	}
	if h.Watches() != 0 {
		t.Fatal("the hub still lists a closed watcher")
	}
	if _, ok := <-b.Events(); ok {
		t.Fatal("a closed subscription's Events is still open")
	}
	c, _ := h.Subscribe("/ws", Options{}, "")
	defer c.Close()
	if h.opens.Load() != 2 {
		t.Fatal("subscribing after the last Close did not open a fresh watcher")
	}
}

func TestHub_AFailedOpenIsRetried(t *testing.T) {
	h := newCountingHub()
	h.failNow.Store(true)
	if _, err := h.Subscribe("/ws", Options{}, ""); err == nil {
		t.Fatal("control: the seam did not fail the open")
	}
	h.failNow.Store(false)
	s, err := h.Subscribe("/ws", Options{}, "")
	if err != nil {
		t.Fatalf("a later Subscribe did not retry the open: %v", err)
	}
	s.Close()
}

// TestHub_ConcurrentSubscribeAndCloseUnderTraffic is the -race proof: churn
// against a live event stream neither panics (send on a closed channel) nor
// leaks a watcher.
func TestHub_ConcurrentSubscribeAndCloseUnderTraffic(t *testing.T) {
	h := newCountingHub()
	anchor, _ := h.Subscribe("/ws", Options{}, "")
	w := h.last()
	stop := make(chan struct{})
	var traffic sync.WaitGroup
	traffic.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case w.events <- Event{Path: fmt.Sprintf("/ws/%d", i), Op: Write}:
			}
		}
	})
	traffic.Go(func() {
		for {
			select {
			case <-stop:
				return
			case <-anchor.Events():
			}
		}
	})
	var churn sync.WaitGroup
	for range 8 {
		churn.Go(func() {
			for range 50 {
				s, err := h.Subscribe("/ws", Options{}, "")
				if err != nil {
					t.Error(err)
					return
				}
				s.Close()
			}
		})
	}
	churn.Wait()
	close(stop)
	traffic.Wait()
	if h.opens.Load() != 1 {
		t.Fatalf("opens = %d; the anchor kept the watcher, so churn must reuse it", h.opens.Load())
	}
	anchor.Close()
	if h.Watches() != 0 {
		t.Fatal("a watcher outlived its last subscriber")
	}
}

func TestHub_NilHubOpensAPrivateWatcher(t *testing.T) {
	var h *Hub
	s, err := h.Subscribe(t.TempDir(), Options{}, "")
	if err != nil {
		t.Skipf("no watcher on this platform: %v", err)
	}
	s.Close()
}

// TestHub_AFailedWatcherIsReplacedForANewSubscriber: a consumer opening again
// after Failed gets a fresh OS watcher; the old one's subscribers keep it until
// they leave, and their leaving does not drop the successor.
func TestHub_AFailedWatcherIsReplacedForANewSubscriber(t *testing.T) {
	h := newCountingHub()
	old, _ := h.Subscribe("/ws", Options{}, "")
	first := h.last()
	close(first.failed)
	if !signalled(old.Failed()) {
		t.Fatal("control: Failed did not reach the subscriber")
	}
	fresh, err := h.Subscribe("/ws", Options{}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if h.opens.Load() != 2 || h.last() == first {
		t.Fatal("a subscriber after Failed was handed the failed watcher")
	}
	select {
	case <-fresh.Failed():
		t.Fatal("the fresh subscription starts out failed")
	default:
	}
	old.Close()
	select {
	case <-first.done:
	case <-time.After(2 * time.Second):
		t.Fatal("the failed watcher was not closed with its last subscriber")
	}
	if h.Watches() != 1 {
		t.Fatal("the old watcher's last Close dropped its successor from the hub")
	}
}

// TestHub_TheLastCloseRacingAJoinNeverLeavesADeafSubscription: with no anchor,
// every last Close races another Subscribe's join; a join must never land on a
// watcher the hub no longer lists (its subscriber would hear nothing, ever).
func TestHub_TheLastCloseRacingAJoinNeverLeavesADeafSubscription(t *testing.T) {
	h := newCountingHub()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 300 {
				s, err := h.Subscribe("/ws", Options{}, "")
				if err != nil {
					t.Error(err)
					return
				}
				h.mu.Lock()
				live := h.watches[s.key] == s.sw
				h.mu.Unlock()
				if !live {
					t.Error("joined a watcher the hub no longer lists")
				}
				s.Close()
			}
		})
	}
	wg.Wait()
	if h.Watches() != 0 || h.Subscriptions() != 0 {
		t.Fatalf("leak: %d watches, %d subscriptions", h.Watches(), h.Subscriptions())
	}
}

// TestHub_DifferentOptionsGetDifferentWatchers: only identical options share.
func TestHub_DifferentOptionsGetDifferentWatchers(t *testing.T) {
	h := newCountingHub()
	a, _ := h.Subscribe("/ws", Options{Cooldown: time.Millisecond}, "")
	defer a.Close()
	b, _ := h.Subscribe("/ws", Options{Cooldown: 2 * time.Millisecond}, "")
	defer b.Close()
	if h.opens.Load() != 2 {
		t.Fatal("subscriptions with different options shared one watcher")
	}
}

// TestHub_CloseClosesLostAndIsSafeTwice.
func TestHub_CloseClosesLostAndIsSafeTwice(t *testing.T) {
	h := newCountingHub()
	keep, _ := h.Subscribe("/ws", Options{}, "")
	defer keep.Close()
	s, _ := h.Subscribe("/ws", Options{}, "")
	s.Close()
	s.Close()
	if _, ok := <-s.Lost(); ok {
		t.Fatal("a closed subscription's Lost is still open")
	}
}

// TestHub_APrivateSubscriptionFiltersAndClosesItsChannels: a nil hub's
// subscription delivers through its own exclusion and, by the time Close
// returns, has closed Events and Lost.
func TestHub_APrivateSubscriptionFiltersAndClosesItsChannels(t *testing.T) {
	ws := t.TempDir()
	var h *Hub
	s, err := h.Subscribe(ws, Options{}, ExcludeDirsRegex(ws, "skipped"))
	if err != nil {
		t.Skipf("no watcher on this platform: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(ws, "skipped"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "skipped", "x"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "kept"), []byte("k"), 0o644); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	sawKept := false
	for !sawKept {
		select {
		case ev := <-s.Events():
			if strings.Contains(ev.Path, "skipped") {
				t.Fatalf("an excluded path came through: %s", ev.Path)
			}
			sawKept = strings.HasSuffix(ev.Path, "kept")
		case <-deadline:
			t.Fatal("the private subscription delivered nothing")
		}
	}
	s.Close()
	// A signal buffered before Close may still be read; after it, the channel
	// must already be closed — never blocking.
	for range 2 {
		select {
		case _, ok := <-s.Lost():
			if !ok {
				return
			}
		default:
			t.Fatal("Close returned before closing Lost")
		}
	}
	t.Fatal("Lost still open after Close")
}

// TestHub_APrivateSubscriptionsEventsCloseWithIt: its Events is closed by the
// time Close returns (drained of anything buffered).
func TestHub_APrivateSubscriptionsEventsCloseWithIt(t *testing.T) {
	var h *Hub
	s, err := h.Subscribe(t.TempDir(), Options{}, "")
	if err != nil {
		t.Skipf("no watcher on this platform: %v", err)
	}
	s.Close()
	for range eventBuffer + 1 {
		select {
		case _, ok := <-s.Events():
			if !ok {
				return
			}
		default:
			t.Fatal("Close returned before closing Events")
		}
	}
	t.Fatal("Events still open after Close")
}

// TestHub_AWatcherFailedBeforeItsFirstJoinIsNotOrphaned: an opener whose
// watcher fails, and is replaced, before the opener joins it leaves a watcher
// nobody holds; the failed join closes it (from #695's review).
func TestHub_AWatcherFailedBeforeItsFirstJoinIsNotOrphaned(t *testing.T) {
	h := newCountingHub()
	key := hubKey{root: "/ws"}
	sw := &sharedWatch{ready: make(chan struct{}), subs: make(map[*Subscription]struct{}), done: make(chan struct{})}
	h.mu.Lock()
	h.watches[key] = sw
	h.mu.Unlock()
	h.openShared(key, sw) // "opener A" is between ready and its join
	first := h.last()
	close(first.failed)
	deadline := time.After(2 * time.Second)
	for !sw.hasFailed() {
		select {
		case <-deadline:
			t.Fatal("fanOut never recorded Failed")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	b, err := h.Subscribe("/ws", Options{}, "") // "B"
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, ok := h.join(key, sw, nil); ok {
		t.Fatal("A joined the replaced watcher")
	}
	select {
	case <-first.done:
	case <-time.After(500 * time.Millisecond):
		t.Error("ORPHAN: the replaced zero-subscriber watcher's OS watcher is never closed")
	}
}
