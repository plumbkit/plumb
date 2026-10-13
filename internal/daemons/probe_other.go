//go:build !linux && !darwin

package daemons

// Plumb's daemon does not run on these platforms at all — its locks are
// flock(2) and Windows is explicitly unsupported — so there is no process table
// this package could read. Every question is unanswered rather than answered
// "none": a caller that prints a count must be able to see that nothing was
// read, which is the whole point of the census failing closed.
// platformProbe answers nothing on a platform where the daemon cannot run at all.
func platformProbe(Options) probe { return probe{} }

func osProcesses(int) ([]Daemon, []string) {
	return nil, []string{"this platform has no plumb daemon census"}
}

func osLockHolders(string) ([]int, error) {
	return nil, errUnsupported
}

func osOwners(string, []int) ([]int, error) {
	return nil, errUnsupported
}

// errUnsupported is what a probe on this platform reports.
var errUnsupported = errUnsupportedPlatform{}

type errUnsupportedPlatform struct{}

func (errUnsupportedPlatform) Error() string {
	return "the daemon census is not supported on this platform"
}
