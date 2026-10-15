package cli

import "github.com/plumbkit/plumb/internal/daemons"

// argvFromProcArgs is darwin's processArgv after the sysctl: the kern.procargs2
// buffer decoded by the census's own decoder (daemons.ParseProcArgs), so the
// census and `plumb stop`/`restart` read a process's argv identically. It is
// untagged so every platform's test run pins it — in particular that a buffer
// that does not decode is "cannot read", never an argv.
func argvFromProcArgs(buf []byte) ([]string, bool) {
	_, args, err := daemons.ParseProcArgs(buf)
	if err != nil || len(args) == 0 {
		return nil, false
	}
	return args, true
}
