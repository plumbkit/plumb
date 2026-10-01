package cli

// conn_lane.go — the guarded mutation lane for changes that take a reference
// close() must release (issue #514).

// mutateLive is mutate for a change that takes a reference close() must
// release: a language-server pin, a quality runner, a write budget. It runs fn
// only while the connection is open and reports whether it did. The check
// sits inside the lane, not before it: close() cancels s.ctx and THEN takes
// the lane to release, so an fn that sees an open connection here commits
// before close's release runs and is released by it, and one that runs after
// sees the cancel. A check before the lane leaves a window in which close()
// can release first and the attach then takes a reference nobody releases
// (issue #514).
func (s *connSession) mutateLive(fn func(v *sessionView)) (live bool) {
	if s.beforeLiveMutate != nil {
		s.beforeLiveMutate()
	}
	s.mutate(func(v *sessionView) {
		if s.ctx != nil && s.ctx.Err() != nil {
			return
		}
		live = true
		fn(v)
	})
	return live
}
