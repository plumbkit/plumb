package fswatch

import (
	"errors"
	"fmt"
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
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range eventBuffer + 10 {
			w.events <- Event{Path: fmt.Sprintf("/ws/f%d", i), Op: Write}
		}
	}()
	for i := range eventBuffer + 10 {
		recv(t, fast.Events(), fmt.Sprintf("fast event %d", i))
	}
	<-done
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
	late, _ := h.Subscribe("/ws", Options{}, "")
	defer late.Close()
	if !signalled(late.Failed()) {
		t.Fatal("a subscriber joining a failed watcher was not told")
	}
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
