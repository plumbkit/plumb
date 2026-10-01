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

// mutateIf is mutate gated on a predicate evaluated INSIDE the lane, against the
// view the change would be made to, and reports whether fn ran. A check made
// before the lane describes a view another change may since have replaced; this
// one cannot be stale, because nothing else commits between it and fn. A nil ok
// always runs fn. ok must not take the lane.
func (s *connSession) mutateIf(ok func(v *sessionView) bool, fn func(v *sessionView)) (ran bool) {
	s.mutate(func(v *sessionView) {
		if ok != nil && !ok(v) {
			return
		}
		ran = true
		fn(v)
	})
	return ran
}
