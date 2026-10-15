package cli

// keyedFlight lets a pool open a key's resource WITHOUT holding the pool's
// mutex (PLAN-536): the first caller for a key becomes its leader and opens it
// unlocked; a later caller for the SAME key waits for the leader and then
// re-checks; callers for other keys never wait at all. Opening a SQLite store,
// starting an indexer or walking a tree for a watcher can take seconds, and
// doing it under a pool-wide mutex stalled every other root behind one slow one.
//
// Concurrency: begin and end must be called with the OWNING pool's mutex held;
// the channel begin returns is waited on after that mutex is released.
type keyedFlight struct {
	inflight map[string]chan struct{}
}

// begin claims key. leader is true when the caller now owns the open and must
// call end when done; otherwise wait closes when the current leader ends, and
// the caller should re-check the pool before trying again.
func (f *keyedFlight) begin(key string) (wait <-chan struct{}, leader bool) {
	if ch, ok := f.inflight[key]; ok {
		return ch, false
	}
	if f.inflight == nil {
		f.inflight = make(map[string]chan struct{})
	}
	f.inflight[key] = make(chan struct{})
	return nil, true
}

// end releases key and wakes its waiters.
func (f *keyedFlight) end(key string) {
	if ch, ok := f.inflight[key]; ok {
		close(ch)
		delete(f.inflight, key)
	}
}
